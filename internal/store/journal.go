package store

import (
	"time"

	"github.com/cplieger/bolttable"
)

// entry is one journal row: a change to this file's state, an
// operator's or the pool's own, written in that change's transaction.
// Action and Outcome are codes and Facts their values, which the client
// words; a row holds no sentence.
type entry struct {
	At      time.Time         `json:"at"`
	Facts   map[string]string `json:"facts,omitempty"`
	Actor   actor             `json:"actor"`
	Action  string            `json:"action"`
	Subject string            `json:"subject"`
	Outcome string            `json:"outcome"`
}

// actor is who made a change: the pool, or a session or key by its ID.
type actor struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// actorPool is the pool acting on its own.
const actorPool = "pool"

// The journal's actions.
const (
	// actionScaleSetRecordDropped is a scale set GitHub no longer has,
	// whose record the pool dropped: subject the scope, facts profile and
	// scale_set_id.
	actionScaleSetRecordDropped = "scale_set_record_dropped"
)

// The journal's outcomes.
const (
	outcomeDone = "done"
)

// write appends e to the journal in tx.
func write(tx *bolttable.Tx, e *entry) error {
	id, err := journal.NextID(tx)
	if err != nil {
		return err
	}
	return journal.Put(tx, id, e)
}
