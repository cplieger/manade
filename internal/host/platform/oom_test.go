package platform

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func readSaved(t *testing.T, path string) oomFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f oomFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOOMTotalSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manade", "oom-kills.json")
	first, err := LoadOOMTotal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := first.Observe(t.Context(), 2); err != nil || got != 2 {
		t.Fatalf("Observe(2) = %d, %v, want 2", got, err)
	}
	if f := readSaved(t, path); f.Total != 2 || f.Count != 2 {
		t.Errorf("saved %+v, want total 2 at count 2", f)
	}
	// A restarted host's recreated cgroup counts from 0.
	second, err := LoadOOMTotal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.Observe(t.Context(), 0); err != nil || got != 2 {
		t.Errorf("Observe(0) after the restart = %d, %v, want the saved 2", got, err)
	}
	if got, err := second.Observe(t.Context(), 1); err != nil || got != 3 {
		t.Errorf("Observe(1) after the restart = %d, %v, want 3", got, err)
	}
	if f := readSaved(t, path); f.Total != 3 || f.Count != 1 {
		t.Errorf("saved %+v, want total 3 at count 1", f)
	}
}

func TestOOMTotalSavesOnlyOnAChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oom-kills.json")
	tot, err := LoadOOMTotal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tot.Observe(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a count of 0 on a new volume wrote the file (%v)", err)
	}
	if _, err := tot.Observe(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := tot.Observe(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an unchanged count wrote the file again (%v)", err)
	}
}

func TestOOMTotalRetriesAFailedSave(t *testing.T) {
	dir := t.TempDir()
	block := filepath.Join(dir, "blocked")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(block, "oom-kills.json")
	// The load fails too, naming the path, and still answers a usable total.
	tot, err := LoadOOMTotal(t.Context(), path)
	if err == nil {
		t.Error("loading under a file answered nil")
	}
	if got, err := tot.Observe(t.Context(), 1); err == nil || got != 1 {
		t.Fatalf("Observe(1) under a file = %d, %v, want 1 and an error", got, err)
	}
	if err := os.Remove(block); err != nil {
		t.Fatal(err)
	}
	if _, err := tot.Observe(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if f := readSaved(t, path); f.Total != 1 {
		t.Errorf("saved %+v, want the retried total 1", f)
	}
}

func TestLoadOOMTotalUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oom-kills.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	tot, err := LoadOOMTotal(t.Context(), path)
	if err == nil {
		t.Error("err = nil, want the unreadable file named")
	}
	if got, _ := tot.Observe(t.Context(), 4); got != 4 {
		t.Errorf("Observe(4) = %d, want 4 counted from 0", got)
	}
}

func TestSetOOMScoreAdj(t *testing.T) {
	r := tree(t, map[string]string{"/proc/7/oom_score_adj": "0\n"})
	if err := SetOOMScoreAdj(r, 7, HostOOMScoreAdj); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.read("/proc/7/oom_score_adj"); got != "-999" {
		t.Errorf("oom_score_adj = %q, want -999", got)
	}
	if err := SetOOMScoreAdj(r, 8, HostOOMScoreAdj); err == nil {
		t.Error("a missing process answered nil")
	}
}
