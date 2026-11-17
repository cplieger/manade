package github

import "time"

// Recorder takes the client's measurements. Its methods must be safe for
// concurrent use.
type Recorder interface {
	// GitHubError counts one failed call of op.
	GitHubError(op Op)
	// QueueRead counts one queued-jobs list read by its outcome.
	QueueRead(r ReadResult)
	// QueueScopeRead stamps a sweep that read all of scope's lists.
	QueueScopeRead(scope string, at time.Time)
	// PollOK stamps a successful poll of ss.
	PollOK(ss ScaleSet, at time.Time)
	// Orphaned sets the scale sets in scope's runner group no profile names.
	Orphaned(scope string, n int)
}

// Op names a GitHub call.
type Op string

// The calls a failure is counted against.
const (
	OpSession      Op = "session"
	OpPoll         Op = "poll"
	OpJIT          Op = "jit"
	OpScaleSet     Op = "scaleset"
	OpVisibility   Op = "visibility"
	OpInstallation Op = "installation"
	OpQueue        Op = "queue"
	OpVariable     Op = "variable"
)

// ReadResult is one queued-jobs read's outcome. ReadUnchanged is a 304,
// which costs no primary rate limit.
type ReadResult string

// The read outcomes.
const (
	ReadChanged   ReadResult = "changed"
	ReadUnchanged ReadResult = "unchanged"
	ReadError     ReadResult = "error"
)
