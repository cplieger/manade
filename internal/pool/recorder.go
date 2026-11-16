package pool

import (
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// ExitReason is how a runner's record closed.
type ExitReason string

// The exit reasons.
const (
	ExitCompleted ExitReason = "completed"
	ExitFailed    ExitReason = "failed"
	ExitOOM       ExitReason = "oom"
	// ExitLost is a runner whose container ended while it held a job, with
	// no JobCompleted.
	ExitLost ExitReason = "lost"
	// ExitRemoved is a container removed by its host or by someone else,
	// never counted as the runner's own error.
	ExitRemoved  ExitReason = "removed"
	ExitIdle     ExitReason = "idle"
	ExitLifetime ExitReason = "lifetime"
)

// JobResult is a JobCompleted's result.
type JobResult string

// The job results.
const (
	JobSucceeded JobResult = "succeeded"
	JobFailed    JobResult = "failed"
	JobCanceled  JobResult = "canceled"
	JobOther     JobResult = "other"
)

// Recorder takes the pool's measurements; host, scope and profile are the
// settings' names. Its methods must be safe for concurrent use and return
// at once.
type Recorder interface {
	ScopeServing(scope string, serving bool)
	RunnerStarted(host string, ss github.ScaleSet, ok bool)
	RunnerExited(host string, ss github.ScaleSet, reason ExitReason)
	JobAssigned(profile string, waited time.Duration)
	JobFinished(profile string, r JobResult, ran time.Duration)
	// HostStatus takes each status a pass read, nil while the host is
	// disconnected or does not answer.
	HostStatus(host string, st *hostapi.Status)
	HostServing(host string, serving bool)
	// HostProved takes a proof's report at the time it ended; a nil report
	// is a proof that failed before the probe ran.
	HostProved(host string, at time.Time, rep *hostapi.ProbeReport, ok bool)
	HostLinkError(host string)
	Pass(ok bool)
	// Generation takes the settings revision of each generation that
	// begins serving.
	Generation(revision uint64)
}
