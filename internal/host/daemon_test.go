package host

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
)

func TestDaemonArgv(t *testing.T) {
	s := &daemonSpec{Kernel: iptKernel(), Storage: platform.Btrfs}
	got := s.argv()
	want := []string{
		"dind", "dockerd", "--host=unix:///run/docker.sock", "--data-root=/var/lib/docker", "--storage-driver=btrfs",
		"--bridge=none", "--ip6tables=false", "--shutdown-timeout=15",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Argv = %q, want %q", got, want)
	}
}

func TestDaemonEnv(t *testing.T) {
	base := []string{"HOME=/root", "PATH=/usr/local/bin:/usr/bin", "DOCKER_INSECURE_NO_IPTABLES_RAW=0"}
	t.Run("the floor", func(t *testing.T) {
		got := (&daemonSpec{Kernel: iptKernel(), NoBridgeNF: true}).env(base)
		want := []string{
			"HOME=/root", "PATH=" + platform.LegacyDir + ":/usr/local/bin:/usr/bin",
			"DOCKER_INSECURE_NO_IPTABLES_RAW=1", "DOCKER_IGNORE_BR_NETFILTER_ERROR=1",
		}
		if !slices.Equal(got, want) {
			t.Errorf("Env = %q, want %q", got, want)
		}
	})
	t.Run("a new kernel leaves the environment alone", func(t *testing.T) {
		got := (&daemonSpec{Kernel: &platform.Kernel{Backend: egress.NFTables}}).env(base[:2])
		if !slices.Equal(got, base[:2]) {
			t.Errorf("Env = %q, want %q", got, base[:2])
		}
	})
}

func TestDaemonStop(t *testing.T) {
	t.Run("an exit on SIGTERM is no error", func(t *testing.T) {
		d, err := startDaemon(t.Context(), []string{"sleep", "60"}, nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Stop(); err != nil {
			t.Errorf("Stop = %v", err)
		}
		if err := d.Stop(); err == nil {
			t.Error("a second Stop answered nil, want the first exit's status")
		}
	})
	t.Run("a daemon that ignores SIGTERM is killed", func(t *testing.T) {
		old := stopWait
		stopWait = 100 * time.Millisecond
		t.Cleanup(func() { stopWait = old })
		d, err := startDaemon(t.Context(), []string{"sh", "-c", `trap "" TERM; echo ready; while :; do sleep 0.05; done`}, nil, &readySignal{ch: make(chan struct{})})
		if err != nil {
			t.Fatal(err)
		}
		<-d.cmd.Stdout.(*readySignal).ch
		if err := d.Stop(); err == nil || !strings.Contains(err.Error(), "killed") {
			t.Errorf("Stop = %v, want it killed", err)
		}
	})
	t.Run("a failing exit on SIGTERM is answered", func(t *testing.T) {
		d, err := startDaemon(t.Context(), []string{"sh", "-c", `trap "exit 3" TERM; echo ready; while :; do sleep 0.05; done`}, nil, &readySignal{ch: make(chan struct{})})
		if err != nil {
			t.Fatal(err)
		}
		<-d.cmd.Stdout.(*readySignal).ch
		if err := d.Stop(); err == nil || !strings.Contains(err.Error(), "exit status 3") {
			t.Errorf("Stop = %v, want exit status 3", err)
		}
	})
	t.Run("an exit of its own is answered", func(t *testing.T) {
		d, err := startDaemon(t.Context(), []string{"sh", "-c", "exit 4"}, nil, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		<-d.Done()
		if err := d.Stop(); err == nil {
			t.Error("Stop = nil for a daemon that exited 4")
		}
	})
}

// readySignal closes ch on the first write, once the child's trap is set.
type readySignal struct {
	ch   chan struct{}
	done bool
}

func (r *readySignal) Write(p []byte) (int, error) {
	if !r.done {
		r.done = true
		close(r.ch)
	}
	return len(p), nil
}

func TestWaitDaemon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		err := waitDaemon(t.Context(), make(chan struct{}), func(context.Context) error {
			calls++
			if calls < 3 {
				return errors.New("connection refused")
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Errorf("WaitDaemon = %v after %d calls, want nil after 3", err, calls)
		}
	})
	t.Run("the daemon exits first", func(t *testing.T) {
		done := make(chan struct{})
		close(done)
		err := waitDaemon(t.Context(), done, func(context.Context) error { return errors.New("connection refused") })
		if err == nil || !strings.Contains(err.Error(), "exited") {
			t.Errorf("WaitDaemon = %v", err)
		}
	})
}
