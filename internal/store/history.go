package store

import (
	"context"
	"encoding/binary"
	"time"

	"github.com/cplieger/bolttable"
)

// Run is one job run's row, keyed by GitHub's runner request ID,
// unique per job attempt. A zero time is one not recorded yet.
type Run struct {
	QueuedAt   time.Time `json:"queued_at,omitzero"`
	AssignedAt time.Time `json:"assigned_at,omitzero"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	ExitedAt   time.Time `json:"exited_at,omitzero"`
	// EndedAt is the first of FinishedAt and ExitedAt recorded, zero while
	// the run is open.
	EndedAt time.Time `json:"ended_at,omitzero"`
	// SeenAt is when the first message naming the run arrived.
	SeenAt   time.Time `json:"seen_at"`
	ExitCode *int      `json:"exit_code,omitempty"`
	// Usage is what the runner used, from its host's exit report.
	Usage *Usage `json:"usage,omitempty"`
	Scope string `json:"scope"`
	// Repository is owner/name.
	Repository  string `json:"repository"`
	WorkflowRef string `json:"workflow_ref,omitempty"`
	Job         string `json:"job"`
	// Event is what started the workflow run.
	Event   string `json:"event,omitempty"`
	Profile string `json:"profile"`
	Host    string `json:"host,omitempty"`
	Runner  string `json:"runner,omitempty"`
	// Result is GitHub's verdict on the job and ResultText its own word;
	// Exit is the pool's verdict on the runner.
	Result     string `json:"result,omitempty"`
	ResultText string `json:"result_text,omitempty"`
	Exit       string `json:"exit,omitempty"`
	// WaitReason is the pool's job reason the run waited under longest while
	// queued, empty when the pool saw it waiting under none.
	WaitReason string `json:"wait_reason,omitempty"`
	WaitMS     int64  `json:"wait_ms,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	RunID      int64  `json:"run_id"`
	ID         uint64 `json:"id"`
}

// Usage is what a run's runner used over its container's life,
// in whole milliseconds; PeakMemory is 0 when its host read none.
type Usage struct {
	CPUMS      int64 `json:"cpu_ms"`
	RanMS      int64 `json:"ran_ms"`
	PeakMemory int64 `json:"peak_memory,omitempty"`
}

// age is when a run leaves the history: its end, or its first message
// while open.
func (r *Run) age() time.Time {
	if !r.EndedAt.IsZero() {
		return r.EndedAt
	}
	return r.SeenAt
}

// Blob is a run's evidence: its log tail and _diag tar.
type Blob struct {
	StoredAt time.Time `json:"stored_at"`
	Log      string    `json:"log,omitempty"`
	Diag     []byte    `json:"diag,omitempty"`
}

// Exit is a runner's exit that no run row held when it arrived, kept until
// a job message names the runner.
type Exit struct {
	At     time.Time `json:"at"`
	Code   *int      `json:"code,omitempty"`
	Usage  *Usage    `json:"usage,omitempty"`
	Host   string    `json:"host"`
	Reason string    `json:"reason"`
	Log    string    `json:"log,omitempty"`
	Runner string    `json:"runner"`
	Diag   []byte    `json:"diag,omitempty"`
}

// HistoryTx is one write transaction over the history.
type HistoryTx struct{ tx *bolttable.Tx }

// UpdateHistory runs fn in one write transaction.
func (s *Store) UpdateHistory(ctx context.Context, fn func(*HistoryTx) error) error {
	return s.update(ctx, func(tx *bolttable.Tx) error { return fn(&HistoryTx{tx: tx}) })
}

// Run answers run id's row.
func (h *HistoryTx) Run(id uint64) (Run, bool, error) { return runs.Get(h.tx.Read(), id) }

// PutRun writes r under its ID.
func (h *HistoryTx) PutRun(r *Run) error { return runs.Put(h.tx, r.ID, r) }

// DeleteRun deletes run id's row and its evidence.
func (h *HistoryTx) DeleteRun(id uint64) error {
	_, err := runs.Delete(h.tx, id)
	return err
}

// RunOfRunner answers the run the runner named name took.
func (h *HistoryTx) RunOfRunner(name string) (Run, bool, error) {
	id, _, found, err := runsByRunner.Lookup(h.tx.Read(), []byte(name))
	if err != nil || !found {
		return Run{}, false, err
	}
	return h.Run(id)
}

// OpenRuns visits every run with a runner on host and no exit, until fn
// answers false.
func (h *HistoryTx) OpenRuns(host string, fn func(*Run) (bool, error)) error {
	return runsOpen.Scan(h.tx.Read(), bolttable.Range{Prefix: append([]byte(host), 0)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		return h.visit(id, fn)
	})
}

// Runs visits every run whose end, or first message while open, is at or
// after from, oldest first, until fn answers false.
func (h *HistoryTx) Runs(from time.Time, fn func(*Run) (bool, error)) error {
	return runsByAge.Scan(h.tx.Read(), bolttable.Range{From: ageBytes(from)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		return h.visit(id, fn)
	})
}

// DeleteRunsBefore deletes every run, with its evidence, whose end, or
// first message while open, is before cutoff.
func (h *HistoryTx) DeleteRunsBefore(cutoff time.Time) error {
	return deleteBefore(h.tx, runs, runsByAge, cutoff)
}

func (h *HistoryTx) visit(id uint64, fn func(*Run) (bool, error)) (bool, error) {
	r, found, err := h.Run(id)
	if err != nil || !found {
		return err == nil, err
	}
	return fn(&r)
}

// PutBlob writes run id's evidence.
func (h *HistoryTx) PutBlob(id uint64, b *Blob) error { return blobs.Put(h.tx, id, b) }

// Blobs visits every run's evidence by run ID until fn answers false.
func (h *HistoryTx) Blobs(fn func(id uint64, b *Blob) (bool, error)) error {
	return blobs.Scan(h.tx.Read(), bolttable.Range{}, func(id uint64, b *Blob) (bool, error) { return fn(id, b) })
}

// DeleteBlob deletes run id's evidence.
func (h *HistoryTx) DeleteBlob(id uint64) error {
	_, err := blobs.Delete(h.tx, id)
	return err
}

// Exit answers the kept exit of the runner named name.
func (h *HistoryTx) Exit(name string) (Exit, bool, error) {
	return exits.Get(h.tx.Read(), []byte(name))
}

// PutExit keeps e until a job message names its runner.
func (h *HistoryTx) PutExit(e *Exit) error { return exits.Put(h.tx, []byte(e.Runner), e) }

// Exits visits every kept exit until fn answers false.
func (h *HistoryTx) Exits(fn func(*Exit) (bool, error)) error {
	return exits.Scan(h.tx.Read(), bolttable.Range{}, func(_ []byte, e *Exit) (bool, error) { return fn(e) })
}

// DeleteExit drops the kept exit of the runner named name.
func (h *HistoryTx) DeleteExit(name string) error {
	_, err := exits.Delete(h.tx, []byte(name))
	return err
}

// TrimJournal deletes the journal's rows older than before, and its oldest
// beyond the newest keep.
func (h *HistoryTx) TrimJournal(before time.Time, keep int64) error {
	n, err := journalCount.Value(h.tx.Read())
	if err != nil {
		return err
	}
	var ids []uint64
	err = journal.Scan(h.tx.Read(), bolttable.Range{}, func(id uint64, e *entry) (bool, error) {
		if n <= keep && !e.At.Before(before) {
			return false, nil
		}
		ids = append(ids, id)
		n--
		return true, nil
	})
	for _, id := range ids {
		if err != nil {
			break
		}
		_, err = journal.Delete(h.tx, id)
	}
	return err
}

func ageBytes(t time.Time) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(max(t.UnixMilli(), 0)))
}

// runIndexes declares the runs table's indexes: the runner that took each
// run, each open run by host and runner, and each run by age.
func runIndexes(t *bolttable.Table[uint64, Run]) (byRunner, open, byAge *bolttable.Index[uint64, Run]) {
	byRunner = bolttable.Must(bolttable.DefineIndex(t, "runs_by_runner", bolttable.IndexOptions{Unique: true, Version: 1},
		func(_ uint64, r *Run) []bolttable.Entry {
			if r.Runner == "" {
				return nil
			}
			return []bolttable.Entry{{Key: []byte(r.Runner)}}
		}))
	open = bolttable.Must(bolttable.DefineIndex(t, "runs_open", bolttable.IndexOptions{Version: 1},
		func(_ uint64, r *Run) []bolttable.Entry {
			if r.Runner == "" || r.Exit != "" {
				return nil
			}
			return []bolttable.Entry{{Key: append(append([]byte(r.Host), 0), r.Runner...)}}
		}))
	byAge = bolttable.Must(bolttable.DefineIndex(t, "runs_by_age", bolttable.IndexOptions{Version: 1},
		func(_ uint64, r *Run) []bolttable.Entry { return []bolttable.Entry{{Key: ageBytes(r.age())}} }))
	return byRunner, open, byAge
}
