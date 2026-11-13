package jobspec

import (
	"os"
	"regexp"
	"testing"

	"github.com/cplieger/manade/internal/config"
)

func TestImageRef(t *testing.T) {
	for version, want := range map[string]string{
		"v1.0.0-dev.9":  RunnerRepo + ":base-v1-dev",
		"v1.2.0":        RunnerRepo + ":base-v1",
		"v2.0.0":        RunnerRepo + ":base-v2",
		"v2.1.0-rc.1":   RunnerRepo + ":base-v2-dev",
		"dev":           RunnerRepo + ":base-v1-dev",
		"ci":            RunnerRepo + ":base-v1-dev",
		"v1.2":          RunnerRepo + ":base-v1-dev",
		"v1.2.3\nextra": RunnerRepo + ":base-v1-dev",
	} {
		if got := ImageRef(&config.Profile{RunnerImage: "base"}, version); got != want {
			t.Errorf("ImageRef at %q = %s, want %s", version, got, want)
		}
	}
	if got := ImageRef(&config.Profile{Image: "registry.example/team/runner:7"}, "v1.2.0"); got != "registry.example/team/runner:7" {
		t.Errorf("ImageRef of an image field = %s", got)
	}
}

func TestModuleMajorMatchesGoMod(t *testing.T) {
	b, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^module (\S+)$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("go.mod names no module")
	}
	if suffix := regexp.MustCompile(`/v(\d+)$`).Find(m[1]); suffix != nil || moduleMajor != 1 {
		t.Errorf("module %s with moduleMajor %d: a /vN module path needs moduleMajor N", m[1], moduleMajor)
	}
}
