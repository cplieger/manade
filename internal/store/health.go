package store

import (
	"context"
	"time"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/manade/internal/host/platform"
)

// writeLease is how long the store's writes may keep failing before it
// reads unhealthy, so one failed commit that the next write
// clears is no fault; a test seam.
var writeLease = 2 * time.Minute

// commitFault is bolttable.Options.CommitFault, a test seam.
var commitFault func(context.Context) error

// writeClass is the one class of every write: a commit is the file's,
// whatever table it writes.
const writeClass = "write"

// update runs fn in one write transaction and records its commit.
func (s *Store) update(ctx context.Context, fn func(*bolttable.Tx) error) error {
	err := s.live.Update(ctx, fn)
	s.writes.Record(writeClass, err)
	return closedAs(err)
}

// Healthy reports false once the store's writes have failed for longer
// than writeLease with none committed since.
func (s *Store) Healthy() bool {
	rec, _ := s.writes.Class(writeClass)
	return !rec.Failing || time.Since(rec.FirstFailed) <= writeLease
}

// WriteFailures counts the writes whose commit failed since Open.
func (s *Store) WriteFailures() uint64 {
	rec, _ := s.writes.Class(writeClass)
	return rec.Failures
}

// Size answers the file's size in bytes.
func (s *Store) Size() (int64, error) {
	db, err := s.live.Pin()
	if err != nil {
		return 0, closedAs(err)
	}
	sp, err := db.Space()
	return sp.FileBytes, closedAs(err)
}

// VolumeFree answers the bytes free to the control plane on the
// filesystem holding the file.
func (s *Store) VolumeFree() (uint64, error) {
	u, err := platform.DataUsage(s.dir)
	return u.Free, err
}
