package store

import (
	"encoding/json"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/bolttable"
)

// The fixture is two builds of one format (design §5.3 Upgrade): the earlier
// keeps runs in namespace runs1 with a field the later one renamed, so the
// later build declares runs2 and retires runs1.
type runV1 struct {
	Repo string `json:"repo"`
}

type runV2 struct {
	Repository string `json:"repository"`
}

const (
	prefixRunsV1   byte = 0xe0
	prefixRunsV2   byte = 0xe1
	prefixCopiedV2 byte = 0xe2
)

var copiedKey = []byte("runs1")

type generations struct {
	earlier *bolttable.Schema
	runs1   *bolttable.Table[[]byte, runV1]
	later   layout
	runs2   *bolttable.Table[[]byte, runV2]
	// carried reads the file under later.carry.
	carriedRuns1  *bolttable.Table[[]byte, runV1]
	carriedCopied *bolttable.Table[[]byte, json.RawMessage]

	path        string
	copies      int
	lockErr     error
	failCopy    error
	skipCopyRow bool
}

func metaSchema() (*bolttable.Schema, *bolttable.Table[[]byte, metaRow]) {
	s := bolttable.NewSchema(schemaName)
	m := bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("meta")), prefixMeta, "meta", bolttable.BytesKey{}, bolttable.JSON[metaRow](1), bolttable.TableOptions{}))
	return s, m
}

func defineRuns1(s *bolttable.Schema) *bolttable.Table[[]byte, runV1] {
	return bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("runs1")), prefixRunsV1, "runs", bolttable.BytesKey{}, bolttable.JSON[runV1](1), bolttable.TableOptions{}))
}

func defineRuns2(s *bolttable.Schema) (*bolttable.Table[[]byte, runV2], *bolttable.Table[[]byte, json.RawMessage]) {
	ns := bolttable.Must(s.Namespace("runs2"))
	runs := bolttable.Must(bolttable.Define(ns, prefixRunsV2, "runs", bolttable.BytesKey{}, bolttable.JSON[runV2](1), bolttable.TableOptions{}))
	copied := bolttable.Must(bolttable.Define(ns, prefixCopiedV2, "copied", bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	return runs, copied
}

func newGenerations(t *testing.T) *generations {
	t.Helper()
	g := &generations{path: testPath(t)}
	g.earlier, _ = metaSchema()
	g.runs1 = defineRuns1(g.earlier)

	later, laterMeta := metaSchema()
	g.runs2, _ = defineRuns2(later)

	carry, _ := metaSchema()
	g.carriedRuns1 = defineRuns1(carry)
	carriedRuns2, carriedCopied := defineRuns2(carry)
	g.carriedCopied = carriedCopied

	g.later = layout{schema: later, meta: laterMeta, carry: carry, retired: []retirement{{
		name: "runs1",
		copied: func(r *bolttable.ReadTx) (bool, error) {
			_, found, err := carriedCopied.Get(r, copiedKey)
			return found, err
		},
		copy: func(tx *bolttable.Tx) error {
			g.copies++
			lock, err := bolttable.LockPath(g.path, 0)
			if err == nil {
				err = lock.Release()
			} else {
				g.lockErr = err
			}
			if err != nil && !errors.Is(err, bolttable.ErrLocked) {
				return err
			}
			if g.failCopy != nil {
				return g.failCopy
			}
			err = g.carriedRuns1.Scan(tx.Read(), bolttable.Range{}, func(k []byte, v *runV1) (bool, error) {
				return true, carriedRuns2.Put(tx, k, &runV2{Repository: v.Repo})
			})
			if err != nil || g.skipCopyRow {
				return err
			}
			return carriedCopied.Put(tx, copiedKey, &json.RawMessage{'{', '}'})
		},
	}}}
	return g
}

// writeEarlier leaves the file as the earlier build would.
func (g *generations) writeEarlier(t *testing.T, rows map[string]string) {
	t.Helper()
	db, err := bolttable.Open(g.path, g.earlier, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(t.Context(), func(tx *bolttable.Tx) error {
		for _, k := range slices.Sorted(maps.Keys(rows)) {
			if err := g.runs1.Put(tx, []byte(k), &runV1{Repo: rows[k]}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func (g *generations) open(t *testing.T) (*Store, error) {
	t.Helper()
	return open(g.path, &g.later, Options{Build: "v1.0.0-dev.5"})
}

func (g *generations) mustOpen(t *testing.T) *Store {
	t.Helper()
	s, err := g.open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func (g *generations) laterRuns(t *testing.T, s *Store) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		return g.runs2.Scan(r, bolttable.Range{}, func(k []byte, v *runV2) (bool, error) {
			got[string(k)] = v.Repository
			return true, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// carried reads what the file holds of runs1 and the copied row, under the
// schema that declares both generations.
func (g *generations) carried(t *testing.T) (runs1 map[string]string, copied bool) {
	t.Helper()
	db, err := bolttable.OpenReadOnly(g.path, g.later.carry, bolttable.ReadOptions{})
	if err != nil {
		t.Fatalf("read the file under both generations: %v", err)
	}
	defer db.Close()
	runs1 = map[string]string{}
	err = db.View(t.Context(), func(r *bolttable.ReadTx) error {
		if err := g.carriedRuns1.Scan(r, bolttable.Range{}, func(k []byte, v *runV1) (bool, error) {
			runs1[string(k)] = v.Repo
			return true, nil
		}); err != nil {
			return err
		}
		_, copied, err = g.carriedCopied.Get(r, copiedKey)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return runs1, copied
}

func namespaces(t *testing.T, s *Store) []string {
	t.Helper()
	var names []string
	for _, ns := range stats(t, s).Namespaces {
		names = append(names, ns.Name)
	}
	return names
}

func oldCopies(t *testing.T, path string) []string {
	t.Helper()
	found, err := filepath.Glob(path + ".*.old")
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func lockFree(t *testing.T, path string) {
	t.Helper()
	lock, err := bolttable.LockPath(path, 0)
	if err != nil {
		t.Fatalf("the open left the file's lock held: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// rows are the runs in either generation: the copy keeps each run's key and
// moves its repository to the renamed field.
var rows = map[string]string{"7": "cplieger/a", "9": "cplieger/b"}

func TestOpenRetiresANamespaceInOneStart(t *testing.T) {
	g := newGenerations(t)
	g.writeEarlier(t, rows)

	s := g.mustOpen(t)
	defer mustClose(t, s)
	if got := g.laterRuns(t, s); !maps.Equal(got, rows) {
		t.Errorf("runs2 holds %v, want %v", got, rows)
	}
	if got := namespaces(t, s); slices.Contains(got, "runs1") {
		t.Errorf("the file still holds runs1: %v", got)
	}
	r := s.db.OpenReport()
	if len(r.Reset) != 1 || r.Reset[0].Name != "runs1" {
		t.Errorf("the open reset %v, want runs1 only", r.Reset)
	}
	if r.Kept == "" {
		t.Error("the open kept no copy of the file before dropping runs1")
	}
	if got := readMetaOf(t, s, g.later.meta); got != (metaRow{Format: format, Build: "v1.0.0-dev.5"}) {
		t.Errorf("meta = %+v after the retirement", got)
	}
}

func TestRetirementHoldsTheLockThroughEveryOpen(t *testing.T) {
	g := newGenerations(t)
	g.writeEarlier(t, rows)
	mustClose(t, g.mustOpen(t))
	if g.copies != 1 {
		t.Fatalf("the copy ran %d times, want once", g.copies)
	}
	if !errors.Is(g.lockErr, bolttable.ErrLocked) {
		t.Errorf("another holder could take the lock during the copy: %v", g.lockErr)
	}
	lockFree(t, g.path)
}

func TestARetiredFileOpensAgainUnchanged(t *testing.T) {
	g := newGenerations(t)
	g.writeEarlier(t, rows)
	mustClose(t, g.mustOpen(t))
	kept := oldCopies(t, g.path)

	s := g.mustOpen(t)
	defer mustClose(t, s)
	if g.copies != 1 {
		t.Errorf("the copy ran %d times over two starts, want once", g.copies)
	}
	if r := s.db.OpenReport(); r.Reset != nil || r.Kept != "" {
		t.Errorf("the second start reset %v and kept %q", r.Reset, r.Kept)
	}
	if got := oldCopies(t, g.path); !slices.Equal(got, kept) {
		t.Errorf("the second start left copies %v, want %v", got, kept)
	}
	if got := g.laterRuns(t, s); !maps.Equal(got, rows) {
		t.Errorf("runs2 holds %v, want %v", got, rows)
	}
}

// A start that stopped after the copy and before the drop leaves the copied
// row beside both namespaces; the next start drops runs1 without copying.
func TestRetirementAfterACrashCopiesNothingAgain(t *testing.T) {
	g := newGenerations(t)
	g.writeEarlier(t, rows)
	lock, err := bolttable.LockPath(g.path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := carryOver(g.path, lock, g.later.carry, g.later.retired); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if runs1, copied := g.carried(t); !copied || !maps.Equal(runs1, rows) {
		t.Fatalf("after the crash runs1 = %v and copied = %v, want both generations", runs1, copied)
	}

	s := g.mustOpen(t)
	defer mustClose(t, s)
	if g.copies != 1 {
		t.Errorf("the copy ran %d times, want once", g.copies)
	}
	if got := namespaces(t, s); slices.Contains(got, "runs1") {
		t.Errorf("the file still holds runs1: %v", got)
	}
	if got := g.laterRuns(t, s); !maps.Equal(got, rows) {
		t.Errorf("runs2 holds %v, want %v", got, rows)
	}
}

func TestOpenLeavesAFileThatNeverHeldTheNamespace(t *testing.T) {
	for name, prepare := range map[string]func(*testing.T, *generations){
		"new file": func(*testing.T, *generations) {},
		"later file": func(t *testing.T, g *generations) {
			mustClose(t, g.mustOpen(t))
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGenerations(t)
			prepare(t, g)
			s := g.mustOpen(t)
			defer mustClose(t, s)
			if g.copies != 0 {
				t.Errorf("the copy ran %d times", g.copies)
			}
			if r := s.db.OpenReport(); r.Reset != nil || r.Kept != "" {
				t.Errorf("the open reset %v and kept %q", r.Reset, r.Kept)
			}
			if got := oldCopies(t, g.path); got != nil {
				t.Errorf("the open left copies %v", got)
			}
		})
	}
}

func TestAFailedCopyDropsNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		fault func(*generations)
		want  string
	}{
		"copy fails":       {func(g *generations) { g.failCopy = errors.New("disk full") }, "disk full"},
		"copy row missing": {func(g *generations) { g.skipCopyRow = true }, "wrote no copied row"},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGenerations(t)
			g.writeEarlier(t, rows)
			tc.fault(g)
			_, err := g.open(t)
			if err == nil || !strings.Contains(err.Error(), "runs1") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Open = %v, want an error naming runs1 and %q", err, tc.want)
			}
			lockFree(t, g.path)
			if runs1, copied := g.carried(t); copied || !maps.Equal(runs1, rows) {
				t.Errorf("after the failed copy runs1 = %v and copied = %v, want runs1 whole and no copied row", runs1, copied)
			}
			if got := oldCopies(t, g.path); got != nil {
				t.Errorf("the failed start left copies %v", got)
			}

			g.failCopy, g.skipCopyRow = nil, false
			s := g.mustOpen(t)
			defer mustClose(t, s)
			if got := g.laterRuns(t, s); !maps.Equal(got, rows) {
				t.Errorf("the next start left runs2 = %v, want %v", got, rows)
			}
		})
	}
}

// A namespace this build neither declares nor retires is a later build's,
// and the file is refused as it is (design §5.3 Upgrade).
func TestOpenRefusesANamespaceItDoesNotRetire(t *testing.T) {
	later := func(s *bolttable.Schema) *bolttable.Table[[]byte, json.RawMessage] {
		return bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("later")), 0xef, "rows", bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	}
	writeLater := func(t *testing.T, path string, s *bolttable.Schema) {
		t.Helper()
		tb := later(s)
		db, err := bolttable.Open(path, s, bolttable.Options{})
		if err != nil {
			t.Fatal(err)
		}
		v := json.RawMessage(`{}`)
		if err := db.Update(t.Context(), func(tx *bolttable.Tx) error { return tb.Put(tx, []byte("k"), &v) }); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(t *testing.T, path string, err error) {
		t.Helper()
		if !errors.Is(err, bolttable.ErrFormat) || !strings.Contains(err.Error(), "later") {
			t.Errorf("Open = %v, want a format error naming later", err)
		}
		lockFree(t, path)
		if got := oldCopies(t, path); got != nil {
			t.Errorf("the refused open left copies %v", got)
		}
	}

	t.Run("this build", func(t *testing.T) {
		path := testPath(t)
		s, _ := metaSchema()
		writeLater(t, path, s)
		_, err := Open(path, Options{Build: "v1.0.0-dev.5"})
		refused(t, path, err)
	})
	t.Run("beside a retired namespace", func(t *testing.T) {
		g := newGenerations(t)
		g.writeEarlier(t, rows)
		s, _ := metaSchema()
		defineRuns1(s)
		writeLater(t, g.path, s)
		_, err := g.open(t)
		refused(t, g.path, err)
		if g.copies != 0 {
			t.Errorf("the copy ran %d times for a refused file", g.copies)
		}
	})
}

func readMetaOf(t *testing.T, s *Store, m *bolttable.Table[[]byte, metaRow]) metaRow {
	t.Helper()
	var row metaRow
	err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var err error
		row, _, err = m.Get(r, metaKey)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return row
}
