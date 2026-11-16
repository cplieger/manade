package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// The queue's timings: a standing slot's rotation, the shortest poll that
// proves a funded slot went unused, and the hold of an unassignable job,
// up to GitHub's 24-hour queue limit.
const (
	rotateAfter = 2 * time.Minute
	countedPoll = 30 * time.Second
	skipFirst   = 10 * time.Minute
	skipMax     = 24 * time.Hour
)

// allocation is one pass over the queue: what it funds each book beyond
// what it reserves, and which hosts the head reserve holds.
type allocation struct {
	headSince time.Time
	// grants holds, by book, the host of each slot it funds.
	grants [][]int
	// standing holds, by book, the host of its standing slot, -1 for none.
	standing []int
	// held reports, by host, that the head reserve keeps it for the books
	// in claimant; need is what they still want there, and free each
	// host's budget left before any grant.
	held      []bool
	need      []res
	free      []res
	claimant  []bool
	headFound bool
}

// queued is one ranked job: a book's waiting job, its queue time and its
// rank within the book.
type queued struct {
	at      time.Time
	book, i int
}

// allocate funds the queue from the budget left after every reservation:
// unplaced assignments first, since GitHub already gave them to this pool
// and each holds its profile's hosts at once; then the ranked jobs, oldest
// first, each on the first host of its profile with room, passing over one
// that does not fit (backfill, until the head reserve, fundJob); then the
// standing slots from what is left (offerStanding).
func (l *ledger) allocate(now time.Time) allocation {
	n := len(l.books)
	a := allocation{
		grants: make([][]int, n), standing: make([]int, n), claimant: make([]bool, n),
		held: make([]bool, len(l.hosts)), need: make([]res, len(l.hosts)), free: l.free(),
	}
	for i := range a.standing {
		a.standing[i] = -1
	}
	w := walk{a: &a, free: slices.Clone(a.free), used: l.used(), covered: make([]int, n)}
	for i, b := range l.books {
		w.covered[i] = l.covered(b)
	}
	l.claimUnplaced(&w, now)
	for _, j := range l.ranked(now) {
		l.fundJob(&w, j, now)
	}
	l.offerStanding(&w, now)
	return a
}

// walk is an allocation in progress: what is left of each host's budget,
// each profile's slots and each book's covered jobs.
type walk struct {
	a       *allocation
	free    []res
	used    []int
	covered []int
}

// claimUnplaced holds the hosts of each book with unplaced assignments.
func (l *ledger) claimUnplaced(w *walk, now time.Time) {
	for _, i := range l.unplaced(now) {
		b := l.books[i]
		if !l.underCap(b.profile, w.used) || !l.anyEligible(b.profile) {
			continue
		}
		w.a.hold(l, i, b.asg.unbacked)
		if !w.a.headFound {
			w.a.headSince, w.a.headFound = b.unplacedSince, true
		}
	}
}

// fundJob funds ranked job j where it fits. The first job under its cap
// that some host could hold is the head; once it has waited reserveAfter
// and does not fit, its profile's hosts are held for it.
func (l *ledger) fundJob(w *walk, j queued, now time.Time) {
	a, b := w.a, l.books[j.book]
	under := l.underCap(b.profile, w.used)
	if j.i < w.covered[j.book] {
		if !a.headFound && under {
			a.headSince, a.headFound = j.at, true
		}
		return
	}
	if !under || !l.anyEligible(b.profile) {
		return
	}
	h, ok := l.fit(b.profile, w.free, a.held)
	if ok {
		w.take(l, b.profile, h)
		a.grants[j.book] = append(a.grants[j.book], h)
	}
	if a.headFound {
		return
	}
	a.headSince, a.headFound = j.at, true
	if !ok && now.Sub(j.at) >= l.reserveAfter {
		a.hold(l, j.book, 1)
	}
}

// offerStanding offers one standing slot per fundable book in standOrder,
// on hosts not held, none past an organization's that did not fit (close).
func (l *ledger) offerStanding(w *walk, now time.Time) {
	closed := slices.Clone(w.a.held)
	for _, i := range l.standOrder() {
		b := l.books[i]
		if !b.fundable || w.covered[i] > len(l.rankedOf(b, now)) || !l.underCap(b.profile, w.used) {
			continue
		}
		h, ok := l.fit(b.profile, w.free, closed)
		switch {
		case ok:
			w.take(l, b.profile, h)
			w.a.standing[i] = h
		case b.org:
			l.close(b.profile, closed)
		}
	}
}

func (w *walk) take(l *ledger, p, h int) {
	w.free[h] = w.free[h].minus(l.slot(p, h))
	w.used[p]++
}

// close shuts every host profile p could use to the standing slots after
// an organization's or enterprise's of p that did not fit: its jobs, which
// no read sees, reach a runner only through a standing slot, and smaller
// slots offered past it would keep its hosts split for good.
func (l *ledger) close(p int, closed []bool) {
	for _, h := range l.profiles[p].hosts {
		if l.eligible(p, h) {
			closed[h] = true
		}
	}
}

// hold keeps every host book i's profile could use for it, adding k of
// its slots to what each still needs.
func (a *allocation) hold(l *ledger, i, k int) {
	b := l.books[i]
	a.claimant[i] = true
	for _, h := range l.profiles[b.profile].hosts {
		if l.eligible(b.profile, h) {
			a.held[h] = true
			a.need[h] = a.need[h].plus(l.slot(b.profile, h).times(k))
		}
	}
}

func (l *ledger) anyEligible(p int) bool {
	return slices.ContainsFunc(l.profiles[p].hosts, func(h int) bool { return l.eligible(p, h) })
}

// unplaced answers the books whose assignments wait for a slot, longest
// waiting first.
func (l *ledger) unplaced(now time.Time) []int {
	var out []int
	for i, b := range l.books {
		if b.serve && b.asg.unbacked > 0 {
			out = append(out, i)
		}
	}
	slices.SortStableFunc(out, func(i, j int) int { return since(l.books[i], now).Compare(since(l.books[j], now)) })
	return out
}

// ranked answers every fundable repository book's waiting jobs, oldest
// queue time first. An organization's or enterprise's jobs cannot be
// read, so they take part only through standing slots.
func (l *ledger) ranked(now time.Time) []queued {
	var jobs []queued
	for bi, b := range l.books {
		for i, at := range l.rankedOf(b, now) {
			jobs = append(jobs, queued{at: at, book: bi, i: i})
		}
	}
	slices.SortStableFunc(jobs, func(x, y queued) int { return x.at.Compare(y.at) })
	return jobs
}

func (l *ledger) rankedOf(b *book, now time.Time) []time.Time {
	if !b.fundable || b.org {
		return nil
	}
	var out []time.Time
	for _, j := range l.open(b, now) {
		out = append(out, j.Created)
	}
	return out
}

// open answers b's waiting jobs, oldest first, less the unassignable.
func (*ledger) open(b *book, now time.Time) []github.QueuedJob {
	var out []github.QueuedJob
	for _, j := range b.asg.waiting(b.jobs) {
		if now.Before(b.skipped[jobKey{j.Name, j.RunID}].until) {
			continue
		}
		out = append(out, j)
	}
	return out
}

// skipped is a job reread marked unassignable, out of the queue until
// until.
type skipped struct {
	until time.Time
	job   github.QueuedJob
}

// reread takes a whole read of b's queued jobs, ending at ended. It forgets
// the assignments and unassignable jobs the read no longer lists, and once
// b's last two counted polls left a funded slot unfilled, marks b's
// waiting jobs unassignable: out of the queue for skipFirst, doubling with
// each further verdict up to skipMax. It answers the jobs it marked.
func (*ledger) reread(b *book, jobs []github.QueuedJob, ended time.Time) []skipped {
	b.jobs = jobs
	b.asg.prune(jobs)
	listed := map[jobKey]bool{}
	for _, j := range jobs {
		listed[jobKey{j.Name, j.RunID}] = true
	}
	for k := range b.skipped {
		if !listed[k] {
			delete(b.skipped, k)
		}
	}
	if b.misses.n < 2 || !ended.After(b.misses.first) {
		return nil
	}
	b.misses = misses{}
	var out []skipped
	for _, j := range b.asg.waiting(jobs) {
		k := jobKey{j.Name, j.RunID}
		prev := b.skipped[k]
		if ended.Before(prev.until) {
			continue
		}
		wait := skipFirst
		if prev.wait > 0 {
			wait = min(2*prev.wait, skipMax)
		}
		b.skipped[k] = skip{until: ended.Add(wait), wait: wait}
		out = append(out, skipped{until: ended.Add(wait), job: j})
	}
	return out
}
