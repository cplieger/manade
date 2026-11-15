package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cplieger/bolttable"
)

// layout is a build's schema and the namespaces earlier builds of its format
// kept that it no longer declares.
type layout struct {
	schema *bolttable.Schema
	meta   *bolttable.Table[[]byte, metaRow]
	// carry declares schema's namespaces and every retired one; nil while
	// nothing is retired.
	carry   *bolttable.Schema
	retired []retirement
}

// retirement carries one retired namespace's rows into the namespace that
// replaced it. A file from any earlier build of the format may still hold
// the retired namespace, so a retirement stays for the rest of the format.
type retirement struct {
	// copied reports whether the replacing namespace holds the row copy writes.
	copied func(*bolttable.ReadTx) (bool, error)
	// copy copies the rows and writes that row, both in the transaction it is given.
	copy func(*bolttable.Tx) error
	name string
}

var current = layout{schema: schema, meta: meta}

// openFile opens path under l and the held lock. A file that still holds
// retired namespaces, and nothing else this build does not read, has their
// rows copied and is then opened again dropping them, keeping the file as it
// was beside it as .old; anything else that fails the first open is returned.
func openFile(path string, lock *bolttable.Lock, l *layout) (*bolttable.DB, error) {
	o := bolttable.Options{Lock: lock, Compact: compaction}
	db, err := bolttable.Open(path, l.schema, o)
	var fe *bolttable.FormatError
	if !errors.As(err, &fe) {
		return db, err
	}
	due, ok := l.due(fe)
	if !ok {
		return nil, err
	}
	if err := carryOver(path, lock, l.carry, due); err != nil {
		return nil, fmt.Errorf("retire %s: %w", names(due), err)
	}
	o.Reset = bolttable.ResetPolicy{
		Allow: func(n bolttable.NamespaceDigest) bool {
			return slices.ContainsFunc(due, func(r retirement) bool { return r.name == n.Name })
		},
		Keep: 2,
	}
	return bolttable.Open(path, l.schema, o)
}

// due is the retirement of every namespace fe lists, and false when fe lists
// one that is not retired.
func (l *layout) due(fe *bolttable.FormatError) ([]retirement, bool) {
	var due []retirement
	for _, n := range fe.Namespaces {
		i := slices.IndexFunc(l.retired, func(r retirement) bool { return r.name == n.Name })
		if i < 0 {
			return nil, false
		}
		due = append(due, l.retired[i])
	}
	return due, len(due) > 0
}

// carryOver copies every due retirement not copied yet in one transaction,
// so a start that stopped before the drop copies nothing the next time.
func carryOver(path string, lock *bolttable.Lock, carry *bolttable.Schema, due []retirement) error {
	db, err := bolttable.Open(path, carry, bolttable.Options{Lock: lock})
	if err != nil {
		return err
	}
	err = db.Update(context.Background(), func(tx *bolttable.Tx) error {
		for _, r := range due {
			if cerr := carryOne(tx, r); cerr != nil {
				return cerr
			}
		}
		return nil
	})
	return errors.Join(err, db.Close())
}

func carryOne(tx *bolttable.Tx, r retirement) error {
	done, err := r.copied(tx.Read())
	if err != nil || done {
		return err
	}
	if cerr := r.copy(tx); cerr != nil {
		return cerr
	}
	// The drop that follows trusts this row, so a copy that forgot it would
	// lose the namespace's rows.
	if done, err = r.copied(tx.Read()); err == nil && !done {
		err = fmt.Errorf("the copy of %s wrote no copied row", r.name)
	}
	return err
}

func names(rs []retirement) string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.name)
	}
	return strings.Join(out, ", ")
}
