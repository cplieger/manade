package pool

import (
	"slices"
	"time"
)

// slot is one runner of profile p's reservation on host h. It counts the
// profile's disk only where the host's capacity sets disk.
func (l *ledger) slot(p, h int) res {
	s := l.profiles[p].slot
	if !l.hosts[h].disk {
		s.disk = 0
	}
	return s
}

// usable reports that host h takes new slots of profile p.
func (l *ledger) usable(p, h int) bool {
	hb := &l.hosts[h]
	return hb.serving && hb.ready[p]
}

// eligible reports that a slot of profile p could be placed on host h once
// its runners end: usable, and within h's whole budget.
func (l *ledger) eligible(p, h int) bool {
	return l.usable(p, h) && l.slot(p, h).within(l.hosts[h].budget)
}

// free answers each host's budget less its runners' reservations and the
// slots reserved there.
func (l *ledger) free() []res {
	free := make([]res, len(l.hosts))
	for h := range l.hosts {
		free[h] = l.hosts[h].budget
	}
	for _, b := range l.books {
		for _, r := range b.runners {
			free[r.host] = free[r.host].minus(r.res)
		}
		for h, n := range b.pend {
			free[h] = free[h].minus(l.slot(b.profile, h).times(n))
		}
	}
	for _, st := range l.strays {
		free[st.host] = free[st.host].minus(st.res)
	}
	return free
}

// used answers, by profile, its runners and reserved slots, which its
// max_runners caps.
func (l *ledger) used() []int {
	used := make([]int, len(l.profiles))
	for _, b := range l.books {
		used[b.profile] += b.reserved()
	}
	for _, st := range l.strays {
		if st.profile >= 0 {
			used[st.profile]++
		}
	}
	return used
}

func (l *ledger) underCap(p int, used []int) bool {
	return l.profiles[p].max == 0 || used[p] < l.profiles[p].max
}

// fit answers the first host in profile p's order of preference that takes
// its slot: usable, not held, and with room in free. A nil held holds none.
func (l *ledger) fit(p int, free []res, held []bool) (int, bool) {
	for _, h := range l.profiles[p].hosts {
		if l.usable(p, h) && (len(held) == 0 || !held[h]) && l.slot(p, h).within(free[h]) {
			return h, true
		}
	}
	return 0, false
}

// settle brings every book's reserved slots to what it needs. It frees the
// slots on a host that no longer takes their profile, and those past the
// book's target from its least preferred host up; then it reserves a slot
// for each assignment that has none, the books whose assignments went
// unplaced longest first, each on the first host with room that the head
// reserve does not hold for another book. Assignments no host has room
// for stay unplaced, counted in asg.unbacked, and the next allocation
// holds their hosts for them.
func (l *ledger) settle(now time.Time) {
	for _, b := range l.books {
		l.trim(b)
	}
	free, used := l.free(), l.used()
	order := slices.Clone(l.books)
	slices.SortStableFunc(order, func(x, y *book) int { return since(x, now).Compare(since(y, now)) })
	for _, b := range order {
		l.place(b, free, used)
	}
	for _, b := range l.books {
		b.asg.unbacked = max(0, l.demand(b)-sum(b.pend))
		switch {
		case b.asg.unbacked == 0:
			b.unplacedSince = time.Time{}
		case b.unplacedSince.IsZero():
			b.unplacedSince = now
		}
	}
}

// trim frees b's slots on hosts that no longer take its profile, and
// those past its target from its least preferred host up.
func (l *ledger) trim(b *book) {
	hosts := l.profiles[b.profile].hosts
	for _, h := range hosts {
		if !b.serve || !l.usable(b.profile, h) {
			b.pend[h] = 0
		}
	}
	excess := sum(b.pend) - l.target(b)
	for i := len(hosts) - 1; i >= 0 && excess > 0; i-- {
		k := min(excess, b.pend[hosts[i]])
		b.pend[hosts[i]] -= k
		excess -= k
	}
}

// place reserves a slot for each of b's assignments that has none, on the
// first host with room the head reserve does not hold for another book.
func (l *ledger) place(b *book, free []res, used []int) {
	held := l.held
	if b.claimant {
		held = nil
	}
	for want := l.demand(b) - sum(b.pend); want > 0 && b.serve && l.underCap(b.profile, used); want-- {
		h, ok := l.fit(b.profile, free, held)
		if !ok {
			return
		}
		b.pend[h]++
		free[h] = free[h].minus(l.slot(b.profile, h))
		used[b.profile]++
	}
}

// since is when b's assignments went unplaced, now for a book whose
// assignments all have a slot.
func since(b *book, now time.Time) time.Time {
	if b.unplacedSince.IsZero() {
		return now
	}
	return b.unplacedSince
}

// take records a starting runner named name for one of b's assignments
// that waits for one, on the most preferred host holding one of b's slots,
// and answers that host. It answers false when no assignment waits or none
// has a slot; the caller settles after each gate change, so a slot it
// takes is on a host that takes it.
func (l *ledger) take(b *book, name string) (int, bool) {
	if !b.serve || l.demand(b) == 0 {
		return 0, false
	}
	for _, h := range l.profiles[b.profile].hosts {
		if b.pend[h] > 0 && l.usable(b.profile, h) {
			b.pend[h]--
			b.runners[name] = &runner{name: name, res: l.slot(b.profile, h), host: h, state: starting}
			return h, true
		}
	}
	return 0, false
}
