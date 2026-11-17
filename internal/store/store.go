// Package store is the control plane's bolttable file, DATA_DIR/manade.db:
// every namespace it keeps on one handle, which holds the file's lock for
// the store's life and refuses a file a later store format wrote.
package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cplieger/bolttable"
)

// File is the store's name under DATA_DIR.
const File = "manade.db"

// ErrClosed reports a call on a store whose file is not open: before Open,
// after a failed Open or Close. A writer on its own schedule reads it as a
// skipped write.
var ErrClosed = errors.New("store: the file is not open")

// compaction compacts at open when half the file, and at least 64 MiB, is free.
var compaction = bolttable.CompactPolicy{MinFreeRatio: 0.5, MinFreeBytes: 64 << 20}

// Options configures New.
type Options struct {
	// Log is slog.Default() when nil.
	Log *slog.Logger
	// Build is the running build, recorded in the file.
	Build string
}

// Store is the file's one handle.
type Store struct {
	live *bolttable.Live
	path string
	// dir is the directory holding the file.
	dir    string
	writes bolttable.WriteHealth
}

// New takes the lock of the file at path and holds it until Close; it
// opens nothing. Another holder of the lock fails it with an error matching
// bolttable.ErrLocked and naming the lock file.
func New(path string, o Options) (*Store, error) {
	log := cmp.Or(o.Log, slog.Default())
	live, err := bolttable.NewLive(path, schema, bolttable.Options{
		Compact: compaction, CommitFault: commitFault,
		Inspect: func(r *bolttable.ReadTx) error { return refuseLater(r, o.Build) },
	})
	if err != nil {
		return nil, err
	}
	live.OnOpen(func(db *bolttable.DB) error {
		r := db.OpenReport()
		logOpen(log, &r)
		return db.Update(context.Background(), func(tx *bolttable.Tx) error { return record(tx, o.Build) })
	})
	return &Store{live: live, path: path, dir: filepath.Dir(path)}, nil
}

// Open opens or creates the file and stamps it with this build. A file a
// later store format stamped fails it with an error wrapping
// bolttable.ErrNewerBuild, and one holding a namespace this schema does not
// declare with a *bolttable.FormatError, either leaving the file as it was;
// bolttable.OpenFailure classes every failure, after which the store stays
// closed.
func (s *Store) Open() error {
	if err := s.live.Open(); err != nil {
		return fmt.Errorf("store %s: %w", s.path, err)
	}
	return nil
}

// Close closes the file and releases its lock; it is final.
func (s *Store) Close() error { return s.live.Close() }

// view runs fn in one read transaction.
func (s *Store) view(ctx context.Context, fn func(*bolttable.ReadTx) error) error {
	return closedAs(s.live.View(ctx, fn))
}

// closedAs translates bolttable's closed handle to ErrClosed.
func closedAs(err error) error {
	if errors.Is(err, bolttable.ErrClosed) {
		return fmt.Errorf("%w: %w", ErrClosed, err)
	}
	return err
}

func logOpen(log *slog.Logger, r *bolttable.OpenReport) {
	switch {
	case r.CompactErr != nil && r.Compacted:
		log.Warn("The store was compacted at open, but its directory did not sync, so a power loss may bring back the file as it was", "error", r.CompactErr)
	case r.CompactErr != nil:
		log.Warn("The store could not be compacted at open, so it keeps its free space until a later start", "error", r.CompactErr)
	case r.Compacted:
		log.Info("The store was compacted at open")
	}
}

// record stamps the file with this build.
func record(tx *bolttable.Tx, build string) error {
	v, err := json.Marshal(stamp{Format: format, Build: build})
	if err != nil {
		return err
	}
	raw := json.RawMessage(v)
	return meta.Put(tx, stampKey, &raw)
}

// readStamp answers the file's stamp; one that does not decode is a damaged
// file.
func readStamp(r *bolttable.ReadTx) (stamp, bool, error) {
	raw, found, err := meta.Get(r, stampKey)
	if err != nil || !found {
		return stamp{}, found, err
	}
	var st stamp
	if err := json.Unmarshal(raw, &st); err != nil {
		return stamp{}, true, fmt.Errorf("%w: meta %s: %w", bolttable.ErrCorrupt, stampKey, err)
	}
	return st, true, nil
}

// refuseLater is the open's inspection: it refuses a file whose stamp names
// a later format than this build's, naming both builds.
func refuseLater(r *bolttable.ReadTx, build string) error {
	last, found, err := readStamp(r)
	if err != nil || !found || last.Format <= format {
		return err
	}
	return fmt.Errorf("%w: manade %s wrote this file in store format %d, and this build, %s, reads format %d: run %s or later",
		bolttable.ErrNewerBuild, cmp.Or(last.Build, "of an unknown build"), last.Format, cmp.Or(build, "unstamped"), format,
		cmp.Or(last.Build, "a newer build"))
}
