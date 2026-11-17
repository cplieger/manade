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
	OpSession      Op = "session"
	OpPoll         Op = "poll"
	OpJIT          Op = "jit"
	OpScaleSet     Op = "scaleset"
	OpVisibility   Op = "visibility"
	OpInstallation Op = "installation"
	OpQueue        Op = "queue"
	OpVariable     Op = "variable"
)

// Ops is every Op.
var Ops = []Op{OpSession, OpPoll, OpJIT, OpScaleSet, OpVisibility, OpInstallation, OpQueue, OpVariable}

// ReadResult is one queued-jobs read's outcome. ReadUnchanged is a 304,
// which costs no primary rate limit.
type ReadResult string

// The read outcomes.
const (
	ReadChanged   ReadResult = "changed"
	ReadUnchanged ReadResult = "unchanged"
	ReadError     ReadResult = "error"
)

// ReadResults is every ReadResult.
var ReadResults = []ReadResult{ReadChanged, ReadUnchanged, ReadError}
