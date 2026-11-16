package pool

import (
	"slices"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

var epoch = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

const gib = 1 << 30

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

// hostSpec and profileSpec build a test config: cpus whole, memory in GiB.
type hostSpec struct {
	name      string
	cpus, mem int64
	disk      int64
	max       int
}

type profileSpec struct {
	name      string
	hosts     []string
	cpus, mem int64
	disk      int64
	max       int
}

func testConfig(hosts []hostSpec, profiles []profileSpec) *config.Config {
	cfg := &config.Config{HeadReserveAfter: 2 * time.Minute}
	for _, h := range hosts {
		cfg.Hosts = append(cfg.Hosts, config.Host{Name: h.name, Capacity: config.Capacity{
			MilliCPUs: h.cpus * 1000, Memory: h.mem * gib, Disk: h.disk * gib, MaxRunners: h.max,
		}})
	}
	for _, p := range profiles {
		cfg.Profiles = append(cfg.Profiles, config.Profile{Name: p.name, Hosts: p.hosts, MilliCPUs: p.cpus * 1000, Memory: p.mem * gib, Disk: p.disk * gib, MaxRunners: p.max})
	}
	return cfg
}

// openLedger is a ledger for cfg whose every host answers, serves and lists
// every profile that names it ready.
func openLedger(cfg *config.Config) *ledger {
	l := newLedger(cfg)
	for h := range l.hosts {
		l.hosts[h].reachable, l.hosts[h].serving = true, true
	}
	for p := range l.profiles {
		for _, h := range l.profiles[p].hosts {
			l.hosts[h].ready[p] = true
		}
	}
	return l
}

// served adds a scale set of scope's profile that is served and fundable.
func served(l *ledger, scope, profile string, org bool) *book {
	b := l.addBook(github.ScaleSet{Scope: scope, Profile: profile, ID: len(l.books) + 1}, org)
	b.serve, b.fundable = true, true
	return b
}

func job(run int64, profile string, at time.Time) github.QueuedJob {
	return github.QueuedJob{RunID: run, Name: "job", Profile: profile, Created: at}
}

// pendOn answers b's reserved slots by host name.
func pendOn(cfg *config.Config, b *book) map[string]int {
	out := map[string]int{}
	for h, n := range b.pend {
		if n > 0 {
			out[cfg.Hosts[h].Name] = n
		}
	}
	return out
}

// reserving reports that a's head reserve holds a host for some book.
func reserving(a *allocation) bool { return slices.Contains(a.claimant, true) }
