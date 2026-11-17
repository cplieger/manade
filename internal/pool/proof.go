package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

// reproveEvery is how often a serving host is proved again.
const reproveEvery = 10 * time.Minute

// host is the pool's view of one host of the settings, read from its
// status and proofs over its connection.
type host struct {
	// admit is the refusal of the host's capacity against st, nil once
	// admitted.
	admit error
	// want asks for a proof; the prover is its one reader, so triggers
	// that arrive during a run fold into one follow-up run. wake asks the
	// host's reconcile loop for a pass the same way.
	want, wake chan struct{}
	// beat is when the host's reconcile loop last began or ended a pass
	// or a call it waited on, and passed when it last ended a pass.
	// provedAt is when the proof in proved passed, and probeUntil the
	// deadline of the probe of probing.
	beat, passed, provedAt, probeUntil time.Time
	// st is the last status read, nil until one answers; report is the
	// last probe's report, nil until one answers.
	st     *hostapi.Status
	report *hostapi.ProbeReport
	cfg    *config.Host
	// fprint is st's proof fingerprint; tried is the fingerprint the last
	// proof began at; probing, the one a proof in flight runs at; proved,
	// the one the last passing proof ran at, "" when the last proof failed
	// or none passed; failed, the one the last failed proof ran at. failure
	// is why the last proof failed. instance is the supervisor st came from.
	proved, instance, fprint, tried, probing, failed, failure string
	// noted is the state last handed to the history, with notedDetail.
	noted       HostState
	notedDetail string
	// ready holds, by profile index, the readiness st reports.
	ready []bool
	// seq numbers the host's POST answers and list beginnings, so a list
	// judges only the runners whose POST was answered before it began.
	seq uint64
	// current reports st running the section this generation delivers the
	// host; reachable, that the last pass read st and the runner list, or
	// st alone from a host not up; up, that st came from a host up on a
	// section, which answers its other calls; judged, that a pass has tried
	// it; waiting, that its reconcile loop waits on a call; settled, that
	// its last pass adopted or removed every runner it listed that no
	// record held.
	current, reachable, up, judged, waiting, settled bool
}

// serving reports whether the host takes new slots at now: reachable,
// settled, accepting, running its delivered section, its capacity
// admitted and a proof holding for its status.
func (h *host) serving(now time.Time) bool {
	return h.reachable && h.settled && h.cfg.Accepting && h.admit == nil && h.current && h.proof(now) == proofHeld
}

// proofState is where a host's proof of its status stands (§10.5).
type proofState int

const (
	// proofHeld is a passing proof of the status's fingerprint, inside
	// reproveEvery or with a re-proof of it running inside its deadline.
	proofHeld proofState = iota
	// proofPending is no proof of the fingerprint completed, and one
	// running inside its deadline or asked for.
	proofPending
	// proofMissing is a failed proof of the fingerprint, or a pass too old
	// with none running.
	proofMissing
)

func (h *host) proof(now time.Time) proofState {
	running := h.fprint != "" && h.probing == h.fprint && now.Before(h.probeUntil)
	switch {
	case h.proved != "" && h.proved == h.fprint && (now.Sub(h.provedAt) <= reproveEvery || running):
		return proofHeld
	case h.failed != h.fprint && h.proved != h.fprint && (running || h.tried != h.fprint):
		return proofPending
	}
	return proofMissing
}

// reproofDue reports a passing proof of the status that reaches
// reproveEvery with none running, which the next pass asks for.
func (h *host) reproofDue(now time.Time) bool {
	return h.proved == h.fprint && h.probing == "" && now.Sub(h.provedAt) >= reproveEvery
}

// carry takes what old's generation learned of the same host: its status,
// proof, supervisor and sequence. The settings-derived verdicts are judged
// again for the new settings.
func (h *host) carry(o *host) {
	h.st, h.report, h.beat, h.passed, h.seq = o.st, o.report, o.beat, o.passed, o.seq
	h.proved, h.instance, h.fprint, h.tried, h.failure = o.proved, o.instance, o.fprint, o.tried, o.failure
	h.provedAt, h.probing, h.probeUntil, h.failed = o.provedAt, o.probing, o.probeUntil, o.failed
	h.reachable, h.judged, h.settled = o.reachable, o.judged, o.settled
	h.noted, h.notedDetail = o.noted, o.notedDetail
}

// fingerprint is what a proof stands for in a status: the supervisor, its
// policy hash and the egress state it reports. Any change of it asks for a
// new proof, and the host serves nothing until that proof passes.
func fingerprint(st *hostapi.Status) string {
	ipv6 := ""
	if st.IPv6 != nil {
		ipv6 = st.IPv6.Prefix.String()
	}
	return fmt.Sprintf("%s|%s|%d|%s|%t|%v|%v|%v|%t|%s",
		st.InstanceID, st.PolicyHash, st.Rules.Restored, st.Rules.Backend, st.Rules.RawOff,
		st.Outward, st.DNS.Configured, st.DNS.Original, st.DNS.OriginalUnknown, ipv6)
}

// applyStatus takes a status host i answered; g.mu is held.
func (g *generation) applyStatus(i int, st *hostapi.Status) {
	h := g.hosts[i]
	was := h.serving(g.now())
	if h.instance != "" && h.instance != st.InstanceID && g.busyOn(i) > 0 {
		g.log.Warn("A host restarted with jobs running, which ends them", "host", h.cfg.Name, "jobs", g.busyOn(i))
	}
	if h.instance != st.InstanceID {
		h.settled = false
	}
	h.st, h.instance, h.reachable, h.judged = st, st.InstanceID, true, true
	g.judge(i)
	if h.fprint = fingerprint(st); h.fprint != h.tried || h.reproofDue(g.now()) {
		wake(h.want)
	}
	g.d.Hosts.Status(h.cfg.Name, g.now(), st)
	g.served(i, was)
}

// judge reads host i's last status against this generation's settings: its
// readiness, its section and its capacity; g.mu is held.
func (g *generation) judge(i int) {
	h, st := g.hosts[i], g.hosts[i].st
	h.ready = readiness(g.cfg, h.cfg.Name, st)
	want := g.sections[h.cfg.Name]
	h.current = want != "" && st.Section.State == hostapi.SectionRunning && st.Section.Hash == want
	if !h.up {
		// A host not up reports no limits to judge its capacity by.
		return
	}
	hb := &g.l.hosts[i]
	if hb.reported {
		hb.budget.cpu, hb.budget.mem = st.Limits.MilliCPUs, max(0, st.Memory.Admissible)
		h.admit = nil
		return
	}
	err := admit(h.cfg, st)
	switch {
	case err != nil && (h.admit == nil || h.admit.Error() != err.Error()):
		g.log.Error("A host's capacity is more than it reports it can give, so it serves nothing", "host", h.cfg.Name, "error", err)
	case err == nil && h.admit != nil:
		g.log.Info("A host's capacity fits what it reports again", "host", h.cfg.Name)
	}
	h.admit = err
}

// unreachable records that host i did not answer or is not connected;
// g.mu is held. Its runners keep their reservations: their jobs may still
// run there.
func (g *generation) unreachable(i int, err error) {
	h := g.hosts[i]
	was := h.serving(g.now())
	// A host not yet connected at a pass before any judged it is a start,
	// not a loss.
	if h.reachable || (!h.judged && !errors.Is(err, errDisconnected)) {
		g.log.Warn("A host does not answer, so it takes no new jobs; its runners keep their reservations", "host", h.cfg.Name, "error", err)
	}
	h.reachable, h.judged = false, true
	if !errors.Is(err, errDisconnected) {
		g.d.Metrics.HostLinkError(h.cfg.Name)
	}
	g.served(i, was)
}

// served records a change of host i's serving and state; g.mu is held.
func (g *generation) served(i int, was bool) {
	h := g.hosts[i]
	g.noteState(i)
	if now := h.serving(g.now()); now && !was {
		g.log.Info("A host serves jobs", "host", h.cfg.Name)
	}
}

// busyOn counts the runners on host i a job holds; g.mu is held.
func (g *generation) busyOn(i int) int {
	n := 0
	for _, b := range g.l.books {
		n += b.runningOn(i)
	}
	return n
}

// runProver proves host i whenever its want fires and every reproveEvery,
// one proof at a time, until ctx ends.
func (g *generation) runProver(ctx context.Context, i int) {
	t := time.NewTicker(reproveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.hosts[i].want:
		case <-t.C:
		}
		g.proveHost(ctx, i)
	}
}

// proveHost runs one proof of host i's status. A proof counts only for the
// status it began at; a status that changed meanwhile has asked for the
// next.
func (g *generation) proveHost(ctx context.Context, i int) {
	h := g.hosts[i]
	g.mu.Lock()
	if !h.reachable || !h.up || h.st == nil {
		g.mu.Unlock()
		return
	}
	fp, deadline := h.fprint, h.st.ProbeDeadline
	h.tried, h.probing, h.probeUntil = fp, fp, g.now().Add(deadline)
	g.mu.Unlock()
	r, err := g.api(i).Probe(ctx, deadline)
	if ctx.Err() != nil {
		// A cut proof decided nothing, so the next status asks again.
		g.mu.Lock()
		h.probing, h.tried = "", ""
		g.mu.Unlock()
		return
	}
	rep, why := &r, probeVerdict(&r, err)
	g.mu.Lock()
	defer g.mu.Unlock()
	h.probing = ""
	now := g.now()
	was := h.serving(now)
	switch {
	case why == "" && h.fprint == fp:
		h.proved, h.provedAt, h.failed, h.failure = fp, now, "", ""
	case why == "":
	default:
		if why != h.failure {
			g.log.Error("A host failed its egress proof, so it takes no new jobs", "host", h.cfg.Name, "check", why)
		}
		h.proved, h.failed, h.failure = "", fp, why
	}
	if err != nil {
		rep = nil
	} else {
		h.report = rep
	}
	g.d.Hosts.Proved(h.cfg.Name, now, rep, why)
	g.served(i, was)
	g.gate()
	g.poke()
}

// probeVerdict is why a probe answer fails the proof, "" when it passes.
func probeVerdict(rep *hostapi.ProbeReport, err error) string {
	switch {
	case errors.Is(err, hostapi.ErrProbeTimeout):
		return "unproven: " + err.Error()
	case err != nil:
		return "the probe request failed: " + err.Error()
	case rep.Proven:
		return ""
	case rep.Unproven != "":
		return "unproven: " + rep.Unproven
	}
	for _, c := range rep.Checks {
		if c.Required && !c.Pass {
			return fmt.Sprintf("%s (%s leg, %s): %s", c.Name, c.Leg, c.Target, c.Reason)
		}
	}
	return "the probe reported the host unproven with no failed check"
}
