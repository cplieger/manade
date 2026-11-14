package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/jobspec"
)

// fakeDaemon holds a run's containers on network manade.
type fakeDaemon struct {
	ctrs     map[string]*docker.Container
	conflict map[string]bool
	calls    []string
	mu       sync.Mutex
	missing  bool
	addr     byte
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{ctrs: map[string]*docker.Container{}, conflict: map[string]bool{}, addr: 1}
}

func (d *fakeDaemon) Create(_ context.Context, name string, req *docker.CreateRequest) docker.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "create "+name)
	if d.missing {
		d.missing = false
		return docker.Result{Outcome: docker.Failed, Err: fmt.Errorf("%w: %s", docker.ErrImageMissing, req.Image)}
	}
	d.addr++
	c := &docker.Container{ID: "id-" + name}
	c.Config.Labels = maps.Clone(req.Labels)
	c.State.Status = docker.StateCreated
	c.NetworkSettings.Networks = map[string]docker.Endpoint{
		"manade": {IPAddress: fmt.Sprintf("172.18.0.%d", d.addr), Gateway: "172.18.0.1"},
	}
	d.ctrs[c.ID] = c
	return docker.Result{Outcome: docker.Done, ID: c.ID}
}

func (d *fakeDaemon) Start(_ context.Context, id string) docker.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "start "+id)
	d.ctrs[id].State.Status = docker.StateRunning
	return docker.Result{Outcome: docker.Done, ID: id}
}

func (d *fakeDaemon) InspectContainer(_ context.Context, ref string) (docker.Container, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.ctrs[ref]
	if !ok {
		return docker.Container{}, false, nil
	}
	return *c, true, nil
}

func (d *fakeDaemon) ListContainers(context.Context) ([]docker.Summary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []docker.Summary
	for _, c := range d.ctrs {
		out = append(out, docker.Summary{ID: c.ID, State: c.State.Status, Labels: maps.Clone(c.Config.Labels)})
	}
	return out, nil
}

func (d *fakeDaemon) RemoveOwned(_ context.Context, ref string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := ref
	if !strings.HasPrefix(id, "id-") {
		id = "id-" + ref
	}
	if d.conflict[id] {
		delete(d.conflict, id)
		return fmt.Errorf("%w: removal in progress", docker.ErrConflict)
	}
	d.calls = append(d.calls, "remove "+id)
	delete(d.ctrs, id)
	return nil
}

func (d *fakeDaemon) callList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

// fakeSets counts a drop for each block check answered while its target
// is set, as the counting rule before a drop would.
type fakeSets struct {
	target  netip.Addr
	sources []netip.Addr
	sets    []string
	counts  egress.ProbeCounts
	clears  int
	mu      sync.Mutex
	// pass counts the drops as passed instead: a rule set whose drops
	// let the packet through.
	pass bool
}

func (s *fakeSets) SetSources(_ context.Context, addrs []netip.Addr) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = slices.Clone(addrs)
	s.sets = append(s.sets, fmt.Sprint("sources ", addrs))
	return nil
}

func (s *fakeSets) SetTarget(ctx context.Context, a netip.Addr) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = a
	return nil
}

func (s *fakeSets) Counts(context.Context) (egress.ProbeCounts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts, nil
}

func (s *fakeSets) Clear(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
	s.target, s.sources = netip.Addr{}, nil
	return nil
}

// dropped is a block check's packet meeting the rules.
func (s *fakeSets) dropped() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.target.IsValid() || len(s.sources) == 0 {
		return
	}
	if s.pass {
		s.counts.Passed++
	} else {
		s.counts.Dropped++
	}
}

type fakeImage struct {
	id    string
	loads int
	mu    sync.Mutex
}

func (i *fakeImage) ProbeImage() string { i.mu.Lock(); defer i.mu.Unlock(); return i.id }

func (i *fakeImage) LoadProbe(context.Context) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.loads++
	i.id = fmt.Sprintf("sha256:probe-%d", i.loads)
	return i.id, nil
}

// probeTest is a Runner on fakes whose checks answer as a host with every
// rule in place would.
type probeTest struct {
	r       *Runner
	d       *fakeDaemon
	sets    *fakeSets
	image   *fakeImage
	hold    chan struct{}
	done    chan struct{}
	attach  func(ctx context.Context) error
	host    *config.Host
	noReply string
	delay   time.Duration
}

func newProbeTest(t *testing.T) *probeTest {
	t.Helper()
	pt := &probeTest{d: newFakeDaemon(), sets: &fakeSets{}, image: &fakeImage{id: "sha256:probe-0"}, done: make(chan struct{})}
	t.Cleanup(func() { close(pt.done) })
	pt.host = &config.Host{Name: "nas", DNS: []netip.Addr{netip.MustParseAddr("1.1.1.1")}, Egress: config.Egress{Internet: true}}
	cfg := &config.Config{File: "/config/config.yaml", Self: &config.Self{Name: "nas", TLSDir: "/tls"}}
	spec := jobspec.NewHost(cfg, pt.host, &jobspec.Platform{Network: "manade"})
	pt.r = NewRunner(&Config{
		Daemon: pt.d, Attach: pt.attachFn, Sets: pt.sets, Image: pt.image,
		Spec: func() *jobspec.Host { return spec },
		Host: func() Host {
			return Host{
				Input:   egress.ProbeInput{Host: pt.host, OutwardGateway: netip.MustParseAddr("192.168.1.1"), APIPort: 7464},
				Sources: []netip.Addr{netip.MustParseAddr("192.168.1.20")},
			}
		},
		Log: slog.New(slog.DiscardHandler), Network: "manade", Started: time.Now(),
	})
	oldDo, oldOut := doCheck, outputCheck
	t.Cleanup(func() { doCheck, outputCheck = oldDo, oldOut })
	doCheck = pt.answer
	outputCheck = func(ctx context.Context, c *egress.Check, _ []netip.Addr) egress.Outcome {
		return pt.answer(ctx, c.Kind, c.Addr, c.Timeout)
	}
	return pt
}

func (pt *probeTest) answer(_ context.Context, kind egress.Kind, _ string, _ time.Duration) egress.Outcome {
	time.Sleep(pt.delay)
	switch kind {
	case egress.KindBlock:
		pt.sets.dropped()
		return egress.OutcomeTimeout
	case egress.KindResolve:
		return egress.OutcomeResolved
	case egress.KindNoIPv6:
		return egress.OutcomeNoRoute
	}
	return egress.OutcomeConnected
}

// attachFn serves the probe container's side over a pipe with the real
// Serve, the way the attached container answers.
func (pt *probeTest) attachFn(ctx context.Context, _ string) (Stream, error) {
	if pt.attach != nil {
		if err := pt.attach(ctx); err != nil {
			return nil, err
		}
	}
	host, ctr := net.Pipe()
	go func() {
		defer ctr.Close()
		if pt.hold != nil {
			<-pt.hold
		}
		if pt.noReply != "" {
			_ = Serve(context.Background(), &stopAt{r: ctr, at: pt.noReply, done: pt.done}, ctr)
			return
		}
		_ = Serve(context.Background(), ctr, ctr)
	}()
	return host, nil
}

// stopAt reads lines until one holds at, then reads no more, as a probe
// container that hangs on that check.
type stopAt struct {
	r    net.Conn
	done <-chan struct{}
	at   string
}

func (s *stopAt) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if strings.Contains(string(p[:n]), s.at) {
		<-s.done
		return 0, io.EOF
	}
	return n, err
}

func TestARunProves(t *testing.T) {
	pt := newProbeTest(t)
	rep, err := pt.r.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Run("every required check passes", func(t *testing.T) {
		if !rep.Proven || rep.Unproven != "" || len(rep.Checks) == 0 {
			t.Fatalf("report = %+v", rep)
		}
		for _, c := range rep.Checks {
			if !c.Pass {
				t.Errorf("check %+v failed", c)
			}
		}
	})
	t.Run("the job leg runs in the probe container, then the output leg from the host", func(t *testing.T) {
		legs := slices.Compact(slices.Collect(func(yield func(string) bool) {
			for _, c := range rep.Checks {
				if !yield(c.Leg) {
					return
				}
			}
		}))
		if !slices.Equal(legs, []string{"job", "output"}) {
			t.Errorf("legs = %v", legs)
		}
		pt.sets.mu.Lock()
		defer pt.sets.mu.Unlock()
		if len(pt.sets.sets) != 2 || pt.sets.sets[1] != "sources [192.168.1.20]" {
			t.Errorf("sources = %q, want the probe's address then the host's own", pt.sets.sets)
		}
	})
	t.Run("the listener stands in for another job and the gateway is the probe's", func(t *testing.T) {
		var targets []string
		for _, c := range rep.Checks {
			targets = append(targets, c.Target)
		}
		if !slices.Contains(targets, "172.18.0.2:8471") || !slices.Contains(targets, "172.18.0.1:7464") {
			t.Errorf("targets = %q", targets)
		}
	})
	t.Run("the sets are emptied and both containers removed", func(t *testing.T) {
		calls := pt.d.callList()
		if n := len(slices.DeleteFunc(slices.Clone(calls), func(c string) bool { return !strings.HasPrefix(c, "remove ") })); n != 2 {
			t.Errorf("calls = %q, want two removals", calls)
		}
		if pt.sets.clears != 1 {
			t.Errorf("%d clears", pt.sets.clears)
		}
	})
}

func TestBlockChecksNeedTheirCounter(t *testing.T) {
	t.Run("a timeout no drop counted is unproven", func(t *testing.T) {
		pt := newProbeTest(t)
		pt.r.cfg.Sets = &noCount{pt.sets}
		rep, err := pt.r.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		c := rep.Checks[0]
		if rep.Proven || !c.Unproven || c.Pass {
			t.Errorf("report proven %v, first check %+v", rep.Proven, c)
		}
	})
	t.Run("a packet past every drop fails", func(t *testing.T) {
		pt := newProbeTest(t)
		pt.sets.pass = true
		rep, _ := pt.r.Run(t.Context())
		if rep.Proven || !strings.Contains(rep.Checks[0].Reason, "passed every drop") {
			t.Errorf("report = %+v", rep.Checks[0])
		}
	})
}

// noCount is a rule set without its counting rules.
type noCount struct{ *fakeSets }

func (*noCount) Counts(context.Context) (egress.ProbeCounts, error) {
	return egress.ProbeCounts{}, nil
}

func TestOneRunAtATime(t *testing.T) {
	pt := newProbeTest(t)
	pt.hold = make(chan struct{})
	first := make(chan error, 1)
	go func() { _, err := pt.r.Run(t.Context()); first <- err }()
	waitFor(t, "the first run's probe", func() bool { return len(pt.d.callList()) >= 4 })
	t.Run("a cancelled waiting request starts no run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := pt.r.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("Run = %v", err)
		}
	})
	second := make(chan error, 1)
	go func() { _, err := pt.r.Run(t.Context()); second <- err }()
	time.Sleep(50 * time.Millisecond)
	t.Run("an overlapping request waits for the run in progress", func(t *testing.T) {
		if n := len(pt.d.callList()); n != 4 {
			t.Errorf("%d calls during the first run, want its 4", n)
		}
	})
	close(pt.hold)
	if err := errors.Join(<-first, <-second); err != nil {
		t.Fatal(err)
	}
	t.Run("then gets a whole run of its own", func(t *testing.T) {
		var runs []string
		for _, c := range pt.d.callList() {
			if strings.HasPrefix(c, "create ") {
				runs = append(runs, c[strings.LastIndex(c, "-")+1:])
			}
		}
		if len(runs) != 4 || runs[0] != runs[1] || runs[2] != runs[3] || runs[0] == runs[2] {
			t.Errorf("created runs = %q, want two whole runs one after the other", runs)
		}
	})
}

func TestARunOutlivesItsClient(t *testing.T) {
	pt := newProbeTest(t)
	pt.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var rep struct {
		err    error
		proven bool
	}
	go func() {
		r, err := pt.r.Run(ctx)
		rep.proven, rep.err = r.Proven, err
		close(done)
	}()
	waitFor(t, "the run's probe", func() bool { return len(pt.d.callList()) >= 4 })
	cancel()
	close(pt.hold)
	<-done
	if rep.err != nil || !rep.proven {
		t.Errorf("Run = %v, proven %v, want a whole run", rep.err, rep.proven)
	}
	if pt.sets.clears != 1 {
		t.Errorf("%d clears, want the sets emptied", pt.sets.clears)
	}
}

func TestUnprovenRuns(t *testing.T) {
	t.Run("a missing probe image is loaded again at once", func(t *testing.T) {
		pt := newProbeTest(t)
		pt.d.missing = true
		rep, err := pt.r.Run(t.Context())
		if err != nil || !rep.Proven || pt.image.loads != 1 {
			t.Errorf("Run = %v, proven %v, %d loads", err, rep.Proven, pt.image.loads)
		}
		pt.d.mu.Lock()
		defer pt.d.mu.Unlock()
		for _, c := range pt.d.ctrs {
			t.Errorf("container %s left", c.ID)
		}
	})
	t.Run("a stalled attach names the call and leaves the containers", func(t *testing.T) {
		pt := newProbeTest(t)
		old := callTimeout
		callTimeout = 20 * time.Millisecond
		t.Cleanup(func() { callTimeout = old })
		pt.attach = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		rep, err := pt.r.Run(t.Context())
		if err != nil || rep.Proven || !strings.Contains(rep.Unproven, "probe container's attach") {
			t.Errorf("Run = %v, %+v", err, rep)
		}
		if calls := pt.d.callList(); slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "remove") }) {
			t.Errorf("calls = %q, want the containers left", calls)
		}
	})
	t.Run("an exchange with no outcome line names the check", func(t *testing.T) {
		pt := newProbeTest(t)
		pt.noReply = egress.GitHubAPI
		start := time.Now()
		rep, err := pt.r.Run(t.Context())
		if err != nil || rep.Proven || !strings.Contains(rep.Unproven, `check "GitHub's API": no outcome line within 5s`) {
			t.Errorf("Run = %v, %+v", err, rep)
		}
		if took := time.Since(start); took < 5*time.Second {
			t.Errorf("the run ended after %s, before the exchange's bound", took)
		}
		if n := len(slices.DeleteFunc(pt.d.callList(), func(c string) bool { return !strings.HasPrefix(c, "remove") })); n != 2 {
			t.Errorf("%d removals, want both containers removed", n)
		}
	})
	t.Run("an ipv6 host with no /64 is unproven", func(t *testing.T) {
		pt := newProbeTest(t)
		host := pt.r.cfg.Host
		pt.r.cfg.Host = func() Host { h := host(); h.No64 = "the echo failed"; return h }
		rep, _ := pt.r.Run(t.Context())
		if rep.Proven || !strings.Contains(rep.Unproven, "the echo failed") {
			t.Errorf("report = %+v", rep)
		}
	})
}

func TestExchangesSumPastTheCallTimeout(t *testing.T) {
	pt := newProbeTest(t)
	old := callTimeout
	callTimeout = 20 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })
	pt.delay = 5 * time.Millisecond
	start := time.Now()
	rep, err := pt.r.Run(t.Context())
	if took := time.Since(start); took <= callTimeout {
		t.Fatalf("the run took %s, not past the call timeout", took)
	}
	if err != nil || !rep.Proven {
		t.Errorf("Run = %v, %+v, want a verdict", err, rep)
	}
}

func TestLeftoverProbeContainers(t *testing.T) {
	pt := newProbeTest(t)
	old := callTimeout
	callTimeout = 50 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })
	leftover := func(run string) string {
		id := "id-manade-probe-" + run
		c := &docker.Container{ID: id}
		c.Config.Labels = map[string]string{jobspec.LabelProbe: run}
		pt.d.mu.Lock()
		pt.d.ctrs[id] = c
		pt.d.mu.Unlock()
		return id
	}
	held := func(id string) bool { _, ok, _ := pt.d.InspectContainer(t.Context(), id); return ok }
	young := leftover("previous")
	t.Run("a previous supervisor's run counts as ended at this one's start", func(t *testing.T) {
		if _, err := pt.r.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !held(young) {
			t.Error("removed before a call timeout passed since this supervisor's start")
		}
	})
	time.Sleep(callTimeout)
	pt.d.conflict[young] = true
	t.Run("a removal answered 409 is tried again at the next run", func(t *testing.T) {
		if _, err := pt.r.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !held(young) {
			t.Error("a 409 removed it")
		}
		if _, err := pt.r.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if held(young) {
			t.Error("left after the retry")
		}
	})
}

func TestEnded(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	r := NewRunner(&Config{Started: start})
	r.runs["done"] = start.Add(time.Minute)
	r.cur = "now"
	for run, want := range map[string]struct {
		at      time.Time
		running bool
	}{"done": {start.Add(time.Minute), false}, "now": {time.Time{}, true}, "never": {start, false}} {
		if at, running := r.Ended(run); !at.Equal(want.at) || running != want.running {
			t.Errorf("Ended(%s) = %v, %v", run, at, running)
		}
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
