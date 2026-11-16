//go:build integration

package hostit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

// smokeProfile is one profile member running image's images/<image>/smoke.sh
// in place of the runner.
func smokeProfile(t *testing.T, image string) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "images", image, "smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := json.Marshal(map[string]any{
		"runner_image": image, "scopes": []string{"acme/app"}, "cpus": 1, "memory": "3GiB",
		"command": []string{"/bin/bash", "-c", string(script), "smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return `"` + image + `": ` + string(p)
}

// TestImages runs the base, build and browser images' smoke tests as
// runners of an open host, each delivered to it over the channel.
func TestImages(t *testing.T) {
	images := []string{"base", "build", "browser"}
	cp := newControlPlane(t)
	h := startHost(t, cp, []string{"MANADE_ALLOW_OPEN=true"})
	open := `{"egress": {"open": true}}`
	if _, err := h.deliver(1, settings(open)); err != nil {
		t.Fatal(err)
	}
	h.up(hashOf(t, settings(open)))
	profiles := make([]string, 0, len(images))
	for _, name := range images {
		h.load(name)
		profiles = append(profiles, smokeProfile(t, name))
	}
	if _, err := h.deliver(2, settings(open, profiles...)); err != nil {
		t.Fatal(err)
	}
	h.await("every profile ready", 10*time.Minute, func(st *hostapi.Status) bool {
		return len(st.Profiles) == len(images) && !slices.ContainsFunc(st.Profiles, func(p hostapi.ProfileStatus) bool { return !p.Ready })
	})
	for _, name := range images {
		t.Run(name, func(t *testing.T) {
			code, logs := h.run(t, name, 20*time.Minute)
			if code != 0 || !strings.Contains(logs, "manade-runner "+name+" smoke: ok") {
				t.Fatalf("exit %d, logs:\n%s", code, logs)
			}
		})
	}
}
