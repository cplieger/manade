// Package pool runs the one queue and the runners behind every scale set:
// it ranks the queued jobs the REST sweep reads, decides the capacity each
// scale set reports before its poll, starts a runner for each job GitHub
// assigns, and reconciles the containers with Docker.
package pool

import (
	"math"
	"slices"
	"time"
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

// claim is one scale set's place in an allocation.
type claim struct {
	// waiting holds the queue times of the scale set's ranked jobs, oldest
	// first. allocate ignores it unless the claim is fundable and a repo's.
	waiting []time.Time
	// held is what the scale set's records reserve now: its runners, the
	// slots behind its assignments and kept cancels, and the headroom of a
	// poll in flight, whose figure no allocation can lower.
	held res
	// slot is one more runner's reservation: the profile's.
	slot    res
	profile int
	// covered is how many of the oldest waiting jobs held already serves:
	// idle runners past the assignments and a poll's headroom in flight
	// both count. The part past the waiting jobs is the standing slot
	// already offered.
	covered int
	// fundable reports an open session no other process holds, on a scope
	// whose last visibility read proved it private and whose last queue
	// read was whole, and an image in service. Capacity checks the proof's
	// age.
	fundable bool
	org      bool
}

// grant is what an allocation funds a scale set above what it holds.
type grant struct {
	funded   int
	standing bool
}

// allocation is one pass over every claim.
type allocation struct {
	headSince time.Time
	grants    []grant
	head      int
	// reserved reports that the head waited past the reserve and does not
	// fit, so nothing was funded.
	reserved bool
}

// allocate funds cs's ranked jobs from the budget left after every claim's
// holdings, oldest queue time first, skipping a job whose profile is at its
// cap or does not fit (backfill). The head is the oldest job whose profile
// is under its cap; once it has waited reserveAfter and still does not
// fit, nothing is funded. What is left gives standing slots, one per
// fundable scale set: organization and enterprise claims first, then repo
// claims, each in cs's order, passing over one that does not fit. caps
// holds each profile's max_runners, 0 for none.
func allocate(budget res, caps []int, cs []claim, now time.Time, reserveAfter time.Duration) allocation {
	a := allocation{grants: make([]grant, len(cs)), head: -1}
	free := budget
	used := make([]int, len(caps))
	for _, c := range cs {
		free = free.minus(c.held)
		used[c.profile] += int(c.held.runners)
	}
	headFunded := a.fund(cs, caps, used, &free)
	if a.head >= 0 && !headFunded && now.Sub(a.headSince) >= reserveAfter {
		clear(a.grants)
		a.reserved = true
	}
	a.stand(cs, caps, used, &free, true)
	a.stand(cs, caps, used, &free, false)
	return a
}

// stand offers standing slots to the fundable claims whose org field is
// org, in cs's order.
func (a *allocation) stand(cs []claim, caps, used []int, free *res, org bool) {
	for i := range cs {
		c := &cs[i]
		if !c.fundable || c.org != org || c.covered > len(ranked(c)) || a.reserved {
			continue
		}
		if underCap(caps, used, c.profile) && c.slot.within(*free) {
			*free = free.minus(c.slot)
			used[c.profile]++
			a.grants[i].standing = true
		}
	}
}

// fund walks the ranked jobs oldest first and reports whether the head was
// funded.
func (a *allocation) fund(cs []claim, caps, used []int, free *res) bool {
	type job struct {
		at       time.Time
		claim, i int
	}
	var jobs []job
	for ci := range cs {
		for i, at := range ranked(&cs[ci]) {
			jobs = append(jobs, job{at, ci, i})
		}
	}
	slices.SortStableFunc(jobs, func(x, y job) int { return x.at.Compare(y.at) })
	headFunded := false
	for _, j := range jobs {
		c := &cs[j.claim]
		under := underCap(caps, used, c.profile)
		funded := j.i < c.covered
		if !funded && under && c.slot.within(*free) {
			*free = free.minus(c.slot)
			used[c.profile]++
			a.grants[j.claim].funded++
			funded = true
		}
		if a.head < 0 && under {
			a.head, a.headSince, headFunded = j.claim, j.at, funded
		}
	}
	return headFunded
}

// ranked is the claim's jobs that take part in the one queue.
func ranked(c *claim) []time.Time {
	if !c.fundable || c.org {
		return nil
	}
	return c.waiting
}

func underCap(caps, used []int, p int) bool { return caps[p] == 0 || used[p] < caps[p] }
