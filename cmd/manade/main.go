// Command manade serves a pool of self-hosted GitHub Actions runners on one
// Docker daemon. With no argument it serves; `manade health` is the image's
// healthcheck, which reads the health marker the server keeps.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
	"github.com/cplieger/manade/internal/r5/jobspec"
	"github.com/cplieger/manade/internal/r5/obs"
	"github.com/cplieger/manade/internal/r5/pool"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

// stableRe matches a stable release tag. Only such a build runs the shipped
// runner images' stable tags; every other build, an unstamped one included,
// runs their dev tags, which are built from the newest source.
var stableRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// dockerTimeout bounds the boot's version, info and socket checks.
const dockerTimeout = 30 * time.Second

func main() { os.Exit(run(os.Args[1:], os.Stderr)) }

// run dispatches one command and returns the exit code: 0 after a clean
// shutdown, 1 when the pool cannot start or stops on a failure, 2 on a usage
// error. The health command exits inside RunProbe.
func run(args []string, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		return serve(stderr)
	case len(args) == 1 && args[0] == "health":
		health.RunProbe(markerPath, health.WithMaxAge(markerLease.Duration()))
		return 0
	default:
		_, _ = fmt.Fprintln(stderr, "usage: manade [health]")
		return 2
	}
}

func serve(stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	level := slogx.Setup(slogx.Options{Output: stderr})
	if err := start(ctx, slog.Default(), level); err != nil {
		slog.Error("manade stopped on a failure", "error", err)
		return 1
	}
	return 0
}

// start loads the config, binds the metrics listener before any other work,
// checks the daemon, wires the pool and runs it until ctx ends.
func start(ctx context.Context, log *slog.Logger, level *slog.LevelVar) error {
	cfg, err := config.Load(ctx)
	if err != nil {
		return err
	}
	level.Set(cfg.LogLevel)
	if cfg.LogLevelInvalid {
		log.Warn("LOG_LEVEL is not a known level, so manade logs at info")
	}
	dev := !stableRe.MatchString(version)
	log.Info("manade starting", "version", version, "dev_images", dev)
	logProfiles(log, cfg.Profiles)
	var ln net.Listener
	if cfg.MetricsListenAddr != "" {
		var lc net.ListenConfig
		if ln, err = lc.Listen(ctx, "tcp", cfg.MetricsListenAddr); err != nil {
			return fmt.Errorf("METRICS_LISTEN_ADDR: %w", err)
		}
		defer ln.Close()
	}
	m, err := obs.New(cfg, log)
	if err != nil {
		return err
	}
	diag, err := openDiag(cfg.DiagDir)
	if err != nil {
		return err
	}
	if diag != nil {
		defer diag.Close()
	}
	dk, host, err := dialDocker(ctx, cfg, log)
	if err != nil {
		return err
	}
	tr, err := github.NewTransport(cfg.GitHub.Proxy)
	if err != nil {
		return err
	}
	rest, err := github.NewREST(cfg, tr, m, log)
	if err != nil {
		return err
	}
	p := pool.New(cfg, &pool.Deps{
		GitHub: github.NewScaleSets(cfg, tr, rest, version), REST: rest, Sweep: github.NewSweep(cfg, rest),
		Docker: dk, Host: host, Metrics: m, Log: log, Diag: diag, Dev: dev,
	})
	m.SetSource(p.Snapshot)
	return runPool(ctx, p, ln, m, log)
}

// logProfiles warns once per elevated profile, naming its grants, and names
// in one line every profile without a disk limit.
func logProfiles(log *slog.Logger, profiles []config.Profile) {
	var noDisk []string
	for i := range profiles {
		p := &profiles[i]
		if g := p.Grants(); len(g) > 0 {
			log.Warn("A profile grants elevated access, so its jobs reach past the default isolation",
				"profile", p.Name, "grants", strings.Join(g, ","),
				"sysbox_root_holds_all_capabilities", p.RootHoldsAllCapabilities())
		}
		if p.Disk == 0 {
			noDisk = append(noDisk, p.Name)
		}
	}
	if len(noDisk) > 0 {
		log.Info("Profiles without a disk limit let a job fill the filesystem that holds its writable layer",
			"profiles", strings.Join(noDisk, ","))
	}
}

// openDiag opens DIAG_DIR, nil when it is unset, and proves the pool can
// write there, so a read-only mount fails startup instead of every runner's
// diagnostics.
func openDiag(dir string) (*os.Root, error) {
	if dir == "" {
		return nil, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("DIAG_DIR: %w", err)
	}
	const probe = ".manade-write-check"
	f, err := root.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err == nil {
		err = errors.Join(f.Close(), root.Remove(probe))
	}
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("DIAG_DIR: the pool cannot write there: %w", err)
	}
	return root, nil
}

// dialDocker connects to the daemon and refuses one the pool cannot serve
// on: an API below the floor, a kernel that would drop the hard limits, or
// a daemon the config's runtimes, disk limits or bind sources do not fit.
// It reads the socket's group only when a profile is granted the socket.
func dialDocker(ctx context.Context, cfg *config.Config, log *slog.Logger) (*docker.Client, *jobspec.Host, error) {
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	dk, err := docker.Dial(ctx, cfg.DockerSocket, cfg.PoolName)
	if err != nil {
		return nil, nil, fmt.Errorf("DOCKER_SOCKET: %w", err)
	}
	info, err := dk.Info(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("DOCKER_SOCKET: %w", err)
	}
	err = info.CheckLimits()
	if err == nil {
		err = cfg.CheckDaemon(&config.Daemon{
			DataRoot: info.DataRoot, ContainerdAddress: info.ContainerdAddress,
			Driver: info.Driver, DriverStatus: info.DriverStatus, Runtimes: info.Runtimes,
		})
	}
	if err != nil {
		return nil, nil, err
	}
	// Below API 1.49 /info does not report the backend.
	log.Info("Docker answered; the firewall rules in docs/firewall.md for this backend keep jobs off the host and the LAN, and an unknown one needs both the iptables and the nftables set",
		"api_version", dk.APIVersion(), "firewall_backend", cmp.Or(info.FirewallBackend, "unknown"))
	var gid uint32
	for i := range cfg.Profiles {
		if cfg.Profiles[i].Docker == config.DockerHostSocket {
			if gid, err = dk.SocketGID(); err != nil {
				return nil, nil, fmt.Errorf("DOCKER_SOCKET: %w", err)
			}
			break
		}
	}
	return dk, jobspec.NewHost(cfg, gid), nil
}

// server is the pool as runPool drives it.
type server interface {
	Run(ctx context.Context) error
	Health() pool.Health
	Healthy() bool
}

var _ server = (*pool.Pool)(nil)

// runPool runs p with its health marker, and its listener when ln is set,
// until ctx ends or the listener fails. It waits for the pool's shutdown.
func runPool(ctx context.Context, p server, ln net.Listener, m *obs.Metrics, log *slog.Logger) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	marker := health.NewMarker(markerPath, health.WithLogger(log))
	defer marker.Cleanup()
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		keepMarker(ctx, health.NewLatch(marker), p)
	}()
	served := make(chan error, 1)
	// The listener outlives ctx, so /metrics and /healthz answer through the
	// pool's shutdown.
	srvCtx, stopSrv := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSrv()
	if ln == nil {
		served <- nil
	} else {
		go func() {
			err := serveHTTP(srvCtx, ln, m, healthChecks(p.Health, func() bool { return ctx.Err() == nil }))
			if err != nil {
				err = fmt.Errorf("METRICS_LISTEN_ADDR: %w", err)
				cancel(err)
			}
			served <- err
		}()
	}
	err := p.Run(ctx)
	// A failed boot returns with ctx live, and the marker loop ends only on
	// ctx.
	cancel(nil)
	stopSrv()
	<-kept
	srvErr := <-served
	return cmp.Or(err, ignoreCanceled(context.Cause(ctx)), srvErr)
}

// ignoreCanceled answers nil for a stop by signal, whose cause matches
// context.Canceled, and for runPool's own cancel after the pool returned.
func ignoreCanceled(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
