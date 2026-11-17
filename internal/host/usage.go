package host

import (
	"log/slog"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/hostapi"
)

// usageCgroups is the runners' own cgroups a runner's usage is read from.
type usageCgroups interface {
	WorkingSet(name string) (int64, bool, error)
	Ended(name string) (platform.RunnerEnd, bool, error)
	Remove(name string) error
	Names() ([]string, error)
}

var _ usageCgroups = (*platform.RunnerCgroups)(nil)

// sampleEvery is how often each running runner's working set is read; a
// var so tests can shorten it.
var sampleEvery = time.Second

// meter keeps each running runner's highest sampled working set and reads
// an ended one's usage. Its methods are safe for concurrent use.
type meter struct {
	cg  usageCgroups
	log *slog.Logger
	// peaks is each sampled runner's highest working set; warned names the
	// runners whose cgroup failed a read or a removal, warned of once.
	peaks  map[string]int64
	warned map[warning]bool
	mu     sync.Mutex
}

// warning is one runner's failure of one kind.
type warning struct{ name, msg string }

func newMeter(cg usageCgroups, log *slog.Logger) *meter {
	return &meter{cg: cg, log: log, peaks: map[string]int64{}, warned: map[warning]bool{}}
}

// sample reads the working set of each runner of names.
func (m *meter) sample(names []string) {
	for _, n := range names {
		ws, found, err := m.cg.WorkingSet(n)
		if err != nil {
			m.warnOnce(n, "Reading a runner's memory failed, so its peak may miss what it used meanwhile", err)
			continue
		}
		if !found {
			continue
		}
		m.mu.Lock()
		if p, ok := m.peaks[n]; !ok || ws > p {
			m.peaks[n] = ws
		}
		m.mu.Unlock()
	}
}

// ended answers the usage of c, an ended container that ran, nil when its
// runner has no cgroup to read. The watermark counts page cache, so it
// stands in for the samples only where it is the answer: an OOM kill,
// whose need reached the limit, or a runner never sampled.
func (m *meter) ended(c *hostapi.Container) *hostapi.Usage {
	e, found, err := m.cg.Ended(c.Name)
	if err != nil {
		m.warnOnce(c.Name, "Reading an ended runner's cgroup failed, so its run records no usage", err)
		return nil
	}
	if !found {
		return nil
	}
	m.mu.Lock()
	peak, sampled := m.peaks[c.Name]
	m.mu.Unlock()
	if (c.OOMKilled || !sampled) && e.Watermark > 0 {
		peak = e.Watermark
	}
	return &hostapi.Usage{CPU: e.CPU, Ran: max(0, c.FinishedAt.Sub(c.StartedAt)), PeakMemory: peak}
}

// forget removes runner name's cgroup and its samples; a cgroup left is
// the next sweep's.
func (m *meter) forget(name string) {
	m.mu.Lock()
	delete(m.peaks, name)
	m.mu.Unlock()
	m.remove(name)
}

// sweep removes the cgroup of each runner held does not report.
func (m *meter) sweep(held func(name string) bool) {
	names, err := m.cg.Names()
	if err != nil {
		m.warnOnce("", "Listing the runners' cgroups failed, so none left over is removed", err)
		return
	}
	for _, n := range names {
		if !held(n) {
			m.forget(n)
		}
	}
}

func (m *meter) remove(name string) {
	if err := m.cg.Remove(name); err != nil {
		m.warnOnce(name, "Removing a runner's cgroup failed; it is tried again every list", err)
		return
	}
	m.mu.Lock()
	for w := range m.warned {
		if w.name == name {
			delete(m.warned, w)
		}
	}
	m.mu.Unlock()
}

// warnOnce warns msg of runner name until a removal of its cgroup ends it.
func (m *meter) warnOnce(name, msg string, err error) {
	w := warning{name: name, msg: msg}
	m.mu.Lock()
	seen := m.warned[w]
	m.warned[w] = true
	m.mu.Unlock()
	if !seen {
		m.log.Warn(msg, "runner", name, "error", err)
	}
}
