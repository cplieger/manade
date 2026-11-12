package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// allowedModules is the dependency rule: beyond the standard library and the
// github.com/cplieger/ modules, the module links only actions/scaleset, its
// two dependencies and go-yaml.
var allowedModules = []string{
	"github.com/actions/scaleset",
	"github.com/golang-jwt/jwt/v5",
	"github.com/google/uuid",
	"go.yaml.in/yaml/v4",
}

func TestNoThirdPartyModuleBeyondTheAllowedSet(t *testing.T) {
	gomod, err := exec.CommandContext(t.Context(), "go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	// The integration tag adds the imports of the integration tests' files.
	list := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test", "-tags", "integration",
		"-f", "{{with .Module}}{{.Path}}{{end}}", "./...")
	list.Dir = filepath.Dir(strings.TrimSpace(string(gomod)))
	var stderr bytes.Buffer
	list.Stderr = &stderr
	out, err := list.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v\n%s", list.Dir, err, stderr.Bytes())
	}
	mods := strings.Fields(string(out))
	if !slices.Contains(mods, "github.com/cplieger/manade") {
		t.Fatalf("go list in %s named no package of github.com/cplieger/manade:\n%s", list.Dir, out)
	}
	var extra []string
	for _, mod := range mods {
		if !strings.HasPrefix(mod, "github.com/cplieger/") && !slices.Contains(allowedModules, mod) && !slices.Contains(extra, mod) {
			extra = append(extra, mod)
		}
	}
	if len(extra) > 0 {
		slices.Sort(extra)
		t.Errorf("go list -deps -test -tags integration ./... names modules outside the dependency rule: %s (allowed beyond github.com/cplieger/: %s)",
			strings.Join(extra, ", "), strings.Join(allowedModules, ", "))
	}
}
