package pool

import (
	"strings"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// Stop is one stopping state of the settings (§9.4) as the pool reads it:
// a scope turned off (Profile empty) or a profile leaving a scope, with its
// scale sets' queued and assigned jobs, and for a repository scope the
// queued jobs its last read named, which Stop now leaves queued. Ended
// reports that every one of them has run, read from statistics GitHub sent
// and a queue read that both came after this generation was built.
type Stop struct {
	Scope, Profile   string
	Jobs             []github.QueuedJob
	Queued, Assigned int
	Ended            bool
}

// settler ends the stopping states the pool read as ended, from settings
// revision rev: it writes the scope off, clears the profile's retiring
// entry and deletes a profile whose last entry cleared, each with its
// journal row, and hands the next revision to Registry.Apply. Settle must
// return at once and owns retrying a commit that fails: a generation hands
// each stop it read ended once. A stop of a revision older than the
// settings in force is dropped, since the generation serving those reads
// its counts again and hands it once more.
type settler interface {
	Settle(rev uint64, ended []Stop)
}

// stops reads every stopping state of the generation's settings; g.mu is
// held.
func (g *generation) stops() []Stop {
	var out []Stop
	for i := range g.cfg.Scopes {
		sc := &g.cfg.Scopes[i]
		if !sc.Stopping {
			continue
		}
		st := Stop{Scope: sc.Key, Ended: true}
		for j := range g.cfg.Profiles {
			if pr := &g.cfg.Profiles[j]; pr.Serves(sc.Key) {
				g.count(&st, sc.Key, pr.Name)
			}
		}
		out = append(out, st)
	}
	for j := range g.cfg.Profiles {
		pr := &g.cfg.Profiles[j]
		for _, key := range pr.Retiring {
			st := Stop{Scope: key, Profile: pr.Name, Ended: true}
			g.count(&st, key, pr.Name)
			out = append(out, st)
		}
	}
	return out
}

// count adds the scale set of profile in the scope with key to st: its
// jobs, and whether they have all run, read after the scope's routing
// variable settled in this generation, so no job routed before the
// variable changed is missed. A scope the settings no longer hold
// or that is public leaves the scale set nothing the pool can run; one
// with no scale set recorded yet cannot be read, so it has not ended.
func (g *generation) count(st *Stop, key, profile string) {
	sc := g.scopeOf(key)
	switch {
	case sc == nil || sc.public:
		return
	case g.bookOf(sc.cfg.Key, profile) == nil:
		st.Ended = false
		return
	}
	b := g.bookOf(sc.cfg.Key, profile)
	stats := b.asg.stats
	queued := stats.Available
	if sc.cfg.Kind == config.ScopeRepo {
		queued = max(queued, len(sc.jobsOf(profile)))
	}
	st.Queued += queued
	st.Assigned += stats.Assigned
	st.Jobs = append(st.Jobs, sc.jobsOf(profile)...)
	since := sc.routed
	fresh := !since.IsZero() && b.asg.at.After(since) && (sc.cfg.Kind != config.ScopeRepo || sc.readAt.After(since) && !sc.partial)
	st.Ended = st.Ended && fresh && queued == 0 && stats.Assigned == 0
}

// scopeOf finds the scope with key, compared without case as GitHub does.
func (g *generation) scopeOf(key string) *scope {
	for k, sc := range g.scopes {
		if strings.EqualFold(k, key) {
			return sc
		}
	}
	return nil
}

// settingsOf answers the settings of the scope with key: the generation's,
// a dropped scope's it still keeps, or else the scope the key names.
func (g *generation) settingsOf(key string) config.Scope {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sc := g.scopeOf(key); sc != nil {
		return *sc.cfg
	}
	for k, sc := range g.gone {
		if strings.EqualFold(k, key) {
			return *sc
		}
	}
	return config.ScopeOf(key)
}

// settle hands the settler each stop that ended and that this generation
// has not handed it before.
func (g *generation) settle() {
	g.mu.Lock()
	var ended []Stop
	for _, st := range g.stops() {
		k := setKey{st.Scope, st.Profile}
		if st.Ended && !g.settled[k] {
			g.settled[k] = true
			ended = append(ended, st)
		}
	}
	rev := g.rev
	g.mu.Unlock()
	if len(ended) > 0 {
		g.log.Info("Stopping states ended with every job they held run", "count", len(ended), "revision", rev)
		g.d.Settle.Settle(rev, ended)
	}
}
