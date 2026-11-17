package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/bolttable"
)

// stagedFile leaves beside path a store file another build wrote, holding
// one host's record, and answers its path.
func stagedFile(t *testing.T, path, build, host string) string {
	t.Helper()
	staged := filepath.Join(filepath.Dir(path), "staged.db")
	s := mustOpen(t, staged, build)
	update(t, s, func(h *HistoryTx) error { return h.PutHostSeen(host, &HostSeen{State: "serving"}) })
	mustClose(t, s)
	return staged
}

// hostSeenOf answers host's record in the store's open file.
func hostSeenOf(t *testing.T, s *Store, host string) (seen HostSeen, found bool) {
	t.Helper()
	if err := s.live.View(t.Context(), func(r *bolttable.ReadTx) (err error) {
		seen, found, err = hostSeen.Get(r, []byte(host))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return seen, found
}

func swap(s *Store, staged string) error {
	return s.live.Swap(context.Background(), func() error { return os.Rename(staged, s.path) })
}

// A file swapped in under the store reopens through its hooks: the stamp
// names this build again, reads see the new file's rows, and work pinned
// to the file it replaced answers ErrClosed instead of writing there.
func TestASwappedFileOpensUnderTheStoresHooks(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1.0.0-dev.5")
	defer mustClose(t, s)
	old := pinned(t, s)
	if err := swap(s, stagedFile(t, path, "v1.0.0-dev.2", "voyager")); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	if got := readStampOf(t, s); got != (stamp{Format: 1, Build: "v1.0.0-dev.5"}) {
		t.Errorf("stamp after the swap = %+v, want this build's", got)
	}
	if seen, found := hostSeenOf(t, s, "voyager"); !found || seen.State != "serving" {
		t.Errorf("voyager after the swap = %+v, %v; want the new file's row", seen, found)
	}
	err := old.Update(t.Context(), func(tx *bolttable.Tx) error { return (&HistoryTx{tx: tx}).PutHostSeen("borgcube", &HostSeen{}) })
	if !errors.Is(err, bolttable.ErrClosed) {
		t.Errorf("a write pinned to the replaced file = %v, want ErrClosed", err)
	}
	if _, found := hostSeenOf(t, s, "borgcube"); found {
		t.Error("the pinned write reached the new file")
	}
}

// A newer build's file swapped in is refused as Open refuses it, and the
// store stays closed: every write is a skipped one, never a write failure.
func TestASwappedNewerFileLeavesTheStoreClosed(t *testing.T) {
	path := testPath(t)
	s := mustOpen(t, path, "v1.0.0-dev.5")
	defer mustClose(t, s)
	staged := filepath.Join(filepath.Dir(path), "staged.db")
	writeFile(t, staged, map[string]string{"store": `{"format":2,"build":"v2.0.0"}`}, nil)
	if err := swap(s, staged); !errors.Is(err, bolttable.ErrNewerBuild) {
		t.Fatalf("Swap = %v, want ErrNewerBuild", err)
	}
	err := s.RecordRoute(t.Context(), &Route{Scope: "acme/app", Variable: "MANADE", Value: "small"})
	if !errors.Is(err, ErrClosed) {
		t.Errorf("a write on the closed store = %v, want ErrClosed", err)
	}
	if _, err := s.Routes(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("a read on the closed store = %v, want ErrClosed", err)
	}
	if _, err := s.Size(); !errors.Is(err, ErrClosed) {
		t.Errorf("Size on the closed store = %v, want ErrClosed", err)
	}
	if n := s.WriteFailures(); n != 0 || !s.Healthy() {
		t.Errorf("WriteFailures = %d, Healthy = %v; a skipped write is no failure", n, s.Healthy())
	}
}
