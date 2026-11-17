package history

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// The bounds on what a host's text puts in a row: hosts on
// other machines are not trusted to keep it short.
const (
	textMax   = 512
	checksMax = 256
)

// detailStopped is the gap the recorder writes for each host it observed
// when it stops.
const detailStopped = "control_plane_stopped"

var _ pool.HostHistory = (*Recorder)(nil)

// clip is host text as a row keeps it: one line, at most textMax bytes.
func clip(s string) string {
	out, _ := textsafe.SanitizeSingleLineCapped(s, textMax, "...")
	return out
}

// facts is what the history compares of a host's status.
type facts struct {
	// digests holds each profile's digest in service, "" while none is;
	// images, the reference each names.
	digests, images  map[string]string
	instance, policy string
	restored         uint64
}

func factsOf(st *hostapi.Status) facts {
	f := facts{
		instance: clip(st.InstanceID), policy: clip(st.PolicyHash), restored: st.Rules.Restored,
		digests: map[string]string{}, images: map[string]string{},
	}
	for i := range st.Profiles {
		p := &st.Profiles[i]
		name := clip(p.Name)
		f.digests[name], f.images[name] = clip(p.Digest), clip(p.Image)
	}
	return f
}

// State queues host's state for its write.
func (rec *Recorder) State(host string, at time.Time, state pool.HostState, detail string) {
	detail = clip(detail)
	rec.enqueue(func(ctx context.Context) { rec.writeState(ctx, host, at, string(state), detail) })
}

// Status queues what host's status changed for its write.
func (rec *Recorder) Status(host string, at time.Time, st *hostapi.Status) {
	f := factsOf(st)
	rec.enqueue(func(ctx context.Context) { rec.writeStatus(ctx, host, at, &f) })
}

// Proved queues host's proof for its write.
func (rec *Recorder) Proved(host string, at time.Time, rep *hostapi.ProbeReport, failure string) {
	p := probeOf(host, at, rep, failure)
	rec.enqueue(func(ctx context.Context) { rec.writeProbe(ctx, &p) })
}

func probeOf(host string, at time.Time, rep *hostapi.ProbeReport, failure string) store.Probe {
	p := store.Probe{At: at, Host: host, Failure: clip(failure)}
	if rep == nil {
		return p
	}
	p.Instance, p.Unproven, p.Proven = clip(rep.InstanceID), clip(rep.Unproven), rep.Proven
	checks := rep.Checks
	if len(checks) > checksMax {
		p.Dropped, checks = len(checks)-checksMax, checks[:checksMax]
	}
	for i := range checks {
		c := &checks[i]
		p.Checks = append(p.Checks, store.ProbeCheck{
			Name: clip(c.Name), Leg: clip(c.Leg), Target: clip(c.Target), Outcome: clip(c.Outcome), Reason: clip(c.Reason),
			DurationMS: c.Duration.Milliseconds(), Pass: c.Pass, Required: c.Required, Unproven: c.Unproven,
		})
	}
	return p
}

func (rec *Recorder) writeState(ctx context.Context, host string, at time.Time, state, detail string) {
	rec.observe(ctx, host, "Writing a host's state to the history failed", stateChange(host, at, state, detail))
}

func stateChange(host string, at time.Time, state, detail string) observation {
	return func(s *store.HostSeen) ([]store.HostEvent, *store.Probe) {
		if s.State == state && s.Detail == detail {
			return nil, nil
		}
		s.State, s.Detail = state, detail
		return []store.HostEvent{{At: at, Host: host, Kind: store.HostEventState, State: state, Detail: detail}}, nil
	}
}

func (rec *Recorder) writeStatus(ctx context.Context, host string, at time.Time, f *facts) {
	rec.observe(ctx, host, "Writing a host's status to the history failed", func(s *store.HostSeen) ([]store.HostEvent, *store.Probe) {
		evs := statusEvents(s, f)
		for i := range evs {
			evs[i].At, evs[i].Host, evs[i].Instance = at, host, f.instance
		}
		s.Instance, s.Restored = f.instance, f.restored
		if f.policy != "" {
			s.Policy = f.policy
		}
		next := map[string]string{}
		for name, d := range f.digests {
			if d = cmp.Or(d, s.Digests[name]); d != "" {
				next[name] = d
			}
		}
		s.Digests = next
		return evs, nil
	})
}

// statusEvents answers the events f gives against s, the facts last
// recorded: none for a host no status was recorded of.
func statusEvents(s *store.HostSeen, f *facts) []store.HostEvent {
	if s.Instance == "" {
		return nil
	}
	var evs []store.HostEvent
	switch {
	case f.instance != s.Instance:
		evs = append(evs, store.HostEvent{Kind: store.HostEventRestart})
	case f.restored > s.Restored:
		evs = append(evs, store.HostEvent{Kind: store.HostEventRulesRestored, Count: f.restored - s.Restored})
	}
	if s.Policy != "" && f.policy != "" && f.policy != s.Policy {
		evs = append(evs, store.HostEvent{Kind: store.HostEventPostureRebuilt})
	}
	for _, name := range slices.Sorted(maps.Keys(f.digests)) {
		if d := f.digests[name]; d != "" && d != s.Digests[name] {
			evs = append(evs, store.HostEvent{Kind: store.HostEventImagePulled, Profile: name, Image: f.images[name], Digest: d})
		}
	}
	return evs
}

func (rec *Recorder) writeProbe(ctx context.Context, p *store.Probe) {
	rec.observe(ctx, p.Host, "Writing a host's proof to the history failed", func(s *store.HostSeen) ([]store.HostEvent, *store.Probe) {
		verdict := store.ProofPassed
		if p.Failure != "" {
			verdict = store.ProofFailed
		}
		var evs []store.HostEvent
		if verdict != s.Proof || p.Failure != s.ProofFailure {
			evs = []store.HostEvent{{At: p.At, Host: p.Host, Kind: store.HostEventProof, Outcome: verdict, Detail: p.Failure, Instance: p.Instance}}
		}
		s.Proof, s.ProofFailure = verdict, p.Failure
		return evs, p
	})
}

// observe applies fn to a copy of what the history last recorded of host
// and writes, in one transaction, the events and the probe it answers and
// the copy, when any of them is new.
func (rec *Recorder) observe(ctx context.Context, host, failed string, fn observation) {
	err := rec.observeErr(ctx, host, fn)
	if err != nil && !skipped(ctx, err) {
		rec.log.Warn(failed, "host", host, "error", err)
	}
}

// observation changes a copy of what the history last recorded of a host
// and answers the events and the probe the change records.
type observation func(*store.HostSeen) ([]store.HostEvent, *store.Probe)

// errUnchanged ends an observation that records nothing, rolling its
// transaction back.
var errUnchanged = errors.New("history: the observation records nothing new")

// observeErr reads what the history last recorded of host in the
// transaction that writes the change, keeping nothing of the file between
// transactions, so a record another write changed, or a file swapped in,
// decides what is new.
func (rec *Recorder) observeErr(ctx context.Context, host string, fn observation) error {
	rec.observed[host] = struct{}{}
	err := rec.st.UpdateHistory(ctx, func(h *store.HistoryTx) error {
		cur, _, err := h.HostSeen(host)
		if err != nil {
			return err
		}
		next, evs, p, changed := apply(&cur, fn)
		if !changed {
			return errUnchanged
		}
		return putObserved(h, host, &next, evs, p)
	})
	if errors.Is(err, errUnchanged) {
		return nil
	}
	return err
}

// putObserved writes an observation's events, probe and record.
func putObserved(h *store.HistoryTx, host string, next *store.HostSeen, evs []store.HostEvent, p *store.Probe) error {
	for i := range evs {
		if err := h.PutHostEvent(&evs[i]); err != nil {
			return err
		}
	}
	if p != nil {
		if err := h.PutProbe(p); err != nil {
			return err
		}
	}
	return h.PutHostSeen(host, next)
}

// apply answers fn's change to a copy of cur, and whether it records
// anything.
func apply(cur *store.HostSeen, fn observation) (next store.HostSeen, evs []store.HostEvent, p *store.Probe, changed bool) {
	next = *cur
	next.Digests = maps.Clone(cur.Digests)
	evs, p = fn(&next)
	return next, evs, p, len(evs) > 0 || p != nil || !sameSeen(cur, &next)
}

func sameSeen(a, b *store.HostSeen) bool {
	return a.Instance == b.Instance && a.Policy == b.Policy && a.State == b.State && a.Detail == b.Detail &&
		a.Proof == b.Proof && a.ProofFailure == b.ProofFailure && a.Restored == b.Restored && maps.Equal(a.Digests, b.Digests)
}

// stopped writes a gap at at for every host this process observed whose
// recorded state is not already one.
func (rec *Recorder) stopped(ctx context.Context, at time.Time) {
	for _, host := range slices.Sorted(maps.Keys(rec.observed)) {
		gap := stateChange(host, at, "", detailStopped)
		rec.observe(ctx, host, "Writing a host's state to the history failed", func(s *store.HostSeen) ([]store.HostEvent, *store.Probe) {
			if s.State == "" {
				return nil, nil
			}
			return gap(s)
		})
	}
}
