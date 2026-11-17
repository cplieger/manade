package history

import (
	"time"

	"github.com/cplieger/go/chartdata"
	"github.com/cplieger/manade/internal/store"
)

// longest is the duration ladder's last edge a run's wait or duration can
// reach: a job runs at most 5 days and waits at most 24 hours (§12.1).
const longest = 7 * 24 * time.Hour

// DurationEdges is the histogram prefix of chartdata's duration ladder, in
// seconds, up to longest: a rollup's bins and the wait and duration
// histograms share it.
func DurationEdges() []float64 {
	edges := chartdata.DurationEdges()
	for i, e := range edges {
		if e >= longest.Seconds() {
			return edges[:i+1]
		}
	}
	return edges
}

// edges is DurationEdges, read once.
var edges = DurationEdges()

// bin answers the interval of edges d falls in, the last for anything at
// or past the last edge.
func bin(d time.Duration) int {
	s := d.Seconds()
	for i := 1; i < len(edges); i++ {
		if s < edges[i] {
			return i - 1
		}
	}
	return len(edges) - 2
}

// A run's rollup pieces, each added once its inputs are on the row.
const (
	pieceJob uint8 = 1 << iota
	pieceWait
	pieceResult
	pieceDuration
	pieceExit
)

// due answers the pieces whose inputs r holds.
func due(r *store.Run) uint8 {
	if r.EndedAt.IsZero() {
		return 0
	}
	p := pieceJob
	started := !r.StartedAt.IsZero()
	if started {
		p |= pieceWait
	}
	if r.Result != "" {
		p |= pieceResult
		if started {
			p |= pieceDuration
		}
	}
	if r.Exit != "" {
		p |= pieceExit
	}
	return p
}

// add adds r's pieces p to u.
func add(u *store.Rollup, r *store.Run, p uint8) {
	if p&pieceJob != 0 {
		u.Jobs++
	}
	if p&pieceWait != 0 {
		count(&u.Wait, time.Duration(r.WaitMS)*time.Millisecond)
	}
	if p&pieceResult != 0 {
		u.Results = inc(u.Results, r.Result)
	}
	if p&pieceDuration != 0 {
		count(&u.Duration, time.Duration(r.DurationMS)*time.Millisecond)
		u.RunnerMS += r.DurationMS
	}
	if p&pieceExit != 0 {
		u.Exits = inc(u.Exits, r.Exit)
		if r.Usage != nil {
			u.CPUMS += r.Usage.CPUMS
			u.PeakMemory = max(u.PeakMemory, r.Usage.PeakMemory)
		}
	}
}

func count(bins *[]int64, d time.Duration) {
	if len(*bins) == 0 {
		*bins = make([]int64, len(edges)-1)
	}
	(*bins)[bin(d)]++
}

func inc(m map[string]int64, k string) map[string]int64 {
	if m == nil {
		m = map[string]int64{}
	}
	m[k]++
	return m
}

// dayOf answers the first instant of the day t falls on in loc.
func dayOf(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// keyOf names the rollup r counts in.
func keyOf(r *store.Run) store.RollupKey {
	return store.RollupKey{Day: r.Day, Scope: r.Scope, Profile: r.Profile, Host: r.DayHost, Repository: r.Repository}
}

// rollIn adds r's pieces p to the rollup it counts in.
func rollIn(h *store.HistoryTx, r *store.Run, p uint8) error {
	k := keyOf(r)
	u, found, err := h.Rollup(&k)
	if err != nil {
		return err
	}
	if !found {
		loc, err := time.LoadLocation(r.DayZone)
		if err != nil {
			return err
		}
		u = store.Rollup{
			Zone: r.DayZone, Date: r.Day.In(loc).Format(time.DateOnly),
			Scope: r.Scope, Profile: r.Profile, Host: r.DayHost, Repo: r.Repository,
		}
	}
	add(&u, r, p)
	return h.PutRollup(&k, &u)
}

// cutZone answers the zone the rollups are cut in, UTC before any cut.
func cutZone(h *store.HistoryTx) (*time.Location, error) {
	c, found, err := h.Cut()
	if err != nil || !found {
		return time.UTC, err
	}
	return time.LoadLocation(c.Zone)
}

// roll adds the pieces of r its row has not counted to its rollup,
// setting the rollup it counts in at its first.
func roll(h *store.HistoryTx, r *store.Run) error {
	p := due(r) &^ r.Counted
	if p == 0 {
		return nil
	}
	if r.Counted == 0 {
		loc, err := cutZone(h)
		if err != nil {
			return err
		}
		place(r, loc)
	}
	r.Counted |= p
	return rollIn(h, r, p)
}

// place sets the rollup r counts in, cut in loc.
func place(r *store.Run, loc *time.Location) {
	r.Day, r.DayZone, r.DayHost = dayOf(r.EndedAt, loc), loc.String(), r.Host
}
