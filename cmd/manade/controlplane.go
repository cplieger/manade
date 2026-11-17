package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/history"
	"github.com/cplieger/manade/internal/hostlink"
	"github.com/cplieger/manade/internal/obs"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

const controlPlaneUsage = "usage: manade control-plane"

// runControlPlane runs `manade control-plane`.
func runControlPlane(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintln(stderr, controlPlaneUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return serveControlPlane(ctx, stderr)
}

// Test seams: drainHook runs once /healthz reads draining, before the
// pool's shutdown is waited on.
var (
	hostsSocket = hostlink.SocketPath
	listenHosts = hostlink.ListenSocket
	runOps      = httpserver.RunWithOps
	drainHook   = func() {}
)

// firstBoot is the settings a first boot creates, the local host and the
// default profile at their defaults, and the only settings the control
// plane serves until the settings store holds a document (D21).
const firstBoot = `{"hosts": {"default": {}}, "profiles": {"default": {}}}`

// serveControlPlane runs the control plane until ctx ends. The ops
// listener binds first and stops last, so /healthz answers starting while
// the store opens and draining until the pool has shut down.
func serveControlPlane(ctx context.Context, stderr io.Writer) int {
	level := slogx.Setup(slogx.Options{Output: stderr})
	log := slog.Default()
	cfg, err := config.LoadControlPlane()
	if err != nil {
		log.Error("The control plane's environment does not validate", "error", err)
		return 1
	}
	level.Set(cfg.LogLevel)
	if cfg.LogLevelInvalid {
		log.Warn("LOG_LEVEL is not a known level, so the control plane logs at info")
	}
	m, err := obs.New(version, log)
	if err != nil {
		log.Error("The control plane's metrics did not register", "error", err)
		return 1
	}
	hc := &cpHealth{}
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	defer stopOps()
	ops, err := httpserver.ListenOps(opsCtx, httpserver.OpsConfig{
		Metrics: m.Handler(), Healthz: hc.handler(), Logger: log,
		Setting: "OPS_LISTEN_ADDR", Addr: cfg.OpsAddr, Hosts: cfg.AllowedHosts,
	})
	if err != nil {
		log.Error("The control plane could not open its ops listener", "error", err)
		return 1
	}
	runCtx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	opsDone := make(chan error, 1)
	go func() {
		opsDone <- runOps(opsCtx, ops)
		stopRun()
	}()
	code := servePool(runCtx, &cpRun{cfg: cfg, metrics: m, health: hc, ops: ops.Addr(), log: log})
	stopOps()
	if err := <-opsDone; err != nil {
		log.Error("The control plane's ops listener stopped on a failure", "error", err)
		code = 1
	}
	return code
}

// cpRun is what servePool runs with.
type cpRun struct {
	cfg     *config.ControlPlane
	metrics *obs.Metrics
	health  *cpHealth
	ops     net.Addr
	log     *slog.Logger
}

// servePool holds the store's lock and runs the pool, its host listener,
// its history and its housekeeping over the open store until ctx ends.
func servePool(ctx context.Context, r *cpRun) int {
	path := filepath.Join(r.cfg.DataDir, store.File)
	st, err := store.New(path, store.Options{Log: r.log, Build: version})
	if err != nil {
		r.log.Error("The control plane could not take its store's lock", "file", path, "error", err)
		return 1
	}
	code := serveOpen(ctx, r, st, path)
	if err := st.Close(); err != nil {
		r.log.Error("The control plane's store did not close cleanly", "file", path, "error", err)
		return 1
	}
	return code
}

// serveOpen opens st and serves it. A file that cannot open holds the
// control plane in its outage until ctx ends: /healthz names the failure's
// class, the log its error, and the file is left as it was.
func serveOpen(ctx context.Context, r *cpRun, st *store.Store, path string) int {
	err := st.Open()
	if err == nil {
		return serveStore(ctx, r, st)
	}
	// New holds the lock, so no open fails with ErrLocked, the one error
	// OpenFailure does not class.
	class, _ := bolttable.OpenFailure(err)
	r.log.Error("The control plane's store cannot open, so the control plane serves nothing until the file is repaired or replaced and it restarts",
		"file", path, "class", class, "error", err, "ops", r.ops.String())
	r.health.outage(httpserver.OutageClass(class))
	<-ctx.Done()
	return 0
}

// serveStore runs the pool and its loops over st until ctx ends.
func serveStore(ctx context.Context, r *cpRun, st *store.Store) int {
	first, err := config.Parse([]byte(firstBoot), nil, nil)
	if err != nil {
		r.log.Error("The first run's settings do not validate", "error", err)
		return 1
	}
	gh, err := githubSide(&r.cfg.GitHub, r.metrics, r.log)
	if err != nil {
		r.log.Error("The control plane could not build its GitHub client", "error", err)
		return 1
	}
	ln, err := listenHosts(hostsSocket)
	if err != nil {
		r.log.Error("The control plane could not listen for its hosts", "socket", hostsSocket, "error", err)
		return 1
	}
	srv := hostlink.NewServer(r.log)
	rec := history.NewRecorder(st, r.log)
	reg := pool.NewRegistry(&pool.Settings{Config: first, GitHub: gh}, &pool.Deps{
		Links: hostLinks{srv}, Metrics: r.metrics, Runs: rec, Hosts: rec, Settle: settleLog{r.log},
		Records: scaleSetRecords{st}, Routes: routeRecords{st}, Log: r.log,
	})
	keeper := history.NewHousekeeper(st, func() *config.Config { return first }, r.log)
	r.metrics.Ready(&obs.Sources{Pool: reg.Snapshot, Store: st, Housekeeping: keeper.Due})
	r.health.ready(&cpParts{store: st, hosts: health.SignalFunc(srv.Accepting), pool: reg.Health, recorder: rec, housekeeper: keeper})

	// The pool's shutdown removes idle runners over the host links and
	// records their exits, so the host listener and the recorder stop only
	// once it has returned.
	poolCtx, stopPool := context.WithCancel(ctx)
	defer stopPool()
	after, stopAfter := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAfter()
	var wg sync.WaitGroup
	var hostsErr error
	wg.Go(func() {
		hostsErr = srv.Serve(after, ln)
		stopPool()
	})
	wg.Go(func() { rec.Run(after) })
	wg.Go(func() { keeper.Run(after) })
	poolDone := make(chan struct{})
	go func() {
		reg.Run(poolCtx)
		close(poolDone)
	}()
	r.log.Info("The control plane is running", "version", version, "ops", r.ops.String(), "hosts_socket", hostsSocket,
		"github", gh != nil)
	<-poolCtx.Done()
	r.health.BeginDrain()
	drainHook()
	<-poolDone
	stopAfter()
	wg.Wait()
	if hostsErr != nil {
		r.log.Error("The control plane's hosts listener stopped on a failure", "error", hostsErr)
		return 1
	}
	return 0
}
