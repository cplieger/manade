package host

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/host/probe"
	"github.com/cplieger/manade/internal/jobspec"
)

// listHeldDaemon is a host daemon whose container list waits for release,
// which holds a probe run in flight; it creates nothing.
type listHeldDaemon struct {
	listing, release chan struct{}
}

func (d *listHeldDaemon) ListContainers(context.Context) ([]docker.Summary, error) {
	close(d.listing)
	<-d.release
	return nil, errors.New("listing refused")
}

func (*listHeldDaemon) Create(context.Context, string, *docker.CreateRequest) docker.Result {
	return docker.Result{Outcome: docker.Failed, Err: errors.New("create refused")}
}

func (*listHeldDaemon) Start(context.Context, string) docker.Result {
	return docker.Result{Outcome: docker.Failed, Err: errors.New("start refused")}
}

func (*listHeldDaemon) InspectContainer(context.Context, string) (docker.Container, bool, error) {
	return docker.Container{}, false, nil
}

func (*listHeldDaemon) RemoveOwned(context.Context, string) error { return nil }

// Status reports a probe run in flight with its deadline, one run of the
// plan after its start, and none once the run has ended.
func TestStatusReportsARunningProbe(t *testing.T) {
	s, _, _ := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	d := &listHeldDaemon{listing: make(chan struct{}), release: make(chan struct{})}
	s.prober = probe.NewRunner(&probe.Config{
		Daemon: d, Image: s.images, Sets: &probeSets{rules: s.net.rules, guard: s.net.rulesGuard},
		Spec: func() *jobspec.Host { return s.served.Load().Spec }, Host: s.probeHost,
		Log: s.log, Network: jobNetwork, Started: time.Now(),
	})
	before := time.Now()
	ran := make(chan error, 1)
	go func() {
		_, err := apiOf(s).Probe(t.Context(), s.prober.Deadline())
		ran <- err
	}()
	<-d.listing
	st := statusOf(t, s)
	run := st.ProbeDeadline / 2
	if got := st.ProbeRunningUntil; got.Before(before.Add(run)) || got.After(time.Now().Add(run)) {
		t.Errorf("during the run, probe_running_until = %v, want one run (%s) after its start", got, run)
	}
	close(d.release)
	if err := <-ran; err != nil {
		t.Fatal(err)
	}
	if got := statusOf(t, s).ProbeRunningUntil; !got.IsZero() {
		t.Errorf("after the run, probe_running_until = %v, want zero", got)
	}
}
