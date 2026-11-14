package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPKI(t *testing.T) {
	t.Run("issues the pool", func(t *testing.T) {
		dir := t.TempDir()
		var out bytes.Buffer
		if code := run([]string{"pki", dir, "nas"}, &out); code != 0 {
			t.Fatalf("run = %d: %s", code, out.String())
		}
		for _, f := range []string{"ca/ca.pem", "ca/ca-key.pem", "orchestrator/orchestrator.pem", "nas/nas-key.pem"} {
			if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		}
		if !strings.Contains(out.String(), "wrote "+filepath.Join(dir, "nas")) || !strings.Contains(out.String(), "keep it off every machine") {
			t.Errorf("output = %q", out.String())
		}
	})
	t.Run("a refused host exits 1", func(t *testing.T) {
		var out bytes.Buffer
		if code := run([]string{"pki", t.TempDir(), "NAS"}, &out); code != 1 || !strings.Contains(out.String(), "lowercase DNS label") {
			t.Errorf("run = %d: %s", code, out.String())
		}
	})
	t.Run("no directory is a usage error", func(t *testing.T) {
		var out bytes.Buffer
		if code := run([]string{"pki"}, &out); code != 2 || !strings.Contains(out.String(), pkiUsage) {
			t.Errorf("run = %d: %s", code, out.String())
		}
	})
}
