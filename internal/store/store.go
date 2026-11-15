// Package store is the control plane's bolttable file, DATA_DIR/manade.db:
// every namespace it keeps, opened under one lock held for the whole start,
// and refused when a later store format wrote it.
package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cplieger/bolttable"
)

// File is the store's name under DATA_DIR.
const File = "manade.db"

// compaction compacts at open when half the file, and at least 64 MiB, is
// free: run history is the bulk, and its retention frees pages daily.
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
}

// Open opens or creates the file at path. Another holder of the file fails
// it with an error matching bolttable.ErrLocked; a file a later format wrote,
// or one whose namespaces this build does not read, fails it unchanged.
func Open(path string, o Options) (*Store, error) {
	log := cmp.Or(o.Log, slog.Default())
	lock, err := bolttable.LockPath(path, 0)
	if err != nil {
		return nil, fmt.Errorf("store %s: %w", path, err)
	}
	s, err := open(path, lock, o.Build, log)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("store %s: %w", path, err), lock.Release())
	}
	return s, nil
}

func open(path string, lock *bolttable.Lock, build string, log *slog.Logger) (*Store, error) {
	db, err := bolttable.Open(path, schema, bolttable.Options{Lock: lock, Compact: compaction})
	if err != nil {
		return nil, err
	}
	switch r := db.OpenReport(); {
	case r.CompactErr != nil:
		log.Warn("The store could not be compacted at open, so it keeps its free space until a later start", "error", r.CompactErr)
	case r.Compacted:
		log.Info("The store was compacted at open")
	}
	if err := db.Update(context.Background(), func(tx *bolttable.Tx) error { return record(tx, build) }); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &Store{db: db, lock: lock}, nil
}

// record refuses a file of a later format and records this build in it.
func record(tx *bolttable.Tx, build string) error {
	last, found, err := meta.Get(tx.Read(), metaKey)
	if err != nil {
		return err
	}
	if found && last.Format > format {
		return fmt.Errorf("manade %s wrote this file in store format %d, and this build, %s, reads format %d: run %s or later",
			cmp.Or(last.Build, "of an unknown build"), last.Format, cmp.Or(build, "unstamped"), format, cmp.Or(last.Build, "a newer build"))
	}
	return meta.Put(tx, metaKey, &metaRow{Format: format, Build: build})
}

// Close closes the file and releases its lock.
func (s *Store) Close() error {
	return errors.Join(s.db.Close(), s.lock.Release())
}
