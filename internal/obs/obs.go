// Package obs owns the control plane's Prometheus series: the events the
// pool and the GitHub client record, seeded for every label set the
// current settings allow, and the read series built from the pool's
// snapshot at each scrape, which come and go with the settings and the
// hosts. Every label value is a settings name, one of this module's closed
// sets, or a host's probe check or daemon call name.
package obs

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/chartdata"
	"github.com/cplieger/go/measure/v2"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/pool"
)

// The start results.
const (
	startOK    = "ok"
	startError = "error"
)

// The settings' label names.
const (
	labelHost    = "host"
	labelScope   = "scope"
	labelProfile = "profile"
)

// sevenDays is the last histogram edge: the ladder's first at or above
// both the longest a job may run (max_lifetime's 5-day cap, GitHub's limit
// for a self-hosted runner's job) and the longest it may wait (GitHub
// cancels a job queued for 24 hours), so no recorded value is past it.
const sevenDays = 7 * 24 * 3600

// durationEdges is chartdata's ladder up to sevenDays, so a sampled
// frame lands on the edges the store's bins use.
func durationEdges() []float64 {
	e := chartdata.DurationEdges()
	return e[:slices.Index(e, sevenDays)+1]
}

// Metrics is the control plane's registry. It implements pool.Recorder and
// github.Recorder; its methods are safe for concurrent use.
type Metrics struct {
	d descs
	// allowed is the label sets the last generation's settings allow; an
	// event outside them is dropped, so no gone host, scope or profile
	// comes back after its series was deleted. mu guards it and orders
	// each event against a generation's seeds and deletes.
	allowed     labelSets
	exits       *measure.LabeledCounter
	restReads   *measure.LabeledCounter
	githubErrs  *measure.LabeledCounter
	starts      *measure.LabeledCounter
	reg         *measure.Registry
	linkErrs    *measure.LabeledCounter
	jobQueue    *measure.LabeledHistogram
	jobDuration *measure.LabeledHistogram
	applied     *measure.Gauge
	// src is nil until Ready.
	src atomic.Pointer[Sources]
	log *slog.Logger
	mu  sync.Mutex
	// sizeFailing and freeFailing hold a failing store read, warned once.
	sizeFailing, freeFailing atomic.Bool
}

// Sources are what the read series read at each scrape; every field is
// required.
type Sources struct {
	// Pool answers the pool's snapshot.
	Pool func() pool.Snapshot
	// Store is the control plane's store.
	Store Store
	// Housekeeping answers when the housekeeper's next finished run is
	// due, zero before it runs.
	Housekeeping func() time.Time
}

// Store is the store's usage as the series read it.
type Store interface {
	Size() (int64, error)
	VolumeFree() (uint64, error)
	WriteFailures() uint64
}

var (
	_ pool.Recorder   = (*Metrics)(nil)
	_ github.Recorder = (*Metrics)(nil)
)

// New registers every series. version is the build's, and log,
// slog.Default() when nil, takes measure's warnings and a failing store
// read's. Until Ready, the read series are manade_booting at 1 alone.
func New(version string, log *slog.Logger) (*Metrics, error) {
	log = cmp.Or(log, slog.Default())
	reg := measure.NewRegistry("manade", measure.WithLogger(log))
	hsp := []string{labelHost, labelScope, labelProfile}
	m := &Metrics{
		reg: reg, log: log, allowed: labelSets{runners: map[hostSet]bool{}, profiles: map[string]bool{}, hosts: map[string]bool{}},
		applied: measure.NewGauge("generation_applied_timestamp_seconds",
			"When the control plane last built a pool generation from the settings, 0 before the first.", measure.UnitSeconds),
		restReads: measure.NewLabeledCounter("rest_reads_total",
			"Queued-jobs list reads by result; unchanged is a 304, which costs no primary rate limit.", measure.UnitCount,
			[]string{"result"}, measure.WithSeed(seeds(github.ReadResults)...)),
		githubErrs: measure.NewLabeledCounter("github_errors_total", "Failed GitHub calls by operation.", measure.UnitCount,
			[]string{"op"}, measure.WithSeed(seeds(github.Ops)...)),
		starts: measure.NewLabeledCounter("runner_starts_total",
			"Runner start attempts by result; a create the host refused or failed is an error.", measure.UnitCount,
			append(slices.Clone(hsp), "result")),
		exits: measure.NewLabeledCounter("runner_exits_total", "How runners' records closed, by reason.", measure.UnitCount,
			append(slices.Clone(hsp), "reason")),
		linkErrs: measure.NewLabeledCounter("host_link_errors_total",
			"Failed calls to a connected host's API and broken event streams.", measure.UnitCount, []string{labelHost}),
		jobQueue: measure.NewLabeledHistogram("job_queue_seconds", "Time from a job's queueing to its runner's assignment.",
			measure.UnitSeconds, []string{labelProfile}, measure.WithBuckets(durationEdges())),
		jobDuration: measure.NewLabeledHistogram("job_duration_seconds",
			"Time from a job's runner assignment to its end, by result.", measure.UnitSeconds, []string{labelProfile, "result"},
			measure.WithBuckets(durationEdges())),
		d: newDescs(),
	}
	build := measure.NewInfo("build_info", "The control plane's build version.", []string{"version"})
	build.Set(version)
	for _, x := range []measure.Metric{
		build, m.applied, m.restReads, m.githubErrs, m.starts, m.exits, m.linkErrs, m.jobQueue, m.jobDuration,
		measure.NewCollector(m.collect, m.d.all()...),
	} {
		if err := reg.Register(x); err != nil {
			return nil, fmt.Errorf("obs: %w", err)
		}
	}
	return m, nil
}

// Ready hands the read series their sources, read at each scrape from
// then on.
func (m *Metrics) Ready(src *Sources) { m.src.Store(src) }

func seeds[T ~string](vals []T) [][]string {
	out := make([][]string, len(vals))
	for i, v := range vals {
		out[i] = []string{string(v)}
	}
	return out
}

// Handler serves the registry's page, the Go and process families included.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }

// hostSet is one host, scope and profile a runner can have.
type hostSet struct{ host, scope, profile string }

// labelSets is what one generation's settings allow the recorded series.
type labelSets struct {
	runners         map[hostSet]bool
	profiles, hosts map[string]bool
}

// setsOf answers cfg's label sets: each host a profile lists, in each
// scope the profile serves, a scope it is leaving included.
func setsOf(cfg *config.Config) labelSets {
	s := labelSets{runners: map[hostSet]bool{}, profiles: map[string]bool{}, hosts: map[string]bool{}}
	for i := range cfg.Hosts {
		s.hosts[cfg.Hosts[i].Name] = true
	}
	for i := range cfg.Profiles {
		p := &cfg.Profiles[i]
		s.profiles[p.Name] = true
		for j := range cfg.Scopes {
			if !p.Serves(cfg.Scopes[j].Key) {
				continue
			}
			for _, h := range p.Hosts {
				s.runners[hostSet{h, cfg.Scopes[j].Key, p.Name}] = true
			}
		}
	}
	return s
}

// Generation seeds at 0 each label set cfg allows that the last settings
// did not, and deletes each one they allowed that cfg does not.
func (m *Metrics) Generation(cfg *config.Config, at time.Time) {
	next := setsOf(cfg)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.followRunners(next.runners)
	m.followProfiles(next.profiles)
	for h := range next.hosts {
		m.linkErrs.Add(0, h)
	}
	for h := range m.allowed.hosts {
		if !next.hosts[h] {
			m.linkErrs.Delete(h)
		}
	}
	m.allowed = next
	m.applied.Set(unixSeconds(at))
}

// followRunners seeds the start and exit series next adds and deletes
// the ones it drops; m.mu is held.
func (m *Metrics) followRunners(next map[hostSet]bool) {
	for k := range next {
		if !m.allowed.runners[k] {
			m.runnerSeries(k, func(c *measure.LabeledCounter, labels ...string) { c.Add(0, labels...) })
		}
	}
	for k := range m.allowed.runners {
		if !next[k] {
			m.runnerSeries(k, (*measure.LabeledCounter).Delete)
		}
	}
}

// followProfiles seeds the job histograms of next's profiles and deletes
// the ones it drops; m.mu is held.
func (m *Metrics) followProfiles(next map[string]bool) {
	for p := range next {
		m.jobQueue.Seed(p)
		for _, r := range pool.JobResults {
			m.jobDuration.Seed(p, string(r))
		}
	}
	for p := range m.allowed.profiles {
		if !next[p] {
			m.jobQueue.Delete(p)
			for _, r := range pool.JobResults {
				m.jobDuration.Delete(p, string(r))
			}
		}
	}
}

// runnerSeries calls fn on each start and exit series of k.
func (m *Metrics) runnerSeries(k hostSet, fn func(c *measure.LabeledCounter, labels ...string)) {
	fn(m.starts, k.host, k.scope, k.profile, startOK)
	fn(m.starts, k.host, k.scope, k.profile, startError)
	for _, r := range pool.ExitReasons {
		fn(m.exits, k.host, k.scope, k.profile, string(r))
	}
}

// RunnerStarted counts one start attempt of ss on host.
func (m *Metrics) RunnerStarted(host string, ss github.ScaleSet, ok bool) {
	result := startError
	if ok {
		result = startOK
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed.runners[hostSet{host, ss.Scope, ss.Profile}] {
		m.starts.Inc(host, ss.Scope, ss.Profile, result)
	}
}

// RunnerExited counts one runner record of ss on host closing for reason.
func (m *Metrics) RunnerExited(host string, ss github.ScaleSet, reason pool.ExitReason) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed.runners[hostSet{host, ss.Scope, ss.Profile}] {
		m.exits.Inc(host, ss.Scope, ss.Profile, string(reason))
	}
}

// JobAssigned observes how long a job of profile waited for its runner.
func (m *Metrics) JobAssigned(profile string, waited time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed.profiles[profile] {
		m.jobQueue.Observe(waited.Seconds(), profile)
	}
}

// JobFinished observes how long a job of profile ran, by its result.
func (m *Metrics) JobFinished(profile string, r pool.JobResult, ran time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed.profiles[profile] {
		m.jobDuration.Observe(ran.Seconds(), profile, string(r))
	}
}

// HostLinkError counts one failed call to host's API or broken events
// stream.
func (m *Metrics) HostLinkError(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.allowed.hosts[host] {
		m.linkErrs.Inc(host)
	}
}

// GitHubError counts one failed GitHub call.
func (m *Metrics) GitHubError(op github.Op) { m.githubErrs.Inc(string(op)) }

// QueueRead counts one queued-jobs list read.
func (m *Metrics) QueueRead(r github.ReadResult) { m.restReads.Inc(string(r)) }

// unixSeconds is t as a timestamp series' value, 0 for the zero time.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixMilli()) / 1000
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
