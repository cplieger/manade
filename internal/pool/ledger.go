// Package pool runs the one queue and the runners behind every scale set:
// it ranks the queued jobs, reserves each funded slot on the first of its
// profile's hosts with room, decides the capacity each scale set reports
// before its poll, and starts a runner on the host its slot was reserved
// on for each job GitHub assigns.
package pool

import (
	"math"
	"slices"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// res is a reservation: milli-CPUs, memory and disk bytes, and runners.
type res struct{ cpu, mem, disk, runners int64 }

// unbounded is a budget dimension the config leaves unset.
const unbounded = math.MaxInt64 / 4

func (r res) plus(o res) res {
	return res{r.cpu + o.cpu, r.mem + o.mem, r.disk + o.disk, r.runners + o.runners}
}

func (r res) minus(o res) res {
	return res{r.cpu - o.cpu, r.mem - o.mem, r.disk - o.disk, r.runners - o.runners}
}

func (r res) times(n int) res {
	k := int64(n)
	return res{r.cpu * k, r.mem * k, r.disk * k, r.runners * k}
}

// within reports whether r fits in o in every dimension.
func (r res) within(o res) bool {
	return r.cpu <= o.cpu && r.mem <= o.mem && r.disk <= o.disk && r.runners <= o.runners
}

// ledger is the pool's books: every host's budget and every scale set's
// assignments, runners and reserved slots. It does no I/O and takes no
// lock, so its caller serializes the calls and keeps the gates current
// before each: every hostBook's reachable, serving and ready, and every
// book's serve and fundable.
type ledger struct {
	byName map[string]int
	// strays holds, by runner name, the runners of no scale set the config
	// holds: each keeps its reservation until its host reports it ended.
	strays map[string]stray
	// held is, by host, the head reserve the last poll's allocation kept:
	// only a book holding a claim (book.claimant) is placed there. need is
	// what the claims want there.
	held         []bool
	need         []res
	hosts        []hostBook
	profiles     []profileBook
	books        []*book
	reserveAfter time.Duration
}

// stray is a runner of no configured scale set: its reservation on host,
// and its profile's index, -1 for a profile the config no longer has,
// whose max_runners it counts against.
type stray struct {
	// created is when this process took it; its lifetime counts from there.
	created time.Time
	// scope is the key of the scope it registered in.
	scope         string
	res           res
	host, profile int
	// recheck marks a record a generation change left with no scale set:
	// the next listing judges it as an adoption, so an idle one is removed.
	recheck bool
}

// hostBook is one host's budget and gates.
type hostBook struct {
	// ready reports, by profile index, that the host's status lists the
	// profile ready.
	ready  []bool
	budget res
	// disk reports a capacity.disk, so a slot there counts its profile's
	// disk.
	disk bool
	// reported marks a host whose settings set no capacity: its budget is
	// what its last status admits (§10.2), zero until one answers.
	reported bool
	// reachable reports that the host answers, so its runners' exits
	// reach the ledger; serving, that it takes new slots: reachable,
	// admitted and proved.
	reachable, serving bool
}

// profileBook is one profile's slot and the hosts that may serve it.
type profileBook struct {
	// hosts holds host indices in the profile's order of preference.
	hosts []int
	slot  res
	// max is max_runners, 0 for none.
	max int
}

func newLedger(cfg *config.Config) *ledger {
	l := &ledger{byName: map[string]int{}, strays: map[string]stray{}, held: make([]bool, len(cfg.Hosts)), need: make([]res, len(cfg.Hosts)), reserveAfter: cfg.HeadReserveAfter}
	hostAt := map[string]int{}
	for i := range cfg.Hosts {
		c := &cfg.Hosts[i].Capacity
		hb := hostBook{budget: res{c.MilliCPUs, c.Memory, unbounded, unbounded}, ready: make([]bool, len(cfg.Profiles)), reported: c.MilliCPUs == 0}
		if c.Disk > 0 {
			hb.budget.disk, hb.disk = c.Disk, true
		}
		if c.MaxRunners > 0 {
			hb.budget.runners = int64(c.MaxRunners)
		}
		hostAt[cfg.Hosts[i].Name] = i
		l.hosts = append(l.hosts, hb)
	}
	for i := range cfg.Profiles {
		pr := &cfg.Profiles[i]
		pb := profileBook{slot: res{cpu: pr.MilliCPUs, mem: pr.Memory, disk: pr.Disk, runners: 1}, max: pr.MaxRunners}
		for _, name := range pr.Hosts {
			pb.hosts = append(pb.hosts, hostAt[name])
		}
		l.byName[pr.Name] = i
		l.profiles = append(l.profiles, pb)
	}
	return l
}

// book is one scale set's records.
type book struct {
	standSince    time.Time
	standEnded    time.Time
	unplacedSince time.Time
	lastAssigned  time.Time
	asg           *assignments
	runners       map[string]*runner
	skipped       map[jobKey]skip
	misses        misses
	ss            github.ScaleSet
	pend          []int
	jobs          []github.QueuedJob
	flight        flight
	profile       int
	org           bool
	claimant      bool
	serve         bool
	fundable      bool
}

// flight is the figure the scale set's last poll began with, and what it
// funded. It stands until the next poll begins or a session opens, so the
// headroom it carried stays reserved while GitHub may still fill it.
type flight struct {
	// funded is how many waiting jobs the figure funded.
	figure, funded   int
	standing, active bool
}

// misses counts polls that left a funded slot unfilled; first is when the
// first counted one began.
type misses struct {
	first time.Time
	n     int
}

// skip is an unassignable job: out of the queue until until, and wait is
// the hold its next verdict doubles.
type skip struct {
	until time.Time
	wait  time.Duration
}

type runnerState int

const (
	starting runnerState = iota
	idle
	busy
)

// runner is one runner's record. It holds its reservation on host from
// the moment the record exists until end deletes it, so it is freed
// exactly once, when its host reports the container ended.
type runner struct {
	// created is when the record was made, or an adopted container started.
	created time.Time
	// req is the POST a start sent and no host answered, repeated by name;
	// nil once answered.
	req  *hostapi.RunnerRequest
	name string
	// instance is the supervisor that holds the record: the last one that
	// listed it or answered its POST, else the last one seen before sending
	// it.
	instance string
	// closing is the reason the pool decided the record closes with, ""
	// until it decided.
	closing ExitReason
	res     res
	// id is the registration's ID, 0 until GitHub answers its JIT config.
	id int64
	// answered is the host's sequence number at the POST's answer, 0
	// until one answers; only a list begun after it judges the runner.
	answered uint64
	host     int
	state    runnerState
	// completed reports that a JobCompleted named the runner; posting,
	// that its POST is in flight; counted, that its start was counted;
	// failed, that its host failed the start; stale, that its profile's
	// spec changed in a later generation, so it takes no new job.
	completed, posting, counted, failed, stale bool
}

// addBook records a scale set; its profile is one the config defines.
func (l *ledger) addBook(ss github.ScaleSet, org bool) *book {
	b := &book{
		asg: newAssignments(), runners: map[string]*runner{}, skipped: map[jobKey]skip{},
		pend: make([]int, len(l.hosts)), ss: ss, profile: l.byName[ss.Profile], org: org,
	}
	l.books = append(l.books, b)
	return b
}

// adopt records a runner a host lists, holding r, its built limits, there.
// The caller settles the ledger once it has adopted every runner.
func (b *book) adopt(name string, host int, r res, state runnerState) *runner {
	rr := &runner{name: name, res: r, host: host, state: state}
	b.runners[name] = rr
	return rr
}

// hold keeps a stray runner's reservation on its host until release.
func (l *ledger) hold(name string, st *stray) { l.strays[name] = *st }

// release frees a stray's reservation once its host reported it ended.
func (l *ledger) release(name string, now time.Time) {
	delete(l.strays, name)
	l.settle(now)
}

// end deletes the runner's record, which frees its reservation, once its
// host reported its container ended or its start failed.
func (l *ledger) end(b *book, name string, now time.Time) {
	delete(b.runners, name)
	l.settle(now)
}

// busy counts b's runners on a job no JobCompleted has ended.
func (b *book) busy() int {
	n := 0
	for _, r := range b.runners {
		if r.state == busy && !r.completed {
			n++
		}
	}
	return n
}

// lost counts the jobs GitHub runs for b on no runner it holds: a runner
// whose container ended with no JobCompleted. GitHub keeps such a job
// running, and assigned, until its JobCompleted or the runner's
// registration is gone, so the scale set's capacity carries a slot for
// each, which costs no budget.
func (b *book) lost() int { return max(0, b.asg.stats.Running-b.busy()) }

// covering counts b's runners that will take an assignment GitHub has not
// started: starting and idle ones, and ones whose job completed, whose
// slot the next assignment waits for. Each counts only on a reachable
// host, whose report of its end arrives.
func (l *ledger) covering(b *book) int {
	n := 0
	for _, r := range b.runners {
		if l.hosts[r.host].reachable && (r.state != busy || r.completed) {
			n++
		}
	}
	return n
}

// demand is how many of b's assignments want a runner start.
func (l *ledger) demand(b *book) int {
	return max(0, b.asg.stats.Assigned-b.asg.stats.Running-l.covering(b))
}

// target is how many slots b needs reserved beyond its runners: one for
// each assignment waiting for a runner and each kept cancel, or as many as
// its poll in flight lets GitHub fill.
func (l *ledger) target(b *book) int {
	need := l.demand(b) + len(b.asg.kept)
	if b.flight.active {
		need = max(need, b.flight.figure-b.lost()-len(b.runners))
	}
	return max(0, need)
}

// reserved counts b's runners and its slots reserved on hosts.
func (b *book) reserved() int { return len(b.runners) + sum(b.pend) }

// covered counts what b reserves past its assignments and kept cancels:
// idle runners and the headroom of its poll in flight, which serve its
// oldest waiting jobs without a new reservation.
func (l *ledger) covered(b *book) int {
	st := b.asg.stats
	return max(0, l.covering(b)+sum(b.pend)-(st.Assigned-st.Running)-len(b.asg.kept))
}

// runningOn counts b's runners on host h on a job no JobCompleted ended.
func (b *book) runningOn(h int) int {
	n := 0
	for _, r := range b.runners {
		if r.host == h && r.state == busy && !r.completed {
			n++
		}
	}
	return n
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

// standOrder is the order standing slots are offered in: organization and
// enterprise scale sets by when their last standing slot ended, never-held
// first, then repository scale sets, most recently assigned first.
func (l *ledger) standOrder() []int {
	order := make([]int, len(l.books))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		x, y := l.books[i], l.books[j]
		switch {
		case x.org != y.org:
			return b2i(y.org) - b2i(x.org)
		case x.org:
			return x.standEnded.Compare(y.standEnded)
		}
		return y.lastAssigned.Compare(x.lastAssigned)
	})
	return order
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
