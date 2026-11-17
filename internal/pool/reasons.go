package pool

import (
	"errors"
	"slices"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// HostReason is why one host of a waiting job's profile gives it no runner
// now, a closed set.
type HostReason string

// The host reasons, in the order the first that holds is taken.
const (
	hostTooSmall           HostReason = "too_small"
	hostDisconnected       HostReason = "disconnected"
	hostDraining           HostReason = "draining"
	hostSettingsNotApplied HostReason = "settings_not_applied"
	hostProving            HostReason = "proving"
	hostUnproved           HostReason = "unproved"
	hostNotReady           HostReason = "not_ready"
	hostImagePulling       HostReason = "image_pulling"
	hostHeld               HostReason = "held"
	hostFull               HostReason = "full"
)

// JobReason is why a job waits, a closed set: the first that applies,
// so it reads as close to running as any of its hosts lets it.
type JobReason string

// The job reasons.
const (
	JobStarting           JobReason = "starting"
	jobAwaitingAssignment JobReason = "awaiting_assignment"
	jobUnassignable       JobReason = "unassignable"
	jobScopeNotServed     JobReason = "scope_not_served"
	JobProfileLimit       JobReason = "profile_limit"
	jobHeldForOlder       JobReason = "held_for_older"
	JobWaitingForSlot     JobReason = "waiting_for_slot"
	jobImagePulling       JobReason = "image_pulling"
	jobProving            JobReason = "proving"
	jobHostNotReady       JobReason = "host_not_ready"
	JobNoHostServing      JobReason = "no_host_serving"
	jobTooLarge           JobReason = "too_large"
)

// JobReasons is every JobReason, nearest running first; a tie for the
// reason a job waited under longest goes to the one nearer running.
var JobReasons = []JobReason{
	JobStarting, jobAwaitingAssignment, jobUnassignable, jobScopeNotServed, JobProfileLimit, jobHeldForOlder,
	JobWaitingForSlot, jobImagePulling, jobProving, jobHostNotReady, JobNoHostServing, jobTooLarge,
}

// The details beside a reason that names a cause of its own: a scope not
// served, and a host's own not_ready or settings_not_applied. A section
// state (hostapi.SectionState) or a profile's readiness reason
// (hostapi.Reason) is passed through as its own text.
const (
	detailEnsureFailed         = "ensure_failed"
	detailNotConnected         = "github_not_connected"
	detailEnterpriseNeedsToken = "enterprise_needs_token"
	detailHeldByOtherPool      = "held_by_other_pool"
	detailScaleSetLost         = "scale_set_lost"
	detailPublic               = "public"
	DetailVisibilityUnproved   = "visibility_unproved"
	detailQueueReadPartial     = "queue_read_partial"
	detailPoolStopping         = "pool_stopping"
	detailAdopting             = "adopting"
	detailCapacityOverLimits   = "capacity_over_limits"
	detailProfileRefused       = "profile_refused"
)

// Waiting is one job waiting for a runner: an assignment GitHub made with
// no runner started (Assigned, which no read names) or a queued job the
// last read listed, its reason with the facts the client words it from,
// and each host of its profile with that host's own reason.
type Waiting struct {
	Created time.Time
	github.ScaleSet
	Name   string
	Reason JobReason
	// Detail names the cause beside scope_not_served, and HeldBy the
	// other pool's owner beside held_by_other_pool.
	Detail, HeldBy string
	Hosts          []HostWait
	RunID          int64
	// Runners and Limit are the profile's runners and slots and its
	// max_runners, beside JobProfileLimit.
	Runners, Limit int
	Assigned       bool
}

// HostWait is one host's reason for a waiting job, with its runners and
// max_runners (0 for none) beside full.
type HostWait struct {
	Host           string
	Reason         HostReason
	Detail         string
	Runners, Limit int
}

// waiting answers every waiting job with its reason from allocation a, the
// allocator's verdict at now; g.mu is held.
func (g *generation) waiting(now time.Time, a *allocation) []Waiting {
	var out []Waiting
	used := g.l.used()
	for bi, b := range g.l.books {
		st := b.asg.stats
		unstarted := max(0, st.Assigned-st.Running)
		starting := min(unstarted, g.l.covering(b)+sum(b.pend))
		for range starting {
			out = append(out, Waiting{ScaleSet: b.ss, Assigned: true, Reason: JobStarting})
		}
		for range unstarted - starting {
			w := Waiting{ScaleSet: b.ss, Assigned: true}
			g.slotReason(&w, bi, a, used, now)
			out = append(out, w)
		}
		rank := 0
		for _, j := range b.asg.waiting(b.jobs) {
			w := Waiting{ScaleSet: b.ss, Created: j.Created, Name: j.Name, RunID: j.RunID}
			switch {
			case now.Before(b.skipped[jobKey{j.Name, j.RunID}].until):
				w.Reason = jobUnassignable
			case !b.fundable:
				g.notServed(&w, b)
			case a.funded[[2]int{bi, rank}]:
				w.Reason = jobAwaitingAssignment
			default:
				g.slotReason(&w, bi, a, used, now)
			}
			if w.Reason != jobUnassignable && b.fundable {
				rank++
			}
			out = append(out, w)
		}
	}
	return append(out, g.unensured()...)
}

// slotReason fills w's reason for a job of book bi that a holds no slot
// for: the profile's cap, then the nearest any host comes to a slot.
func (g *generation) slotReason(w *Waiting, bi int, a *allocation, used []int, now time.Time) {
	b := g.l.books[bi]
	p := b.profile
	if !g.l.underCap(p, used) {
		w.Reason, w.Runners, w.Limit = JobProfileLimit, used[p], g.l.profiles[p].max
		return
	}
	for _, h := range g.l.profiles[p].hosts {
		w.Hosts = append(w.Hosts, g.hostWait(p, h, a, a.claimant[bi], now))
	}
	has := func(rs ...HostReason) bool {
		return slices.ContainsFunc(w.Hosts, func(hw HostWait) bool { return slices.Contains(rs, hw.Reason) })
	}
	switch {
	case has(hostHeld):
		w.Reason = jobHeldForOlder
	case has(hostFull):
		w.Reason = JobWaitingForSlot
	case has(hostImagePulling):
		w.Reason = jobImagePulling
	case has(hostProving):
		w.Reason = jobProving
	case has(hostSettingsNotApplied, hostNotReady):
		w.Reason = jobHostNotReady
	case has(hostDisconnected, hostDraining, hostUnproved):
		w.Reason = JobNoHostServing
	default:
		w.Reason = jobTooLarge
	}
}

// hostWait is host h's reason for a job of profile p that a holds no slot
// for; claimant reports that a holds h for that job's book. A host whose
// budget waits on its first report is never too small.
func (g *generation) hostWait(p, h int, a *allocation, claimant bool, now time.Time) HostWait {
	ho, hb := g.hosts[h], &g.l.hosts[h]
	w := HostWait{Host: ho.cfg.Name}
	slot := g.l.slot(p, h)
	switch {
	case (!hb.reported || ho.st != nil) && !slot.within(hb.budget):
		w.Reason = hostTooSmall
	case !ho.reachable || !ho.cfg.Accepting || !ho.current || ho.proof(now) != proofHeld:
		w.Reason, w.Detail = unserved(ho, now)
	case !ho.settled:
		w.Reason, w.Detail = hostNotReady, detailAdopting
	case ho.admit != nil:
		w.Reason, w.Detail = hostNotReady, detailCapacityOverLimits
	case !hb.ready[p]:
		w.Reason, w.Detail = profileWait(ho.st, g.cfg.Profiles[p].Name)
	case slot.within(a.left[h]) && a.held[h] && !claimant:
		w.Reason = hostHeld
	default:
		w.Reason, w.Runners = hostFull, g.runnersOn(h)
		if hb.budget.runners < unbounded {
			w.Limit = int(hb.budget.runners)
		}
	}
	return w
}

// unserved is why ho, connected or not, takes nothing before its own
// readiness counts: its connection, its drain, its section or its proof.
func unserved(ho *host, now time.Time) (reason HostReason, detail string) {
	switch {
	case !ho.reachable:
		return hostDisconnected, ""
	case !ho.cfg.Accepting:
		return hostDraining, ""
	case !ho.current:
		return hostSettingsNotApplied, sectionDetail(ho.st)
	case ho.proof(now) == proofPending:
		return hostProving, ""
	}
	return hostUnproved, ""
}

// sectionDetail names why a host does not run the section last delivered:
// its section state, or waiting for the section while it runs an older one.
func sectionDetail(st *hostapi.Status) string {
	if st == nil || st.Section.State == hostapi.SectionRunning {
		return string(hostapi.SectionWaiting)
	}
	return string(st.Section.State)
}

// profileWait reads why a host's status lists the profile not ready.
func profileWait(st *hostapi.Status, profile string) (reason HostReason, detail string) {
	i := slices.IndexFunc(st.Profiles, func(ps hostapi.ProfileStatus) bool { return ps.Name == profile })
	if i < 0 {
		return hostNotReady, detailProfileRefused
	}
	switch r := st.Profiles[i].Reason; r {
	case hostapi.ReasonPulling, hostapi.ReasonImageMissing:
		return hostImagePulling, ""
	case hostapi.ReasonEgressChangePending, hostapi.ReasonEgressChangeFailed, hostapi.ReasonRestartPending:
		return hostSettingsNotApplied, string(r)
	default:
		return hostNotReady, string(r)
	}
}

// runnersOn counts the runners and strays holding a reservation on host h.
func (g *generation) runnersOn(h int) int {
	n := 0
	for _, b := range g.l.books {
		for _, r := range b.runners {
			if r.host == h {
				n++
			}
		}
	}
	for _, st := range g.l.strays {
		if st.host == h {
			n++
		}
	}
	return n
}

// notServed gives w, a job of b, its scope_not_served reason: the detail
// and, beside held_by_other_pool, the holder; g.mu is held.
func (g *generation) notServed(w *Waiting, b *book) {
	w.Reason, w.Detail = jobScopeNotServed, g.unservedDetail(b)
	if w.Detail == detailHeldByOtherPool {
		w.HeldBy = g.sets[b].holder
	}
}

// unservedDetail names why b funds nothing; g.mu is held.
func (g *generation) unservedDetail(b *book) string {
	s := g.sets[b]
	switch {
	case g.stopping:
		return detailPoolStopping
	case !g.credentialed:
		return detailNotConnected
	case s.scope.public:
		return detailPublic
	case s.lost && s.scope.ensureErr != nil:
		return ensureDetail(s.scope.ensureErr)
	case s.lost:
		return detailScaleSetLost
	case s.held:
		return detailHeldByOtherPool
	case !s.scope.private():
		return DetailVisibilityUnproved
	}
	return detailQueueReadPartial
}

// unensured answers the queued jobs of each profile serving a scope that
// has no scale set of it yet: its ensure failed or has not answered.
func (g *generation) unensured() []Waiting {
	var out []Waiting
	for i := range g.cfg.Scopes {
		sc := g.scopes[g.cfg.Scopes[i].Key]
		for j := range g.cfg.Profiles {
			pr := &g.cfg.Profiles[j]
			if !pr.Serves(sc.cfg.Key) || g.bookOf(sc.cfg.Key, pr.Name) != nil {
				continue
			}
			for _, job := range sc.jobsOf(pr.Name) {
				out = append(out, Waiting{
					Scope: sc.cfg.Key, Profile: pr.Name, Created: job.Created, Name: job.Name, RunID: job.RunID,
					Reason: jobScopeNotServed, Detail: ensureDetail(sc.ensureErr),
				})
			}
		}
	}
	return out
}

// ensureDetail names why a scope's scale sets were not ensured: no
// credential, a credential that cannot serve the scope's kind, or a
// failed call.
func ensureDetail(err error) string {
	switch {
	case errors.Is(err, errNoCredential):
		return detailNotConnected
	case errors.Is(err, github.ErrEnterpriseNeedsToken):
		return detailEnterpriseNeedsToken
	}
	return detailEnsureFailed
}
