// Package obs owns manade's Prometheus series, served by Handler, and the
// pass record, the INFO line the LogQL bundle reads where no one scrapes.
//
// Every label value comes from a closed set: scope and profile from the
// config, every other label from one of this package's typed vocabularies.
// Recording a scale set the config does not define is the caller's bug: it
// makes a series no seed covers.
package obs

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/measure"
	"github.com/cplieger/manade/internal/r5/config"
)

// The label vocabularies.
type (
	// ReadResult is the outcome of one queued-jobs read.
	ReadResult string
	// ExitReason is how a runner ended.
	ExitReason string
	// JobResult is a finished job's conclusion.
	JobResult string
	// GitHubOp names the GitHub call that failed.
	GitHubOp string
	// DockerOp names the Docker call that failed.
	DockerOp string
)

// The vocabularies' values. ReadUnchanged is a 304, which costs no primary
// rate limit.
const (
	ReadChanged   ReadResult = "changed"
	ReadUnchanged ReadResult = "unchanged"
	ReadError     ReadResult = "error"

	ExitCompleted ExitReason = "completed"
	ExitFailed    ExitReason = "failed"
	ExitOOM       ExitReason = "oom"
	ExitLifetime  ExitReason = "lifetime"
	ExitIdle      ExitReason = "idle"
	ExitLost      ExitReason = "lost"

	JobSucceeded JobResult = "succeeded"
	JobFailed    JobResult = "failed"
	JobCanceled  JobResult = "canceled"
	JobOther     JobResult = "other"

	GitHubSession      GitHubOp = "session"
	GitHubPoll         GitHubOp = "poll"
	GitHubJIT          GitHubOp = "jit"
	GitHubScaleSet     GitHubOp = "scaleset"
	GitHubVisibility   GitHubOp = "visibility"
	GitHubInstallation GitHubOp = "installation"
	GitHubQueue        GitHubOp = "queue"

	DockerPull    DockerOp = "pull"
	DockerCreate  DockerOp = "create"
	DockerStart   DockerOp = "start"
	DockerInspect DockerOp = "inspect"
	DockerRemove  DockerOp = "remove"
	DockerList    DockerOp = "list"
	DockerEvents  DockerOp = "events"
	DockerArchive DockerOp = "archive"
)

// Each list seeds its counter at 0, so the first failure shows in increase().
var (
	readResults = []ReadResult{ReadChanged, ReadUnchanged, ReadError}
	exitReasons = []ExitReason{ExitCompleted, ExitFailed, ExitOOM, ExitLifetime, ExitIdle, ExitLost}
	githubOps   = []GitHubOp{
		GitHubSession, GitHubPoll, GitHubJIT, GitHubScaleSet, GitHubVisibility, GitHubInstallation, GitHubQueue,
	}
	dockerOps = []DockerOp{
		DockerPull, DockerCreate, DockerStart, DockerInspect, DockerRemove, DockerList, DockerEvents, DockerArchive,
	}
)

// A queue wait runs from seconds on a standing slot to GitHub's 24-hour
// queue limit for self-hosted runners, and a job up to its 6-hour default
// timeout:
// https://docs.github.com/en/actions/reference/limits
// https://docs.github.com/en/actions/writing-workflows/workflow-syntax-for-github-actions#jobsjob_idtimeout-minutes
var (
	queueBuckets    = []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 86400}
	durationBuckets = []float64{30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 21600}
)

const (
	scopeLabel   = "scope"
	profileLabel = "profile"
	resultLabel  = "result"
)

// ScaleSet names one scale set: a profile in a scope, both as the config
// spells them.
type ScaleSet struct{ Scope, Profile string }

// Metrics records manade's series. Its methods are safe for concurrent use.
type Metrics struct {
	now         func() time.Time
	started     time.Time
	log         *slog.Logger
	reg         *measure.Registry
	src         atomic.Pointer[Source]
	restReads   *measure.LabeledCounter
	starts      *measure.LabeledCounter
	exits       *measure.LabeledCounter
	githubErrs  *measure.LabeledCounter
	dockerErrs  *measure.LabeledCounter
	queueRead   *measure.LabeledGauge
	poll        *measure.LabeledGauge
	serving     *measure.LabeledGauge
	orphaned    *measure.LabeledGauge
	imageCheck  *measure.LabeledGauge
	reconcile   *measure.Gauge
	booting     *measure.Gauge
	jobQueue    *measure.LabeledHistogram
	jobDuration *measure.LabeledHistogram
	d           descs
	last        totals
	passes      int
	passMu      sync.Mutex
}

// New registers every series for cfg's scopes and profiles and seeds them.
// The freshness timestamps start at the call's time, so a loop that never
// succeeds reads as stale from boot, and boot runs from it until Booted.
// log is the registry's and the records' logger, slog.Default() when nil.
// Each line it writes carries the pool's POOL_NAME as pool, a contract with
// alerts/logql.yaml bound by TestAlertParityLogQLReadsTheRecords.
func New(cfg *config.Config, log *slog.Logger) (*Metrics, error) { return newAt(cfg, log, time.Now) }

func newAt(cfg *config.Config, log *slog.Logger, now func() time.Time) (*Metrics, error) {
	m := &Metrics{now: now, started: now(), log: cmp.Or(log, slog.Default()).With("pool", cfg.PoolName)}
	m.reg = measure.NewRegistry("manade", measure.WithLogger(m.log))
	scopeProfile := []string{scopeLabel, profileLabel}
	capCPUs := measure.NewGauge("capacity_cpus", "The CPU budget.")
	capMemory := measure.NewGauge("capacity_memory_bytes", "The memory budget.")
	elevated := measure.NewLabeledGauge("profile_elevated",
		"1 for each elevated field a profile sets.", []string{profileLabel, "grant"})
	m.restReads = measure.NewLabeledCounter("rest_reads_total",
		"Queued-jobs reads by result; unchanged is a 304, which costs no primary rate limit.", []string{resultLabel})
	m.queueRead = measure.NewLabeledGauge("queue_read_last_success_timestamp_seconds",
		"End of the last sweep that read all of a repo scope's lists (process start until the first).", []string{scopeLabel})
	m.starts = measure.NewLabeledCounter("runner_starts_total",
		"Runner start attempts by result.", []string{scopeLabel, profileLabel, resultLabel})
	m.exits = measure.NewLabeledCounter("runner_exits_total",
		"How runners ended, by reason.", []string{scopeLabel, profileLabel, "reason"})
	m.jobQueue = measure.NewLabeledHistogram("job_queue_seconds",
		"Time from a job's queueing to its runner's assignment.", []string{profileLabel}, measure.WithBuckets(queueBuckets))
	m.jobDuration = measure.NewLabeledHistogram("job_duration_seconds",
		"Time from a job's runner assignment to its end, by result.", []string{profileLabel, resultLabel},
		measure.WithBuckets(durationBuckets))
	m.poll = measure.NewLabeledGauge("poll_last_success_timestamp_seconds",
		"Last good poll per scale set (process start until the first).", scopeProfile)
	m.reconcile = measure.NewGauge("reconcile_last_success_timestamp_seconds",
		"Last good reconciler pass (process start until the first).")
	m.booting = measure.NewGauge("booting", "1 from the start until boot has finished, then 0.")
	m.serving = measure.NewLabeledGauge("scope_serving",
		"1 while the scope is served, 0 while it is stopped.", []string{scopeLabel})
	m.orphaned = measure.NewLabeledGauge("scale_sets_orphaned",
		"Scale sets in the scope's runner group that the config does not name, as of boot.", []string{scopeLabel})
	m.imageCheck = measure.NewLabeledGauge("image_checked_timestamp_seconds",
		"Last good check of a profile's image; 0 until the first.", []string{profileLabel})
	m.githubErrs = measure.NewLabeledCounter("github_errors_total", "GitHub API failures by operation.", []string{"op"})
	m.dockerErrs = measure.NewLabeledCounter("docker_errors_total", "Docker API failures by operation.", []string{"op"})

	all := []measure.Metric{
		capCPUs, capMemory, elevated, m.restReads, m.queueRead, m.starts, m.exits, m.jobQueue,
		m.jobDuration, m.poll, m.reconcile, m.booting, m.serving, m.orphaned, m.imageCheck, m.githubErrs, m.dockerErrs,
		m.d.collector(m.collect, cfg.Capacity.Disk > 0),
	}
	var capDisk *measure.Gauge
	if cfg.Capacity.Disk > 0 {
		capDisk = measure.NewGauge("capacity_disk_bytes", "The disk budget.")
		all = append(all, capDisk)
	}
	for _, x := range all {
		if err := m.reg.Register(x); err != nil {
			return nil, fmt.Errorf("obs: %w", err)
		}
	}

	capCPUs.Set(float64(cfg.Capacity.MilliCPUs) / 1000)
	capMemory.Set(float64(cfg.Capacity.Memory))
	if capDisk != nil {
		capDisk.Set(float64(cfg.Capacity.Disk))
	}
	for i := range cfg.Profiles {
		for _, g := range cfg.Profiles[i].Grants() {
			elevated.Set(1, cfg.Profiles[i].Name, g)
		}
	}
	for _, r := range readResults {
		m.restReads.Add(0, string(r))
	}
	for _, op := range githubOps {
		m.githubErrs.Add(0, string(op))
	}
	for _, op := range dockerOps {
		m.dockerErrs.Add(0, string(op))
	}
	m.seed(cfg, unixSeconds(m.started))
	return m, nil
}

// seed starts every freshness series at start, every image check at 0,
// every scope stopped until its first visibility proof and the boot running.
func (m *Metrics) seed(cfg *config.Config, start float64) {
	m.reconcile.Set(start)
	m.booting.Set(1)
	for i := range cfg.Profiles {
		m.imageCheck.Set(0, cfg.Profiles[i].Name)
	}
	for _, s := range cfg.Scopes {
		m.serving.Set(0, s.Key)
		m.orphaned.Set(0, s.Key)
		if s.Kind == config.ScopeRepo {
			m.queueRead.Set(start, s.Key)
		}
		for i := range cfg.Profiles {
			p := &cfg.Profiles[i]
			if !p.Allowed(s.Key) {
				continue
			}
			m.poll.Set(start, s.Key, p.Name)
			m.starts.Add(0, s.Key, p.Name, "ok")
			m.starts.Add(0, s.Key, p.Name, "error")
			for _, r := range exitReasons {
				m.exits.Add(0, s.Key, p.Name, string(r))
			}
		}
	}
}

// Handler serves the registry's page, the Go and process families included.
func (m *Metrics) Handler() http.Handler { return m.reg.Handler() }

// PollOK records a good poll of ss at now.
func (m *Metrics) PollOK(ss ScaleSet, now time.Time) {
	m.poll.Set(unixSeconds(now), ss.Scope, ss.Profile)
}

// QueueRead counts one queued-jobs read.
func (m *Metrics) QueueRead(r ReadResult) { m.restReads.Inc(string(r)) }

// QueueScopeRead records the end, at now, of a sweep that read all of a repo
// scope's lists.
func (m *Metrics) QueueScopeRead(scope string, now time.Time) {
	m.queueRead.Set(unixSeconds(now), scope)
}

// ScopeServing records whether scope is served.
func (m *Metrics) ScopeServing(scope string, serving bool) {
	v := 0.0
	if serving {
		v = 1
	}
	m.serving.Set(v, scope)
}

// Orphaned records how many of scope's scale sets the config does not name.
func (m *Metrics) Orphaned(scope string, n int) { m.orphaned.Set(float64(n), scope) }

// RunnerStarted counts one start attempt for ss.
func (m *Metrics) RunnerStarted(ss ScaleSet, ok bool) {
	result := "error"
	if ok {
		result = "ok"
	}
	m.starts.Inc(ss.Scope, ss.Profile, result)
}

// RunnerExited counts one runner of ss ending for reason.
func (m *Metrics) RunnerExited(ss ScaleSet, reason ExitReason) {
	m.exits.Inc(ss.Scope, ss.Profile, string(reason))
}

// JobAssigned observes how long a job of profile waited for its runner.
func (m *Metrics) JobAssigned(profile string, waited time.Duration) {
	m.jobQueue.Observe(waited.Seconds(), profile)
}

// JobFinished observes how long a job of profile ran, by its result.
func (m *Metrics) JobFinished(profile string, r JobResult, ran time.Duration) {
	m.jobDuration.Observe(ran.Seconds(), profile, string(r))
}

// ImageChecked records a good check, at now, of profile's image.
func (m *Metrics) ImageChecked(profile string, now time.Time) {
	m.imageCheck.Set(unixSeconds(now), profile)
}

// GitHubError counts one failed GitHub call.
func (m *Metrics) GitHubError(op GitHubOp) { m.githubErrs.Inc(string(op)) }

// DockerError counts one failed Docker call.
func (m *Metrics) DockerError(op DockerOp) { m.dockerErrs.Inc(string(op)) }

func unixSeconds(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }
