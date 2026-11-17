package store

import (
	"time"

	"github.com/cplieger/bolttable"
)

// Entry is one journal row: a change to this file's state, an
// operator's or the pool's own, written in that change's transaction.
// Action and Outcome are codes and Facts their values, which the client
// words; a row holds no sentence.
type Entry struct {
	At      time.Time         `json:"at"`
	Facts   map[string]string `json:"facts,omitempty"`
	Actor   Actor             `json:"actor"`
	Action  string            `json:"action"`
	Subject string            `json:"subject"`
	Outcome string            `json:"outcome"`
}

// Actor is who made a change: the pool, or a session or key by its ID.
type Actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// ActorPool is the pool acting on its own.
const ActorPool = "pool"

// The journal's actions.
const (
	// ActionScaleSetRecordDropped is a scale set GitHub no longer has,
	// whose record the pool dropped: subject the scope, facts profile and
	// scale_set_id.
	ActionScaleSetRecordDropped = "scale_set_record_dropped"
)

// The journal's outcomes.
const (
	OutcomeDone = "done"
)

// write appends e to the journal in tx.
func write(tx *bolttable.Tx, e *Entry) error {
	id, err := journal.NextID(tx)
	if err != nil {
		return err
	}
	return journal.Put(tx, id, e)
}
