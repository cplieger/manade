package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/bolttable"
)

func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), File)
}

func mustOpen(t *testing.T, path, build string) *Store {
	t.Helper()
	s, err := Open(path, Options{Build: build})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustClose(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func stats(t *testing.T, s *Store) bolttable.Stats {
	t.Helper()
	st, err := s.db.Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return st
}

// putMeta writes raw as the meta row of a new file at path, as another build
// would have left it. Its schema declares the meta table with a raw value,
// which has the production table's digest, so the full schema opens the file.
func putMeta(t *testing.T, path, raw string) {
	t.Helper()
	rs := bolttable.NewSchema(schemaName)
	ns := bolttable.Must(rs.Namespace("meta"))
	tb := bolttable.Must(bolttable.Define(ns, prefixMeta, "meta", bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	db, err := bolttable.Open(path, rs, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	v := json.RawMessage(raw)
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error { return tb.Put(tx, metaKey, &v) }); err != nil {
		t.Fatalf("put meta: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func readMeta(t *testing.T, s *Store) metaRow {
	t.Helper()
	var row metaRow
	err := s.db.View(t.Context(), func(r *bolttable.ReadTx) error {
		var found bool
		var err error
		row, found, err = meta.Get(r, metaKey)
		if err == nil && !found {
			err = errors.New("no meta row")
		}
		return err
	})
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	return row
}

// The namespaces and tables of the store's design (design §5.3, "What it
// holds"): the settings document comes with the settings store and the
// accounts live in their own file.
var designedTables = map[string][]string{
	"meta":    {"meta"},
	"secrets": {"values"},
	"hosts":   {"identities", "tombstones"},
	"github":  {"scalesets"},
	"journal": {"entries"},
	"history": {"runs", "runblobs", "hostevents", "probes"},
	"rollups": {"days"},
}

func TestOpenDeclaresEveryNamespace(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	got := map[string][]string{}
	for _, ns := range stats(t, s).Namespaces {
		for _, tb := range ns.Tables {
			got[ns.Name] = append(got[ns.Name], tb.Name)
		}
	}
	for name, want := range designedTables {
		have := slices.Sorted(slices.Values(got[name]))
		if !slices.Equal(have, slices.Sorted(slices.Values(want))) {
			t.Errorf("namespace %s holds tables %v, want %v", name, have, want)
		}
	}
	for name := range got {
		if _, ok := designedTables[name]; !ok {
			t.Errorf("namespace %s is not in the design", name)
		}
	}
}

const digestsGolden = "testdata/digests.golden"

// A namespace's digest decides whether a stored file still opens, so it
// changes only on purpose: a changed line here is a format change.
func TestNamespaceDigestsArePinned(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	var b strings.Builder
	for _, ns := range stats(t, s).Namespaces {
		fmt.Fprintf(&b, "%s %s\n", ns.Name, hex.EncodeToString(ns.Format[:]))
	}
	got := []byte(b.String())
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(digestsGolden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(digestsGolden)
	if err != nil {
		t.Fatalf("%v; regenerate with UPDATE_GOLDEN=1 go test ./internal/store/ -run TestNamespaceDigestsArePinned", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("namespace digests changed:\n%s\nwant:\n%s\nA change is a store format change; regenerate with UPDATE_GOLDEN=1 go test ./internal/store/ -run TestNamespaceDigestsArePinned", got, want)
	}
}

func TestOpenRefusesASecondHolder(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	_, err := Open(path, Options{Build: "v1.0.0-dev.2"})
	if !errors.Is(err, bolttable.ErrLocked) {
		t.Errorf("a second Open = %v, want ErrLocked", err)
	}
	mustClose(t, s)
	mustClose(t, mustOpen(t, path, "v1.0.0-dev.2"))
}

func TestOpenRecordsTheBuildAndFormat(t *testing.T) {
	path := testPath(t)
	mustClose(t, mustOpen(t, path, "v1.0.0-dev.1"))
	s := mustOpen(t, path, "v1.0.0-dev.7")
	defer mustClose(t, s)
	if got := readMeta(t, s); got != (metaRow{Format: 1, Build: "v1.0.0-dev.7"}) {
		t.Errorf("meta = %+v, want format 1 and build v1.0.0-dev.7", got)
	}
}

func TestOpenRefusesANewerFormat(t *testing.T) {
	path := testPath(t)
	putMeta(t, path, `{"format":2,"build":"v2.0.0"}`)
	_, err := Open(path, Options{Build: "v1.0.0-dev.9"})
	if err == nil {
		t.Fatal("Open of a format 2 file succeeded")
	}
	for _, want := range []string{"v2.0.0", "format 2", "v1.0.0-dev.9", "format 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	lock, err := bolttable.LockPath(path, 0)
	if err != nil {
		t.Fatalf("the refused open kept the file's lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	db, err := bolttable.OpenReadOnly(path, schema, bolttable.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.View(t.Context(), func(r *bolttable.ReadTx) error {
		row, _, err := meta.Get(r, metaKey)
		if err == nil && row != (metaRow{Format: 2, Build: "v2.0.0"}) {
			t.Errorf("the refused open rewrote meta to %+v", row)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Within a format a row only gains optional fields (design §5.3 Upgrade):
// one an older build wrote lacks them, one a newer build wrote has more.
func TestMetaReadsOlderAndNewerRowsOfItsFormat(t *testing.T) {
	for name, raw := range map[string]string{
		"older": `{"format":1}`,
		"newer": `{"format":1,"build":"v1.9.0","later":{"x":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := testPath(t)
			putMeta(t, path, raw)
			s := mustOpen(t, path, "v1.0.0-dev.3")
			defer mustClose(t, s)
			if got := readMeta(t, s); got != (metaRow{Format: 1, Build: "v1.0.0-dev.3"}) {
				t.Errorf("meta = %+v after the open", got)
			}
		})
	}
}

func TestOpenCompactsPastItsThresholds(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	big := strings.Repeat("x", 8<<20)
	if err := s.db.Update(t.Context(), func(tx *bolttable.Tx) error {
		return meta.Put(tx, metaKey, &metaRow{Format: 1, Build: big})
	}); err != nil {
		t.Fatal(err)
	}
	mustClose(t, s)
	// The open records a short build, freeing the large row's pages.
	mustClose(t, mustOpen(t, path, "v1.0.0-dev.2"))
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("below", func(t *testing.T) {
		s := mustOpen(t, path, "v1.0.0-dev.2")
		defer mustClose(t, s)
		if s.db.OpenReport().Compacted {
			t.Error("compacted a file with under 64 MiB free")
		}
	})
	t.Run("past", func(t *testing.T) {
		old := compaction
		compaction = bolttable.CompactPolicy{MinFreeRatio: 0.25, MinFreeBytes: 1 << 20}
		t.Cleanup(func() { compaction = old })
		s := mustOpen(t, path, "v1.0.0-dev.2")
		defer mustClose(t, s)
		if !s.db.OpenReport().Compacted {
			t.Fatal("did not compact a file past the thresholds")
		}
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if after.Size() >= before.Size()/2 {
			t.Errorf("file is %d bytes after compaction, was %d", after.Size(), before.Size())
		}
	})
}
