package pool

import (
	"context"
	"time"

	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
)

// Runs takes what the run history records of each job. Its
// methods are safe for concurrent use.
type Runs interface {
	// Job takes one job message of scale set ss; host holds the message's
	// runner, empty when no record holds it. An assignment carries the
	// reason its job waited under longest while queued, empty when no pass
	// or read saw it waiting. It returns at once.
	Job(ss github.ScaleSet, host string, e *github.Event, waited JobReason)
	// Exited takes a runner's end, which may write before it returns.
	Exited(ctx context.Context, x *RunnerExit)
	// Listed takes the first runner list host answered in this process,
	// which may write before it returns.
	Listed(ctx context.Context, host string, runners []string)
}

// RunnerExit is how a runner that may have held a job ended. Log and Diag
// are an unclean exit's log tail and _diag tar as its host answered them,
// nil when the host held no container or answered none; Usage is what the
// host reported the runner used, nil when it reported none.
type RunnerExit struct {
	At     time.Time
	Code   *int
	Usage  *hostapi.Usage
	Host   string
	Runner string
	Reason ExitReason
	Log    []byte
	Diag   []byte
}

// unclean reports whether an exit keeps the runner's evidence.
func unclean(reason ExitReason) bool {
	return reason == ExitFailed || reason == ExitOOM || reason == ExitLost
}

// evidence is an exited runner's code and usage and, for an unclean exit,
// its log tail and _diag tar.
type evidence struct {
	code      *int
	usage     *hostapi.Usage
	log, diag []byte
}
