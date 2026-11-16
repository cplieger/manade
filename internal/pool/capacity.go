package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// capacity answers the maxCapacity of b's poll beginning at start, and
// whether it is above the slots b's assignments hold, which needs a fresh
// proof that its scope is private; without one it is cut to those slots.
// The figure is b's runners and reserved slots, a slot per lost runner and
// what the allocation funds it, less what the head reserve takes back
// (takeBack); 0 when b is not served.
func (l *ledger) capacity(b *book, start time.Time, fresh bool) (int, bool) {
	b.flight = flight{active: true}
	l.settle(start)
	if !b.serve {
		return 0, false
	}
	a := l.allocate(start)
	if b.rotate(start) {
		a = l.allocate(start)
	}
	l.keep(&a)
	i := slices.Index(l.books, b)
	figure := b.reserved() - l.takeBack(i, &a) + b.lost()
	grants := slices.Clone(a.grants[i])
	if h := a.standing[i]; h >= 0 {
		grants = append(grants, h)
	}
	if !b.fundable {
		grants = nil
	}
	figure += len(grants)
	owed := b.asg.owed()
	above := figure > owed
	if above && !fresh {
		figure, grants = owed, nil
	}
	room, open := max(0, figure-owed), len(l.open(b, start))
	f := flight{figure: figure, funded: min(open, room), standing: room > open, active: true}
	switch {
	case f.standing && b.standSince.IsZero():
		b.standSince = start
	case !f.standing && !b.standSince.IsZero():
		b.standSince, b.standEnded = time.Time{}, start
	}
	for _, h := range grants {
		b.pend[h]++
	}
	b.flight = f
	l.settle(start)
	return figure, above
}

// keep records a's head reserve for settle: a refill GitHub assigns
// another book is never placed on a host held for the head.
func (l *ledger) keep(a *allocation) {
	copy(l.held, a.held)
	copy(l.need, a.need)
	for i, b := range l.books {
		b.claimant = a.claimant[i]
	}
}

// takeBack is how many of book i's slots on hosts the head reserve holds
// its figure leaves out: GitHub refills a slot freed inside a poll at the
// figure the poll began with. On each held host, the fewest slots whose
// end covers what the head still needs past the free budget, at most its
// running jobs and spare runners there; a claimant book is left whole.
func (l *ledger) takeBack(i int, a *allocation) int {
	if a.claimant[i] {
		return 0
	}
	b := l.books[i]
	spare := l.spares(b, a.held)
	n := 0
	for h, held := range a.held {
		if have := b.runningOn(h) + len(spare[h]); held && have > 0 {
			n += min(have, slotsFor(a.need[h].minus(a.free[h]), l.slot(b.profile, h)))
		}
	}
	return n
}

// spares answers, by held host, b's idle runners there that no assignment
// of b's waits for, by name: they would hold the slot the head waits for
// with no job to end and free it. A starting runner covers an assignment
// first and is never named: it is judged only once listed running.
func (l *ledger) spares(b *book, held []bool) map[int][]string {
	n := -(b.asg.stats.Assigned - b.asg.stats.Running)
	var names []string
	for name, r := range b.runners {
		if r.state == busy || !l.hosts[r.host].reachable {
			continue
		}
		n++
		if r.state == idle && held[r.host] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	out := map[int][]string{}
	for _, name := range names[:max(0, min(n, len(names)))] {
		h := b.runners[name].host
		out[h] = append(out[h], name)
	}
	return out
}

// retire answers the spare runners the head reserve the last poll kept
// wants removed: on each held host, of each book holding no claim, as many
// as the head still needs there. The caller removes each as idle, after
// which end frees its slot.
func (l *ledger) retire() map[*book][]string {
	free := l.free()
	out := map[*book][]string{}
	for _, b := range l.books {
		if b.claimant {
			continue
		}
		for h, names := range l.spares(b, l.held) {
			k := slotsFor(l.need[h].minus(free[h]), l.slot(b.profile, h))
			out[b] = append(out[b], names[:min(k, len(names))]...)
		}
		if len(out[b]) == 0 {
			delete(out, b)
		}
	}
	return out
}

// slotsFor is the fewest slots of size s whose sum covers short in every
// dimension s takes part in.
func slotsFor(short, s res) int {
	k := int64(0)
	for _, d := range [][2]int64{{short.cpu, s.cpu}, {short.mem, s.mem}, {short.disk, s.disk}, {short.runners, s.runners}} {
		if d[0] > 0 && d[1] > 0 {
			k = max(k, (d[0]+d[1]-1)/d[1])
		}
	}
	return int(k)
}

// rotate ends an organization or enterprise book's standing slot still
// unfilled rotateAfter after the first poll carrying it began, so the next
// goes to the one whose last slot ended longest ago. While the budget
// covers all of them, the allocation that follows grants it again.
func (b *book) rotate(start time.Time) bool {
	if !b.org || b.standSince.IsZero() || start.Sub(b.standSince) < rotateAfter {
		return false
	}
	b.standSince, b.standEnded = time.Time{}, start
	return true
}

// opened takes a new session's statistics: its last figure no longer
// stands, and its assignments get slots as far as the budget covers them.
func (l *ledger) opened(b *book, st github.Stats, now time.Time) {
	b.flight = flight{}
	b.asg.observe(st)
	l.settle(now)
}

// dropFlight ends b's last figure: another process holds its session, so
// nothing b reported stands.
func (l *ledger) dropFlight(b *book, now time.Time) {
	b.flight = flight{}
	l.settle(now)
}

// polled takes an answered poll of b: its assignments, its runners' job
// messages, whether it filled the standing slot it carried, and whether it
// left a funded slot unfilled.
func (l *ledger) polled(b *book, p *github.Poll) {
	slots := b.reserved() + b.lost()
	if b.flight.active {
		slots = max(slots, b.flight.figure)
	}
	b.asg.apply(p)
	b.asg.fitKept(slots)
	n := b.mark(p.Events)
	if n > 0 {
		b.lastAssigned = p.Ended
	}
	if b.flight.standing && n > b.flight.funded {
		b.standSince, b.standEnded = time.Time{}, p.Ended
	}
	switch {
	case b.flight.funded == 0 || n > 0:
		b.misses = misses{}
	case p.Idle || p.Ended.Sub(p.Started) >= countedPoll:
		if b.misses.n == 0 {
			b.misses.first = p.Started
		}
		b.misses.n++
	}
	l.settle(p.Ended)
}

// mark records the job messages of b's runners and answers how many
// JobAssigned events arrived.
func (b *book) mark(events []github.Event) int {
	n := 0
	for i := range events {
		e := &events[i]
		r := b.runners[e.Runner]
		switch {
		case e.Kind == github.Assigned:
			n++
		case e.Kind == github.Started && r != nil:
			r.state = busy
		case e.Kind == github.Completed && r != nil:
			r.completed = true
		}
	}
	return n
}
