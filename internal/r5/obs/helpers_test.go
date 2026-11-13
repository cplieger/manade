package obs

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/r5/config"
)

// Two scopes and two profiles make three scale sets: small in both scopes,
// and the elevated dind in acme/app only.
const layout = `scopes:
  - repo: acme/app
  - org: acme
profiles:
  small: {}
  dind: {network: bridge, scopes: [acme/app]}
`

var (
	appSmall = ScaleSet{"acme/app", "small"}
	orgSmall = ScaleSet{"acme", "small"}
	appDind  = ScaleSet{"acme/app", "dind"}
)

var start = time.Unix(1_000_000, 0)

// testPool is every test config's POOL_NAME. It differs from config's
// default, so a newAt that writes a constant pool fails the field's tests.
const testPool = "ci-pool"

// load validates capacity plus layout through config.Load, so the scale
// sets a test sees come from config's own Allowed.
func load(t *testing.T, capacity string) *config.Config {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(capacity+"\n"+layout), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"CONFIG_FILE": file, "GITHUB_TOKEN": "test-token", "POOL_NAME": testPool, "DOCKER_SOCKET": "/var/run/docker.sock",
		"GITHUB_APP_CLIENT_ID": "", "GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_FILE": "", "GITHUB_TOKEN_FILE": "",
	} {
		t.Setenv(k, v)
	}
	cfg, err := config.Load(t.Context())
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// clock is a settable now for newAt.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// newTest builds Metrics at start on a settable clock, logging to a recorder.
func newTest(t *testing.T, capacity string) (*Metrics, *capture.Recorder, *clock) {
	t.Helper()
	log, rec := capture.New()
	c := &clock{start}
	m, err := newAt(load(t, capacity), log, c.now)
	if err != nil {
		t.Fatalf("newAt: %v", err)
	}
	return m, rec, c
}

const defaultCapacity = "capacity: {cpus: 16, memory: 64GiB}"

// gather reads every sample of m as "name{v1,v2,...}" to its value.
func gather(t *testing.T, m *Metrics) map[string]float64 {
	t.Helper()
	fams, err := m.reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	out := map[string]float64{}
	for _, f := range fams {
		for _, s := range f.Samples {
			key := f.Name + "{"
			for i, l := range s.Labels {
				if i > 0 {
					key += ","
				}
				key += l.Value
			}
			key += "}"
			if s.Histogram != nil {
				out[key] = float64(s.Histogram.Count)
				continue
			}
			out[key] = s.Value
		}
	}
	return out
}

// count answers how many samples of family name m holds.
func count(t *testing.T, m *Metrics, name string) int {
	t.Helper()
	fams, err := m.reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		if f.Name == name {
			return len(f.Samples)
		}
	}
	return -1
}

// passRecord answers the attributes of the last pass record rec holds,
// failing unless it holds exactly n.
func passRecord(t *testing.T, rec *capture.Recorder, n int) map[string]slog.Value {
	t.Helper()
	var found []slog.Record
	for _, r := range rec.Records() {
		if r.Message == "pass record" {
			found = append(found, r)
		}
	}
	if len(found) != n {
		t.Fatalf("pass records = %d, want %d", len(found), n)
	}
	last := found[n-1]
	if last.Level != slog.LevelInfo {
		t.Errorf("pass record level = %v, want INFO", last.Level)
	}
	attrs := map[string]slog.Value{}
	last.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value
		return true
	})
	return attrs
}
