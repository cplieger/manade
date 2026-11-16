package pool

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/github"
)

// The allocation's properties over random books: hosts, profiles on some
// of them, scale sets with runners, assignments, headroom and waiting
// jobs. Each target's seeds are a fixed pseudo-random set, so the plain
// test run covers the same scenarios every time.

func allocSeeds(f *testing.F) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 600 {
		b := make([]byte, 96)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		f.Add(b)
	}
}

// decodeLedger builds a ledger in some state, and the time to allocate at.
func decodeLedger(data []byte) (*ledger, time.Time) {
	in := bytesIn(data)
	var hosts []hostSpec
	for i := range 1 + in.n(3) {
		hosts = append(hosts, hostSpec{name: string(rune('a' + i)), cpus: int64(2 + in.n(14)), mem: int64(4 + in.n(28)), max: []int{0, 0, 2, 6}[in.n(4)]})
	}
	var profiles []profileSpec
	for i := range 1 + in.n(3) {
		p := profileSpec{name: string(rune('p' + i)), cpus: int64(1 + in.n(4)), mem: int64(1 + in.n(8)), max: []int{0, 0, 0, 1, 3}[in.n(5)]}
		for _, k := range rand.New(rand.NewPCG(uint64(in.n(256)), 0)).Perm(len(hosts))[:1+in.n(len(hosts))] {
			p.hosts = append(p.hosts, hosts[k].name)
		}
		profiles = append(profiles, p)
	}
	cfg := testConfig(hosts, profiles)
	cfg.HeadReserveAfter = time.Duration(in.n(4)) * 30 * time.Minute
	l := openLedger(cfg)
	for h := range l.hosts {
		l.hosts[h].serving = in.n(5) != 0
		for p := range l.hosts[h].ready {
			l.hosts[h].ready[p] = l.hosts[h].ready[p] && in.n(5) != 0
		}
	}
	run := int64(0)
	for i := range 1 + in.n(6) {
		p := in.n(len(profiles))
		b := served(l, "acme/r"+string(rune('0'+i)), profiles[p].name, in.n(4) == 0)
		b.fundable = in.n(5) != 0
		busyN := 0
		for k := range in.n(3) {
			h := l.profiles[p].hosts[in.n(len(l.profiles[p].hosts))]
			state := []runnerState{starting, idle, busy, busy}[in.n(4)]
			busyN += b2i(state == busy)
			b.adopt(b.ss.Scope+"-"+string(rune('0'+k)), h, l.slot(p, h), state)
		}
		l.opened(b, github.Stats{Assigned: busyN + in.n(3), Running: busyN}, epoch)
		var js []github.QueuedJob
		for range in.n(5) {
			run++
			js = append(js, github.QueuedJob{RunID: run, Name: "job", Profile: profiles[p].name, Created: epoch.Add(time.Duration(in.n(90)) * time.Minute)})
		}
		l.reread(b, js, epoch)
	}
	now := epoch.Add(time.Duration(60+in.n(90)) * time.Minute)
	for _, b := range l.books {
		if in.n(3) == 0 {
			l.capacity(b, now, true)
		}
	}
	return l, now
}

// funding walks the ranked jobs in queue order and answers, for each, the
// host the allocation funds it on, -1 for none, and whether its book's
// reservations already cover it. A book's grants fund its oldest
// uncovered jobs in order, since its jobs share a slot and a cap.
func funding(l *ledger, a *allocation, now time.Time) (jobs []queued, at []int, covered []bool) {
	for _, j := range l.ranked(now) {
		b := l.books[j.book]
		c := l.covered(b)
		h := -1
		if k := j.i - c; k >= 0 && k < len(a.grants[j.book]) {
			h = a.grants[j.book][k]
		}
		jobs, at, covered = append(jobs, j), append(at, h), append(covered, j.i < c)
	}
	return jobs, at, covered
}

func FuzzAllocate_withinEachHostsBudget(f *testing.F) {
	allocSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		l, now := decodeLedger(data)
		a := l.allocate(now)
		end := slices.Clone(a.free)
		took := make([]bool, len(l.hosts))
		used := l.used()
		for i, b := range l.books {
			hosts := slices.Clone(a.grants[i])
			if a.standing[i] >= 0 {
				hosts = append(hosts, a.standing[i])
			}
			for _, h := range hosts {
				if !l.usable(b.profile, h) || a.held[h] {
					t.Fatalf("book %d funded on host %d, which is not usable (%v) or held (%v)", i, h, l.usable(b.profile, h), a.held[h])
				}
				end[h], took[h] = end[h].minus(l.slot(b.profile, h)), true
				used[b.profile]++
			}
		}
		for h := range end {
			if took[h] && !(res{}).within(end[h]) {
				t.Fatalf("grants leave host %d %+v, below zero (start %+v)", h, end[h], a.free[h])
			}
		}
		before := l.used()
		for p := range l.profiles {
			if c := l.profiles[p].max; c > 0 && used[p] > max(c, before[p]) {
				t.Fatalf("profile %d holds %d slots against its cap %d (held %d before)", p, used[p], c, before[p])
			}
		}
	})
}

// No ranked job waits while it fits a host that is usable, not held, and
// has room left at its turn: what every job ranked before it took aside.
func FuzzAllocate_oldestFirst(f *testing.F) {
	allocSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		l, now := decodeLedger(data)
		a := l.allocate(now)
		free := slices.Clone(a.free)
		jobs, at, covered := funding(l, &a, now)
		for k, j := range jobs {
			b := l.books[j.book]
			if at[k] >= 0 {
				free[at[k]] = free[at[k]].minus(l.slot(b.profile, at[k]))
				continue
			}
			if covered[k] || l.profiles[b.profile].max > 0 {
				continue
			}
			if h, ok := l.fit(b.profile, free, a.held); ok {
				t.Fatalf("book %d's job of %v fits host %d (%+v left at its turn) and waits", j.book, j.at, h, free[h])
			}
		}
	})
}

// The head is the first claim under its cap that a host could hold: when
// it fits, it is funded; when it waited past the reserve, or is an
// unplaced assignment, and does not fit, its hosts are held.
func FuzzAllocate_headFirst(f *testing.F) {
	allocSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		l, now := decodeLedger(data)
		a := l.allocate(now)
		claims := l.unplaced(now)
		claims = slices.DeleteFunc(claims, func(i int) bool {
			b := l.books[i]
			return !l.underCap(b.profile, l.used()) || !l.anyEligible(b.profile)
		})
		if len(claims) > 0 {
			if b := l.books[claims[0]]; !a.claimant[claims[0]] || !a.headSince.Equal(since(b, now)) {
				t.Fatalf("head since %v, claimants %v; want unplaced book %d's assignments holding", a.headSince, a.claimant, claims[0])
			}
			return
		}
		jobs, at, covered := funding(l, &a, now)
		for k, j := range jobs {
			b := l.books[j.book]
			if !l.underCap(b.profile, l.used()) || (!covered[k] && !l.anyEligible(b.profile)) {
				continue
			}
			if !a.headFound || !a.headSince.Equal(j.at) {
				t.Fatalf("head since %v (found %v), want book %d's job of %v", a.headSince, a.headFound, j.book, j.at)
			}
			_, fits := l.fit(b.profile, a.free, nil)
			if fits && !covered[k] && at[k] < 0 {
				t.Fatalf("the head fits and is not funded")
			}
			waited := now.Sub(j.at) >= l.reserveAfter
			if want := !covered[k] && at[k] < 0 && waited; a.claimant[j.book] != want || reserving(&a) != want {
				t.Fatalf("claimants %v, want book %d holding %v (covered %v, funded on %d, waited %v)", a.claimant, j.book, want, covered[k], at[k], waited)
			}
			return
		}
		if a.headFound || reserving(&a) {
			t.Fatalf("head found %v, claimants %v, with no claim and no ranked job", a.headFound, a.claimant)
		}
	})
}

// A standing slot takes only what no waiting job could use, and never
// room an organization's standing slot earlier in the order needed.
func FuzzAllocate_standingFromLeftover(f *testing.F) {
	allocSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		l, now := decodeLedger(data)
		a := l.allocate(now)
		end := slices.Clone(a.free)
		for i, b := range l.books {
			for _, h := range append(slices.Clone(a.grants[i]), a.standing[i]) {
				if h >= 0 {
					end[h] = end[h].minus(l.slot(b.profile, h))
				}
			}
		}
		jobs, at, covered := funding(l, &a, now)
		order := l.standOrder()
		for pos, i := range order {
			h := a.standing[i]
			if h < 0 {
				continue
			}
			left := slices.Clone(end)
			left[h] = left[h].plus(l.slot(l.books[i].profile, h))
			for k, j := range jobs {
				b := l.books[j.book]
				if at[k] < 0 && !covered[k] && l.profiles[b.profile].max == 0 && l.usable(b.profile, h) && !a.held[h] && l.slot(b.profile, h).within(left[h]) {
					t.Fatalf("book %d's standing slot on host %d takes room book %d's waiting job fits", i, h, j.book)
				}
			}
			for _, e := range order[:pos] {
				o := l.books[e]
				if !o.org || !o.fundable || a.standing[e] >= 0 || l.profiles[o.profile].max > 0 || l.covered(o) > 0 {
					continue
				}
				if l.usable(o.profile, h) && l.slot(o.profile, h).within(left[h]) {
					t.Fatalf("book %d's standing slot on host %d takes room organization book %d's, earlier, fits", i, h, e)
				}
			}
		}
	})
}
