package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
)

// Where the host's daemon keeps its data and serves its API, inside the
// host container only.
const (
	dataRoot     = "/var/lib/docker"
	daemonSocket = "/run/docker.sock"
	// stateDir holds the host's own files on the data volume, outside
	// Docker's layout.
	stateDir = dataRoot + "/manade"
)

// shutdownTimeout is dockerd's wait for its containers on SIGTERM; with
// stopWait it fits the compose files' 30-second stop grace.
const shutdownTimeout = 15

// stopWait is how long Stop waits for dockerd after its SIGTERM; a var so a
// test can shorten it.
var stopWait = 25 * time.Second

// daemonSpec is what the daemon's command line is built from.
type daemonSpec struct {
	Kernel  *platform.Kernel
	Storage platform.Driver
	// NoBridgeNF is a namespace without bridge-nf-call-iptables, which
	// before Linux 5.3 only the machine's initial namespace has.
	NoBridgeNF bool
}

// argv answers the daemon's command line, run through the image's dind
// script and never dockerd-entrypoint.sh, whose defaults generate TLS files
// and listen on TCP 2376. An explicit graph driver keeps Docker 29 off its
// containerd image store, which a full volume wedges.
func (s *daemonSpec) argv() []string {
	return []string{
		"dind", "dockerd",
		"--host=unix://" + daemonSocket,
		"--data-root=" + dataRoot,
		"--storage-driver=" + string(s.Storage),
		"--bridge=none",
		"--ip6tables=false",
		fmt.Sprintf("--shutdown-timeout=%d", shutdownTimeout),
	}
}

// env answers the daemon's environment from base, the supervisor's: on the
// iptables backend the legacy binaries first in PATH, since the image's
// default iptables is the nf_tables variant, and the kernel's opt-outs.
func (s *daemonSpec) env(base []string) []string {
	set := map[string]string{}
	path := ""
	for _, kv := range base {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	if s.Kernel.Backend == egress.IPTables {
		set["PATH"] = platform.LegacyDir + ":" + path
	}
	if s.Kernel.RawOff {
		set["DOCKER_INSECURE_NO_IPTABLES_RAW"] = "1"
	}
	if s.NoBridgeNF {
		set["DOCKER_IGNORE_BR_NETFILTER_ERROR"] = "1"
	}
	out := make([]string, 0, len(base)+len(set))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; !ok {
			out = append(out, kv)
		}
	}
	for _, k := range []string{"PATH", "DOCKER_INSECURE_NO_IPTABLES_RAW", "DOCKER_IGNORE_BR_NETFILTER_ERROR"} {
		if v, ok := set[k]; ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// dockerd is the running dockerd.
type dockerd struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// startDaemon starts argv with env, its output to out. ctx's cancellation
// never ends the process: only Stop does, so dockerd always gets its
// SIGTERM.
func startDaemon(ctx context.Context, argv, env []string, out io.Writer) (*dockerd, error) {
	cmd := exec.CommandContext(context.WithoutCancel(ctx), argv[0], argv[1:]...) //nolint:gosec // G204: argv is daemonSpec.argv, which this package builds
	cmd.Env, cmd.Stdout, cmd.Stderr = env, out, out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting dockerd: %w", err)
	}
	d := &dockerd{cmd: cmd, done: make(chan struct{})}
	go func() {
		d.err = cmd.Wait()
		close(d.done)
	}()
	return d, nil
}

// PID is the daemon's process ID.
func (d *dockerd) PID() int { return d.cmd.Process.Pid }

// Done is closed once the daemon has exited.
func (d *dockerd) Done() <-chan struct{} { return d.done }

// Err answers how the daemon exited; call it after Done is closed.
func (d *dockerd) Err() error { return d.err }

// Stop sends the daemon SIGTERM, which stops each runner, and waits for it
// to exit, killing it after stopWait. It answers how the daemon exited, nil
// for an exit on that SIGTERM.
func (d *dockerd) Stop() error {
	select {
	case <-d.done:
		return d.err
	default:
	}
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stopping dockerd: %w", err)
	}
	t := time.NewTimer(stopWait)
	defer t.Stop()
	select {
	case <-d.done:
	case <-t.C:
		_ = d.cmd.Process.Kill()
		<-d.done
		return fmt.Errorf("dockerd did not exit within %s of SIGTERM and was killed", stopWait)
	}
	if exit, ok := errors.AsType[*exec.ExitError](d.err); ok {
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGTERM {
			return nil
		}
	}
	return d.err
}

// daemonPoll is how often waitDaemon asks the daemon.
const daemonPoll = 500 * time.Millisecond

// waitDaemon calls answer until it succeeds, the daemon exits or ctx ends.
func waitDaemon(ctx context.Context, done <-chan struct{}, answer func(ctx context.Context) error) error {
	t := time.NewTicker(daemonPoll)
	defer t.Stop()
	for {
		err := answer(ctx)
		if err == nil {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("dockerd exited before it answered: %w", err)
		case <-ctx.Done():
			return fmt.Errorf("dockerd did not answer: %w", err)
		case <-t.C:
		}
	}
}
