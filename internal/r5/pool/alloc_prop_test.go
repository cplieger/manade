package pool

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// The allocator's properties, as fuzz targets over scenarios
// decoded from the input. Each target's seeds are a fixed pseudo-random set,
// so the plain test run covers the same scenarios every time.

var epoch = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// bytesIn decodes a scenario's choices; an exhausted input reads zeros.
type bytesIn []byte

func (b *bytesIn) n(k int) int {
	if len(*b) == 0 {
		return 0
	}
	v := int((*b)[0]) % k
	*b = (*b)[1:]
	return v
}

type scenario struct {
	now          time.Time
	caps         []int
	claims       []claim
	budget       res
	reserveAfter time.Duration
}

const gib = 1 << 30

func decode(data []byte) scenario {
	in := bytesIn(data)
	var s scenario
	slots := make([]res, 2+in.n(2))
	s.caps = make([]int, len(slots))
	for p := range slots {
		slots[p] = res{cpu: []int64{500, 1000, 2000, 4000}[in.n(4)], mem: int64(1+in.n(8)) * gib, disk: int64(in.n(2) * 10), runners: 1}
		s.caps[p] = []int{0, 0, 0, 1, 2, 4}[in.n(6)]
	}
	s.budget = res{cpu: int64(2+in.n(24)) * 1000, mem: int64(2+in.n(48)) * gib, disk: unbounded, runners: unbounded}
	if in.n(2) == 1 {
		s.budget.disk = int64(10 * (1 + in.n(10)))
	}
	if in.n(2) == 1 {
		s.budget.runners = int64(2 + in.n(10))
	}
	for range 1 + in.n(6) {
		c := claim{profile: in.n(len(slots)), org: in.n(4) == 0, fundable: in.n(5) != 0, covered: []int{0, 0, 0, 1, 2}[in.n(5)]}
		c.slot = slots[c.profile]
		c.held = c.slot.times(c.covered + in.n(2))
		at := epoch
		for range in.n(5) {
			at = at.Add(time.Duration(in.n(30)) * time.Minute)
			c.waiting = append(c.waiting, at)
		}
		s.claims = append(s.claims, c)
	}
	s.now = epoch.Add(time.Duration(60+in.n(90)) * time.Minute)
	s.reserveAfter = time.Duration(in.n(4)) * 30 * time.Minute
	return s
}

func seeds(f *testing.F) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 400 {
		b := make([]byte, 64)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		f.Add(b)
	}
}

// outcome is what an allocation leaves, measured from its grants alone.
type outcome struct {
	start, end res // budget left before and after the grants
	usedStart  []int
	usedEnd    []int
	standing   res // what the standing grants take
}

func measure(s *scenario, a allocation) outcome {
	o := outcome{start: s.budget, usedStart: make([]int, len(s.caps)), usedEnd: make([]int, len(s.caps))}
	for _, c := range s.claims {
		o.start = o.start.minus(c.held)
		o.usedStart[c.profile] += int(c.held.runners)
	}
	o.end = o.start
	copy(o.usedEnd, o.usedStart)
	for i, g := range a.grants {
		c := s.claims[i]
		o.end = o.end.minus(c.slot.times(g.funded))
		o.usedEnd[c.profile] += g.funded
		if g.standing {
			o.end = o.end.minus(c.slot)
			o.standing = o.standing.plus(c.slot)
			o.usedEnd[c.profile]++
		}
	}
	return o
}

// rankedOf is the claim's jobs in the one queue: a fundable repo claim's.
func rankedOf(c *claim) []time.Time {
	if !c.fundable || c.org {
		return nil
	}
	return c.waiting
}

// paid is how many of the claim's ranked jobs the allocation funds or its
// poll in flight covers.
func paid(c *claim, g grant) int { return min(len(rankedOf(c)), c.covered) + g.funded }

func FuzzAllocate_withinBudget(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		o := measure(&s, a)
		granted := o.end != o.start
		if granted && !(res{}).within(o.end) {
			t.Fatalf("grants leave %+v of the budget, below zero (start %+v)", o.end, o.start)
		}
		for p, limit := range s.caps {
			if limit > 0 && o.usedEnd[p] > max(limit, o.usedStart[p]) {
				t.Fatalf("profile %d holds %d runners against its cap %d (held %d before)", p, o.usedEnd[p], limit, o.usedStart[p])
			}
		}
	})
}

func FuzzAllocate_grantsMatchDemand(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		for i, g := range a.grants {
			c := &s.claims[i]
			if paid(c, g) > len(rankedOf(c)) {
				t.Fatalf("claim %d: %d jobs paid for %d ranked", i, paid(c, g), len(rankedOf(c)))
			}
			if g.standing && (!c.fundable || c.covered > len(rankedOf(c))) {
				t.Fatalf("claim %d: a standing slot for a claim that is unfundable (%v) or already holds one", i, !c.fundable)
			}
		}
	})
}

// head finds the queue's head: the oldest ranked job whose profile is under
// its cap before any grant.
func head(s *scenario, o outcome) (int, time.Time) {
	at, idx := time.Time{}, -1
	for i := range s.claims {
		c := &s.claims[i]
		r := rankedOf(c)
		if len(r) == 0 || (s.caps[c.profile] > 0 && o.usedStart[c.profile] >= s.caps[c.profile]) {
			continue
		}
		if idx < 0 || r[0].Before(at) {
			idx, at = i, r[0]
		}
	}
	return idx, at
}

func FuzzAllocate_headFirst(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		o := measure(&s, a)
		hi, at := head(&s, o)
		if hi < 0 {
			if a.reserved {
				t.Fatal("reserved with no head")
			}
			return
		}
		if !a.headSince.Equal(at) {
			t.Fatalf("head since %v, want the oldest uncapped job's %v", a.headSince, at)
		}
		h := &s.claims[hi]
		fits := h.covered > 0 || h.slot.within(o.start)
		funded := paid(h, a.grants[hi]) > 0
		if fits && !funded {
			t.Fatalf("the head fits (%+v in %+v) and is not funded", h.slot, o.start)
		}
		waited := s.now.Sub(at) >= s.reserveAfter
		if a.reserved != (!fits && waited) {
			t.Fatalf("reserved = %v; head fits %v, waited past the reserve %v", a.reserved, fits, waited)
		}
		if a.reserved && o.end != o.start {
			t.Fatalf("the head reserve holds, and grants still take %+v", o.start.minus(o.end))
		}
	})
}

// fundedAt is the queue time of each job the allocation funds, by claim:
// a claim's funded jobs are its oldest after those its poll in flight
// covers, since one claim's jobs share a slot and a cap.
func fundedAt(s *scenario, a allocation) [][]time.Time {
	out := make([][]time.Time, len(s.claims))
	for i, g := range a.grants {
		r := rankedOf(&s.claims[i])
		from := min(len(r), s.claims[i].covered)
		out[i] = r[from:min(len(r), from+g.funded)]
	}
	return out
}

// A waiting job that fits the budget and its profile's cap at its turn,
// what every job not newer than it took aside, is never passed over for a
// newer one.
func FuzzAllocate_oldestFirst(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		o := measure(&s, a)
		at := fundedAt(&s, a)
		for j := range s.claims {
			cj := &s.claims[j]
			rj := rankedOf(cj)
			if paid(cj, a.grants[j]) >= len(rj) {
				continue
			}
			first := rj[paid(cj, a.grants[j])]
			left, used, newer := o.start, o.usedStart[cj.profile], -1
			for k, times := range at {
				for _, when := range times {
					if when.After(first) {
						newer = k
						continue
					}
					left = left.minus(s.claims[k].slot)
					used += b2i(s.claims[k].profile == cj.profile)
				}
			}
			under := s.caps[cj.profile] == 0 || used < s.caps[cj.profile]
			if newer >= 0 && under && cj.slot.within(left) {
				t.Fatalf("claim %d's job of %v fits the %+v left at its turn and waits while claim %d's newer job is funded", j, first, left, newer)
			}
		}
	})
}

func FuzzAllocate_standingFromLeftover(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		o := measure(&s, a)
		if o.standing == (res{}) {
			return
		}
		// Before the standing slots, the budget left was the end plus them.
		left := o.end.plus(o.standing)
		for j := range s.claims {
			cj := &s.claims[j]
			if paid(cj, a.grants[j]) == len(rankedOf(cj)) {
				continue
			}
			capped := s.caps[cj.profile] > 0 && o.usedEnd[cj.profile] >= s.caps[cj.profile]
			if !capped && cj.slot.within(left) {
				t.Fatalf("claim %d's waiting job fits the %+v left, and standing slots take %+v", j, left, o.standing)
			}
		}
	})
}

// standingOrder is the order standing slots are offered in: organization
// and enterprise claims, then repo claims, each in their given order.
func standingOrder(s *scenario) []int {
	var orgs, repos []int
	for i, c := range s.claims {
		if c.org {
			orgs = append(orgs, i)
		} else {
			repos = append(repos, i)
		}
	}
	return append(orgs, repos...)
}

// Walking the standing order, a fundable claim with no standing slot of
// its own never fits the budget and its profile's cap left at its turn,
// unless the head reserve holds. The standing order is organization and
// enterprise claims first, then repo claims, to as many as the budget left
// covers.
func FuzzAllocate_standingInOrder(f *testing.F) {
	seeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		s := decode(data)
		a := allocate(s.budget, s.caps, s.claims, s.now, s.reserveAfter)
		o := measure(&s, a)
		left := o.end.plus(o.standing)
		used := slices.Clone(o.usedStart)
		for i, g := range a.grants {
			used[s.claims[i].profile] += g.funded
		}
		for _, j := range standingOrder(&s) {
			cj := &s.claims[j]
			if a.grants[j].standing {
				left = left.minus(cj.slot)
				used[cj.profile]++
				continue
			}
			if !cj.fundable || cj.covered > len(rankedOf(cj)) || a.reserved {
				continue
			}
			under := s.caps[cj.profile] == 0 || used[cj.profile] < s.caps[cj.profile]
			if under && cj.slot.within(left) {
				t.Fatalf("claim %d (org %v) fits the %+v left at its turn and has no standing slot", j, cj.org, left)
			}
		}
	})
}
