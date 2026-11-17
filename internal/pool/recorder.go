package pool

import (
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
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
	// exitRemoved is a container removed by its host or by someone else,
	// never counted as the runner's own error.
	exitRemoved  ExitReason = "removed"
	exitIdle     ExitReason = "idle"
	exitLifetime ExitReason = "lifetime"
)

// ExitReasons is every ExitReason.
var ExitReasons = []ExitReason{ExitCompleted, ExitFailed, ExitOOM, ExitLost, exitRemoved, exitIdle, exitLifetime}

// JobResult is a JobCompleted's result.
type JobResult string

// The job results.
const (
	JobSucceeded JobResult = "succeeded"
	JobFailed    JobResult = "failed"
	jobCanceled  JobResult = "canceled"
	jobOther     JobResult = "other"
)

// JobResults is every JobResult.
var JobResults = []JobResult{JobSucceeded, JobFailed, jobCanceled, jobOther}

// Recorder takes the pool's events, the measurements a snapshot cannot
// read later; host, scope and profile are the settings' names. Its methods
// must be safe for concurrent use and return at once.
type Recorder interface {
	RunnerStarted(host string, ss github.ScaleSet, ok bool)
	RunnerExited(host string, ss github.ScaleSet, reason ExitReason)
	JobAssigned(profile string, waited time.Duration)
	JobFinished(profile string, r JobResult, ran time.Duration)
	HostLinkError(host string)
	// Generation takes each generation's settings before the generation
	// boots, so the events it records name what those settings hold.
	Generation(cfg *config.Config, at time.Time)
}
