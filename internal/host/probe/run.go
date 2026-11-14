package probe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// Daemon is the host daemon's calls a probe run makes.
type Daemon interface {
	Create(ctx context.Context, name string, req *docker.CreateRequest) docker.Result
	Start(ctx context.Context, id string) docker.Result
	InspectContainer(ctx context.Context, ref string) (docker.Container, bool, error)
	ListContainers(ctx context.Context) ([]docker.Summary, error)
	RemoveOwned(ctx context.Context, ref string) error
}

var _ Daemon = (*docker.Client)(nil)

// Stream is an attached probe container's standard input and output.
type Stream interface {
	deadlineRW
	Close() error
}

// Sets are the rule set's probe sets and counters.
type Sets interface {
	// SetSources makes addrs the probe's sources, alone.
	SetSources(ctx context.Context, addrs []netip.Addr) error
	// SetTarget makes a the check's target, alone; the zero Addr empties
	// the target sets.
	SetTarget(ctx context.Context, a netip.Addr) error
	Counts(ctx context.Context) (egress.ProbeCounts, error)
	// Clear empties every probe set.
	Clear(ctx context.Context) error
}

// Image is the probe image the host loads from its own binary.
type Image interface {
	ProbeImage() string
	LoadProbe(ctx context.Context) (string, error)
}

// Host is what a run reads of the host at its start.
type Host struct {
	// No64 says why an ipv6 host with internet has no learned /64, which
	// fails its proof; "" otherwise.
	No64 string
	// Sources are the host's own addresses, the output leg's sources.
	Sources []netip.Addr
	// Input is the plan's input without the run's own addresses: the job
	// gateways and the listener's, which the run reads from its containers.
	Input egress.ProbeInput
}

// Config is what a Runner is built from.
type Config struct {
	// Started is the supervisor's start: a run it never saw ended then.
	Started time.Time
	Daemon  Daemon
	Sets    Sets
	Image   Image
	// Attach answers within its own bound: docker.Client.Attach owns it.
	Attach func(ctx context.Context, id string) (Stream, error)
	Spec   func() *jobspec.Host
	Host   func() Host
	Log    *slog.Logger
	// Network is the job network the containers join.
	Network string
}

// A run's two containers, by their role.
const (
	roleProbe  = "probe"
	roleListen = "listen"
)

// probeCommand runs the probe binary's two sides.
var (
	probeCommand    = []string{"/manade", roleProbe}
	listenerCommand = []string{"/manade", roleProbe, roleListen}
)

// callTimeout is docker.CallTimeout, a test seam for the leftover age and
// the bound of a run's cleanup.
var callTimeout = docker.CallTimeout

// Runner runs one probe at a time. Its methods are safe for concurrent use.
type Runner struct {
	cfg  Config
	slot chan struct{}
	runs map[string]time.Time
	// pausing closes when a pause begins; paused holds until it ends.
	pausing chan struct{}
	cur     string
	mu      sync.Mutex
	paused  bool
}

// NewRunner answers a Runner for cfg.
func NewRunner(cfg *Config) *Runner {
	return &Runner{cfg: *cfg, slot: make(chan struct{}, 1), runs: map[string]time.Time{}, pausing: make(chan struct{})}
}

// errPaused is the Unproven of a run asked for while a pause holds.
const errPaused = "the host is changing its egress; probe it again once its profiles are ready"

// Pause waits for a run in flight and starts none until resume is called:
// a run asked for meanwhile answers at once, unproven. A ctx that ends
// first answers its error and leaves no pause. One pause at a time.
func (r *Runner) Pause(ctx context.Context) (resume func(), err error) {
	r.mu.Lock()
	r.paused = true
	close(r.pausing)
	r.mu.Unlock()
	unpause := func() {
		r.mu.Lock()
		r.paused, r.pausing = false, make(chan struct{})
		r.mu.Unlock()
	}
	select {
	case r.slot <- struct{}{}:
	case <-ctx.Done():
		unpause()
		return nil, ctx.Err()
	}
	return func() {
		unpause()
		<-r.slot
	}, nil
}

// pause answers whether a pause holds and the channel a pause closes.
func (r *Runner) pause() (paused bool, pausing <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused, r.pausing
}

func pausedReport() hostapi.ProbeReport {
	now := time.Now()
	return hostapi.ProbeReport{StartedAt: now, EndedAt: now, Unproven: errPaused}
}

// Deadline bounds a probe request: two runs of the host's current plan.
func (r *Runner) Deadline() time.Duration {
	return egress.PlanProbe(placeholder(new(r.cfg.Host().Input))).Deadline(docker.CallTimeout)
}

// placeholder fills in's run addresses so the plan holds every check a run
// makes; a run's checks depend on its addresses' families only.
func placeholder(base *egress.ProbeInput) *egress.ProbeInput {
	in := *base
	in.JobGateway, in.Listener = netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")
	if in.Host.Egress.IPv6 {
		in.JobGateway6, in.Listener6 = netip.MustParseAddr("fd00::1"), netip.MustParseAddr("fd00::2")
	}
	in.ListenerPort = listenerPort
	return &in
}

// Leftover reports a container of probe run that a tidy pass removes: its
// run ended at least one call timeout ago, so none of its calls is still in
// flight. A run this supervisor never saw ended at its start.
func (r *Runner) Leftover(run string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run == r.cur {
		return false
	}
	ended, ok := r.runs[run]
	if !ok {
		ended = r.cfg.Started
	}
	return time.Since(ended) >= callTimeout
}

// Run runs the probe once any run in progress ends. A request whose ctx
// ends while it waits starts no run, and one made while a pause holds
// answers at once, unproven; a run once started finishes and empties its
// sets whatever ctx does.
func (r *Runner) Run(ctx context.Context) (hostapi.ProbeReport, error) {
	paused, pausing := r.pause()
	if paused {
		return pausedReport(), nil
	}
	select {
	case r.slot <- struct{}{}:
	case <-pausing:
		return pausedReport(), nil
	case <-ctx.Done():
		return hostapi.ProbeReport{}, ctx.Err()
	}
	defer func() { <-r.slot }()
	// A select with several cases ready picks one at random.
	if err := ctx.Err(); err != nil {
		return hostapi.ProbeReport{}, err
	}
	if paused, _ := r.pause(); paused {
		return pausedReport(), nil
	}
	h := r.cfg.Host()
	plan := egress.PlanProbe(placeholder(&h.Input))
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), plan.Bound(docker.CallTimeout))
	defer cancel()
	run := newRunID()
	r.mu.Lock()
	r.cur = run
	r.mu.Unlock()
	rep := hostapi.ProbeReport{StartedAt: time.Now()}
	leave := r.run(rctx, run, &h, &rep)
	rep.EndedAt = time.Now()
	rep.Proven = rep.Unproven == "" && allRequired(rep.Checks)
	r.cleanUp(ctx, run, leave)
	r.ended(run)
	if rep.Unproven != "" {
		r.cfg.Log.Warn("A probe run ended unproven", "run", run, "why", rep.Unproven)
	}
	return rep, nil
}

// cleanUp empties the probe sets and, unless leave, removes run's
// containers, within one call timeout whatever ctx does.
func (r *Runner) cleanUp(ctx context.Context, run string, leave bool) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if err := r.cfg.Sets.Clear(cctx); err != nil {
		r.cfg.Log.Error("Emptying the probe sets failed", "error", err, "bound", callTimeout)
	}
	if !leave {
		r.remove(cctx, run)
	}
}

// ended records run ended now and forgets each run that ended a call
// timeout ago, which Leftover then judges by the supervisor's start alike.
func (r *Runner) ended(run string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(r.runs, func(_ string, at time.Time) bool { return now.Sub(at) >= callTimeout })
	r.cur, r.runs[run] = "", now
}

func allRequired(cs []hostapi.ProbeCheck) bool {
	for i := range cs {
		if cs[i].Required && !cs[i].Pass {
			return false
		}
	}
	return true
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// run makes run's checks into rep, answering whether its containers must
// stay: a call that timed out may still be in flight.
func (r *Runner) run(ctx context.Context, run string, h *Host, rep *hostapi.ProbeReport) (leave bool) {
	r.tidy(ctx)
	if h.No64 != "" {
		rep.Unproven = "the machine's /64 is not learned: " + h.No64
		return false
	}
	listener, err := r.container(ctx, run, roleListen, false)
	if err != nil {
		return unproven(rep, err)
	}
	probe, err := r.container(ctx, run, roleProbe, true)
	if err != nil {
		return unproven(rep, err)
	}
	defer probe.stream.Close()
	in := h.Input
	in.Listener, in.Listener6, in.ListenerPort = listener.ep.addr4(), listener.ep.addr6(in.Host.Egress.IPv6), listenerPort
	in.JobGateway, in.JobGateway6 = probe.ep.gateway4(), probe.ep.gateway6(in.Host.Egress.IPv6)
	plan := egress.PlanProbe(&in)
	if err := r.cfg.Sets.SetSources(ctx, probe.ep.addrs()); err != nil {
		return unproven(rep, fmt.Errorf("filling the probe sets: %w", err))
	}
	x := newExchanger(probe.stream)
	output := false
	for i := range plan.Checks {
		c := &plan.Checks[i]
		if c.Leg == egress.LegOutput && !output {
			output = true
			_ = probe.stream.Close()
			if err := r.cfg.Sets.SetSources(ctx, h.Sources); err != nil {
				return unproven(rep, fmt.Errorf("filling the probe sets: %w", err))
			}
		}
		pc, err := r.check(ctx, c, x, in.Host.DNS)
		if err != nil {
			return unproven(rep, err)
		}
		rep.Checks = append(rep.Checks, pc)
	}
	return false
}

// unproven ends rep's run with err, answering whether the containers stay.
func unproven(rep *hostapi.ProbeReport, err error) bool {
	rep.Unproven = err.Error()
	return errors.Is(err, errInFlight)
}

// check makes one check: a block check with only its target in the target
// sets and the counters read on each side of it.
func (r *Runner) check(ctx context.Context, c *egress.Check, x *exchanger, dns []netip.Addr) (hostapi.ProbeCheck, error) {
	start := time.Now()
	var o egress.Outcome
	counted, err := r.counted(ctx, c, func() error {
		if c.Leg != egress.LegJob {
			o = outputCheck(ctx, c, dns)
			return nil
		}
		var err error
		if o, err = x.exchange(c); err != nil {
			return fmt.Errorf("check %q: %w", c.Name, err)
		}
		return nil
	})
	if err != nil {
		return hostapi.ProbeCheck{}, err
	}
	took := time.Since(start)
	v := egress.Judge(c, o, counted)
	if c.Kind == egress.KindTry {
		r.cfg.Log.Info("An allow entry's connection", "target", c.Addr, "outcome", o.String())
	}
	leg := "job"
	if c.Leg == egress.LegOutput {
		leg = "output"
	}
	return hostapi.ProbeCheck{
		Name: c.Name, Leg: leg, Target: c.Addr, Outcome: o.String(), Reason: v.Reason,
		Duration: took, Pass: v.Pass, Required: v.Required, Unproven: v.Unproven,
	}, nil
}

// counted runs fn, and for a block check answers the probe counters'
// change across it with only c's target in the target sets.
func (r *Runner) counted(ctx context.Context, c *egress.Check, fn func() error) (egress.ProbeCounts, error) {
	if c.Kind != egress.KindBlock {
		return egress.ProbeCounts{}, fn()
	}
	if err := r.cfg.Sets.SetTarget(ctx, c.Target); err != nil {
		return egress.ProbeCounts{}, fmt.Errorf("setting the target of check %q: %w", c.Name, err)
	}
	before, err := r.cfg.Sets.Counts(ctx)
	if err != nil {
		return egress.ProbeCounts{}, fmt.Errorf("reading the probe counters: %w", err)
	}
	if ferr := fn(); ferr != nil {
		return egress.ProbeCounts{}, ferr
	}
	after, err := r.cfg.Sets.Counts(ctx)
	if err != nil {
		return egress.ProbeCounts{}, fmt.Errorf("reading the probe counters: %w", err)
	}
	if err := r.cfg.Sets.SetTarget(ctx, netip.Addr{}); err != nil {
		return egress.ProbeCounts{}, fmt.Errorf("emptying the target sets: %w", err)
	}
	return after.Since(before), nil
}
