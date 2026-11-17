package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/bolttable"
	"github.com/cplieger/go/slogx/capture"
)

func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), File)
}

// open locks and opens the file at path as build, answering the open's
// error with the store still locked.
func open(t *testing.T, path, build string) (*Store, error) {
	t.Helper()
	s, err := New(path, Options{Build: build, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, s.Open()
}

func mustOpen(t *testing.T, path, build string) *Store {
	t.Helper()
	s, err := open(t, path, build)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// pinned answers the store's open DB.
func pinned(t *testing.T, s *Store) *bolttable.DB {
	t.Helper()
	db, err := s.live.Pin()
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	return db
}

func mustClose(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func stats(t *testing.T, s *Store) bolttable.Stats {
	t.Helper()
	st, err := pinned(t, s).Stats(t.Context())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	return st
}

// writeFile leaves a file at path as a later build would: every namespace
// this build declares, rows in meta, and a namespace named later when later
// holds any.
func writeFile(t *testing.T, path string, rows, later map[string]string) {
	t.Helper()
	s, d := declare(bolttable.NewSchema(schemaName))
	m := d.meta
	var l *bolttable.Table[[]byte, json.RawMessage]
	if later != nil {
		l = bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("later")), 0xef, "rows",
			bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	}
	db, err := bolttable.Open(path, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	put := func(tx *bolttable.Tx, tb *bolttable.Table[[]byte, json.RawMessage], rows map[string]string) error {
		for _, k := range slices.Sorted(maps.Keys(rows)) {
			v := json.RawMessage(rows[k])
			if err := tb.Put(tx, []byte(k), &v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error {
		if err := put(tx, m, rows); err != nil || l == nil {
			return err
		}
		return put(tx, l, later)
	}); err != nil {
		t.Fatalf("write the file: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func readRaw(t *testing.T, s *Store, key string) (json.RawMessage, bool) {
	t.Helper()
	var raw json.RawMessage
	var found bool
	err := s.live.View(t.Context(), func(r *bolttable.ReadTx) (err error) {
		raw, found, err = meta.Get(r, []byte(key))
		return err
	})
	if err != nil {
		t.Fatalf("read meta %s: %v", key, err)
	}
	return raw, found
}

// journalRows answers every journal row in key order.
func journalRows(t *testing.T, s *Store) []Entry {
	t.Helper()
	var out []Entry
	err := s.live.View(t.Context(), func(r *bolttable.ReadTx) error {
		return journal.Scan(r, bolttable.Range{}, func(_ uint64, e *Entry) (bool, error) {
			out = append(out, *e)
			return true, nil
		})
	})
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	return out
}

func readStampOf(t *testing.T, s *Store) stamp {
	t.Helper()
	var st stamp
	err := s.live.View(t.Context(), func(r *bolttable.ReadTx) error {
		var found bool
		var err error
		st, found, err = readStamp(r)
		if err == nil && !found {
			err = errors.New("no stamp")
		}
		return err
	})
	if err != nil {
		t.Fatalf("read the stamp: %v", err)
	}
	return st
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

// lowCompaction makes a file with a few MiB free compact at open.
func lowCompaction(t *testing.T) {
	t.Helper()
	old := compaction
	compaction = bolttable.CompactPolicy{MinFreeRatio: 0.25, MinFreeBytes: 1 << 20}
	t.Cleanup(func() { compaction = old })
}

// freePages writes an 8 MiB row under key and deletes it, leaving its pages
// free in the file.
func freePages(t *testing.T, path string) {
	t.Helper()
	s := bolttable.NewSchema(schemaName)
	m := bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("meta")), prefixMeta, "meta",
		bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	db, err := bolttable.Open(path, s, bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	big := json.RawMessage(`"` + strings.Repeat("x", 8<<20) + `"`)
	key := []byte("big")
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error { return m.Put(tx, key, &big) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(t.Context(), func(tx *bolttable.Tx) error { _, err := m.Delete(tx, key); return err }); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenDeclaresItsNamespaces(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	got := map[string][]string{}
	for _, ns := range stats(t, s).Namespaces {
		for _, tb := range ns.Tables {
			got[ns.Name] = append(got[ns.Name], tb.Name)
		}
	}
	want := map[string][]string{
		"github": {"scalesets", "routes"}, "journal": {"journal"}, "meta": {"meta"},
		"history": {"runs", "runblobs", "exits", "hostevents", "probes", "hostseen"},
	}
	if !maps.EqualFunc(got, want, slices.Equal) {
		t.Errorf("namespaces = %v, want %v", got, want)
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

func TestNewRefusesASecondHolder(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1.0.0-dev.1")
	_, err := New(path, Options{Build: "v1.0.0-dev.2"})
	if !errors.Is(err, bolttable.ErrLocked) || !strings.Contains(err.Error(), path) {
		t.Errorf("a second New = %v, want ErrLocked naming %s", err, path)
	}
	if _, ok := bolttable.OpenFailure(err); ok {
		t.Errorf("a held lock is classed as an outage: %v", err)
	}
	mustClose(t, s)
	mustClose(t, mustOpen(t, path, "v1.0.0-dev.2"))
}

func TestOpenRecordsTheBuildAndFormat(t *testing.T) {
	path := testPath(t)
	mustClose(t, mustOpen(t, path, "v1.0.0-dev.1"))
	s := mustOpen(t, path, "v1.0.0-dev.7")
	defer mustClose(t, s)
	if got := readStampOf(t, s); got != (stamp{Format: 1, Build: "v1.0.0-dev.7"}) {
		t.Errorf("stamp = %+v, want format 1 and build v1.0.0-dev.7", got)
	}
}

// A file an older build wrote, before a namespace this build declares
// existed, opens and gains it: the read-only check leaves that file to Open.
func TestOpenCompletesAFileLackingANamespace(t *testing.T) {
	path := testPath(t)
	db, err := bolttable.Open(path, bolttable.NewSchema(schemaName), bolttable.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t, path, "v1.0.0-dev.3")
	defer mustClose(t, s)
	if got := readStampOf(t, s); got != (stamp{Format: 1, Build: "v1.0.0-dev.3"}) {
		t.Errorf("stamp = %+v, want format 1 and build v1.0.0-dev.3", got)
	}
}

// A refused file is left byte for byte as it was, its lock file too: no
// compaction, no namespace created, no stamp rewritten. The store keeps the
// lock until Close, and a newer build's stamp is refused whatever else the
// file holds.
func TestOpenRefusesALaterFileUnchanged(t *testing.T) {
	lowCompaction(t)
	for _, tc := range []struct {
		name, stamp, class string
		later              map[string]string
		want               []string
	}{
		{"laterFormat", `{"format":2,"build":"v2.0.0"}`, "newer", nil, []string{"v2.0.0 wrote", "format 2", "v1.0.0-dev.9", "format 1", "run v2.0.0 or later"}},
		{"laterFormatUnknownBuild", `{"format":2}`, "newer", nil, []string{"of an unknown build", "format 2", "run a newer build or later"}},
		{"laterFormatWithANamespaceItAdded", `{"format":2,"build":"v2.0.0"}`, "newer", map[string]string{"k": `{}`}, []string{"format 2"}},
		{"undeclaredNamespace", `{"format":1,"build":"v1.4.0"}`, "format", map[string]string{"k": `{}`}, []string{"later"}},
		{"stampThatDoesNotDecode", `"v1.4.0"`, "damaged", nil, []string{"meta store"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testPath(t)
			freePages(t, path)
			writeFile(t, path, map[string]string{"store": tc.stamp}, tc.later)
			before := readFile(t, path)
			lock := path + ".lock"
			lockBefore := readFile(t, lock)
			s, err := open(t, path, "v1.0.0-dev.9")
			if err == nil {
				t.Fatal("Open succeeded")
			}
			for _, want := range append(tc.want, path) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if class, _ := bolttable.OpenFailure(err); class != tc.class {
				t.Errorf("OpenFailure = %q, want %q: %v", class, tc.class, err)
			}
			if got := errors.Is(err, bolttable.ErrNewerBuild); got != (tc.class == "newer") {
				t.Errorf("errors.Is(ErrNewerBuild) = %v for %v", got, err)
			}
			if !bytes.Equal(readFile(t, path), before) {
				t.Error("the refused open changed the file")
			}
			if !bytes.Equal(readFile(t, lock), lockBefore) {
				t.Error("the refused open changed the lock file")
			}
			if _, err := bolttable.LockPath(path, 0); !errors.Is(err, bolttable.ErrLocked) {
				t.Errorf("the refused store let its lock go before Close: %v", err)
			}
			mustClose(t, s)
			lockFree(t, path)
		})
	}
}

// A brand-new file, absent or empty, has nothing to inspect: its first
// open creates it and writes the stamp.
func TestOpenStampsABrandNewFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"absent", func(*testing.T, string) {}},
		{"empty", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testPath(t)
			tc.write(t, path)
			s := mustOpen(t, path, "v1.0.0-dev.4")
			defer mustClose(t, s)
			if got := readStampOf(t, s); got != (stamp{Format: 1, Build: "v1.0.0-dev.4"}) {
				t.Errorf("stamp = %+v, want format 1 and build v1.0.0-dev.4", got)
			}
		})
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Within a format a row only gains optional fields: one an older build
// wrote lacks them, one a newer build wrote has more.
func TestStampReadsOlderAndNewerRowsOfItsFormat(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      stamp
	}{
		{"older", `{"format":1}`, stamp{Format: 1}},
		{"newer", `{"format":1,"build":"v1.9.0","later":{"x":1}}`, stamp{Format: 1, Build: "v1.9.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := testPath(t)
			writeFile(t, path, map[string]string{"store": tc.raw}, nil)
			db, err := bolttable.OpenReadOnly(path, schema, bolttable.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var got stamp
			err = db.View(t.Context(), func(r *bolttable.ReadTx) (err error) {
				got, _, err = readStamp(r)
				return err
			})
			if err = errors.Join(err, db.Close()); err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("stamp = %+v, want %+v", got, tc.want)
			}
			s := mustOpen(t, path, "v1.0.0-dev.3")
			defer mustClose(t, s)
			if got := readStampOf(t, s); got != (stamp{Format: 1, Build: "v1.0.0-dev.3"}) {
				t.Errorf("stamp = %+v after the open", got)
			}
		})
	}
}

// The open rewrites the stamp alone: a fact under another key survives it.
func TestOpenKeepsEveryOtherMetaRow(t *testing.T) {
	path := testPath(t)
	const salt = `{"salt":"3q2+7wABAgMEBQYHCAkKCw==","kdf":"argon2id"}`
	writeFile(t, path, map[string]string{"store": `{"format":1,"build":"v1.9.0"}`, "salt": salt}, nil)
	s := mustOpen(t, path, "v1.0.0-dev.3")
	defer mustClose(t, s)
	if raw, found := readRaw(t, s, "salt"); !found || string(raw) != salt {
		t.Errorf("salt = %s (found %v), want %s", raw, found, salt)
	}
}

func TestOpenCompactsPastItsThresholds(t *testing.T) {
	path := testPath(t)
	freePages(t, path)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("below", func(t *testing.T) {
		s := mustOpen(t, path, "v1.0.0-dev.2")
		defer mustClose(t, s)
		if pinned(t, s).OpenReport().Compacted {
			t.Error("compacted a file with under 64 MiB free")
		}
	})
	t.Run("past", func(t *testing.T) {
		lowCompaction(t)
		log, rec := capture.New()
		s, err := New(path, Options{Build: "v1.0.0-dev.2", Log: log})
		if err != nil {
			t.Fatal(err)
		}
		defer mustClose(t, s)
		if err := s.Open(); err != nil {
			t.Fatal(err)
		}
		if !pinned(t, s).OpenReport().Compacted {
			t.Fatal("did not compact a file past the thresholds")
		}
		if rec.CountLevel(slog.LevelInfo, "The store was compacted at open") != 1 {
			t.Errorf("the open did not log its compaction: %v", rec.Messages())
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

// A failed compaction leaves the file as it was and the start goes on
// (bolttable's TestOpenCompactionFailureContinues); the log tells it from a
// compaction whose directory sync failed after the swap.
func TestLogOpen(t *testing.T) {
	syncErr := errors.New("fsync: input/output error")
	for _, tc := range []struct {
		name   string
		report bolttable.OpenReport
		level  slog.Level
		msg    string
	}{
		{"compacted", bolttable.OpenReport{Compacted: true}, slog.LevelInfo, "was compacted at open"},
		{"notCompacted", bolttable.OpenReport{CompactErr: syncErr}, slog.LevelWarn, "could not be compacted"},
		{"compactedUnsynced", bolttable.OpenReport{Compacted: true, CompactErr: syncErr}, slog.LevelWarn, "directory did not sync"},
		{"nothing", bolttable.OpenReport{}, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, rec := capture.New()
			logOpen(log, &tc.report)
			got := rec.Records()
			if tc.msg == "" {
				if len(got) != 0 {
					t.Errorf("logged %d records, want none", len(got))
				}
				return
			}
			if len(got) != 1 || got[0].Level != tc.level || !strings.Contains(got[0].Message, tc.msg) {
				t.Fatalf("logged %+v, want one %v record holding %q", got, tc.level, tc.msg)
			}
			if tc.report.CompactErr == nil {
				return
			}
			if v, ok := rec.Attr(capture.Message(tc.msg), "error"); !ok || v.Any() != syncErr {
				t.Errorf("error attribute = %v, want %v", v, syncErr)
			}
		})
	}
}
