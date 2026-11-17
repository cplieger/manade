// Package store is the control plane's bolttable file, DATA_DIR/manade.db:
// every namespace it keeps, opened under one lock held for the whole start,
// and refused when a later store format wrote it.
package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"

	"github.com/cplieger/bolttable"
)

// File is the store's name under DATA_DIR.
const File = "manade.db"

// compaction compacts at open when half the file, and at least 64 MiB, is free.
var compaction = bolttable.CompactPolicy{MinFreeRatio: 0.5, MinFreeBytes: 64 << 20}

// Options configures Open.
type Options struct {
	// Log is slog.Default() when nil.
	Log *slog.Logger
	// Build is the running build, recorded in the file.
	Build string
}

// Store is the open file.
type Store struct {
	db   *bolttable.DB
	lock *bolttable.Lock
	// dir is the directory holding the file.
	dir    string
	writes bolttable.WriteHealth
}

// Open opens or creates the file at path. Another holder of the file fails
// it with an error matching bolttable.ErrLocked. A file stamped by a later
// store format fails it naming both builds, and one holding a namespace
// this schema does not declare fails it with a *bolttable.FormatError.
// Either way the file is left as it was, unless a later build opens it
// between the stamp's read and the lock, when Open's compaction may already
// have run.
func Open(path string, o Options) (*Store, error) {
	log := cmp.Or(o.Log, slog.Default())
	if err := checkStamp(path, o.Build); err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	lock, err := bolttable.LockPath(path, 0)
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	s, err := start(path, lock, o.Build, log)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("store %s: %w", path, err), lock.Release())
	}
	return s, nil
}

// checkStamp refuses a file a later format stamped before anything opens it
// for writing: bolttable.Open compacts and creates namespaces before any
// transaction of ours runs. A file that is absent, or that this schema
// cannot read, is left to Open.
func checkStamp(path, build string) error {
	db, err := bolttable.OpenReadOnly(path, schema, bolttable.ReadOptions{})
	var fe *bolttable.FormatError
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.As(err, &fe):
		return nil
	case err != nil:
		return err
	}
	var last stamp
	var found bool
	err = db.View(context.Background(), func(r *bolttable.ReadTx) (verr error) {
		last, found, verr = readStamp(r)
		return verr
	})
	if err = errors.Join(err, db.Close()); err != nil || !found {
		return err
	}
	return refuseLater(last, build)
}

func start(path string, lock *bolttable.Lock, build string, log *slog.Logger) (*Store, error) {
	db, err := bolttable.Open(path, schema, bolttable.Options{Lock: lock, Compact: compaction, CommitFault: commitFault})
	if err != nil {
		return nil, err
	}
	r := db.OpenReport()
	logOpen(log, &r)
	if err := db.Update(context.Background(), func(tx *bolttable.Tx) error { return record(tx, build) }); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db, lock: lock, dir: filepath.Dir(path)}, nil
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

// record stamps the file with this build. It refuses a later format again:
// another build may have opened the file between checkStamp and the lock.
func record(tx *bolttable.Tx, build string) error {
	last, found, err := readStamp(tx.Read())
	if err != nil {
		return err
	}
	if found {
		if rerr := refuseLater(last, build); rerr != nil {
			return rerr
		}
	}
	v, err := json.Marshal(stamp{Format: format, Build: build})
	if err != nil {
		return err
	}
	raw := json.RawMessage(v)
	return meta.Put(tx, stampKey, &raw)
}

func readStamp(r *bolttable.ReadTx) (stamp, bool, error) {
	raw, found, err := meta.Get(r, stampKey)
	if err != nil || !found {
		return stamp{}, found, err
	}
	var st stamp
	if err := json.Unmarshal(raw, &st); err != nil {
		return stamp{}, true, fmt.Errorf("meta %s: %w", stampKey, err)
	}
	return st, true, nil
}

func refuseLater(last stamp, build string) error {
	if last.Format <= format {
		return nil
	}
	return fmt.Errorf("manade %s wrote this file in store format %d, and this build, %s, reads format %d: run %s or later",
		cmp.Or(last.Build, "of an unknown build"), last.Format, cmp.Or(build, "unstamped"), format, cmp.Or(last.Build, "a newer build"))
}

// Close closes the file and releases its lock.
func (s *Store) Close() error {
	return errors.Join(s.db.Close(), s.lock.Release())
}
