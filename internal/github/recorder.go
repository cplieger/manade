package github

// Recorder takes the client's measurements. Its methods must be safe for
// concurrent use.
type Recorder interface {
	// GitHubError counts one failed call of op.
	GitHubError(op Op)
	// QueueRead counts one queued-jobs list read by its outcome.
	QueueRead(r ReadResult)
}

// Op names a GitHub call.
type Op string

// The calls a failure is counted against.
const (
	opSession      Op = "session"
	OpPoll         Op = "poll"
	opJIT          Op = "jit"
	opScaleSet     Op = "scaleset"
	opVisibility   Op = "visibility"
	opInstallation Op = "installation"
	opQueue        Op = "queue"
	opVariable     Op = "variable"
)

// Ops is every Op.
var Ops = []Op{opSession, OpPoll, opJIT, opScaleSet, opVisibility, opInstallation, opQueue, opVariable}

// ReadResult is one queued-jobs read's outcome. ReadUnchanged is a 304,
// which costs no primary rate limit.
type ReadResult string

// The read outcomes.
const (
	readChanged   ReadResult = "changed"
	ReadUnchanged ReadResult = "unchanged"
	readError     ReadResult = "error"
)

// ReadResults is every ReadResult.
var ReadResults = []ReadResult{readChanged, ReadUnchanged, readError}
