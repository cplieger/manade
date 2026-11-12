package obs

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/slogx"
	"go.yaml.in/yaml/v4"
)

// shippedAlerts holds every alert name the rule files have carried: "" for
// one still in both files, else the alert now covering its condition or
// "removed: <why>". Deleting or renaming a rule fails the test until its
// entry here records the decision.
var shippedAlerts = map[string]string{
	"ManadeDown":                "",
	"ManadeBootStalled":         "",
	"ManadePollStalled":         "",
	"ManadeScopeNotServing":     "",
	"ManadeRunnerStartsFailing": "",
	"ManadeAssignedNotStarted":  "",
	"ManadeQueueBackedUp":       "",
	"ManadeQueueReadsStale":     "",
	"ManadeRunnerOOMKilled":     "",
	"ManadeReconcileStalled":    "",
	"ManadeImageStale":          "",
}

// rule is one alert rule: its expression and its for, "" when unset.
type rule struct{ expr, hold string }

// readRules returns alerts/<name>'s rules by alert name.
func readRules(t *testing.T, name string) map[string]rule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "alerts", name))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Groups []struct {
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
				For   string `yaml:"for"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	out := map[string]rule{}
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			if _, dup := out[r.Alert]; dup {
				t.Errorf("%s: alert %s twice", name, r.Alert)
			}
			out[r.Alert] = rule{r.Expr, r.For}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no rules", name)
	}
	return out
}

func TestAlertParityPromQLReadsServedSeries(t *testing.T) {
	served := servedSeries(t)
	for alert, r := range readRules(t, "promql.yaml") {
		t.Run(alert, func(t *testing.T) {
			names := metricNames(r.expr)
			if len(names) == 0 {
				t.Fatalf("no metric name in %q", r.expr)
			}
			for _, name := range names {
				// up is the scraper's own series, never on the page.
				if name != "up" && !served[name] {
					t.Errorf("reads %s, which /metrics does not serve", name)
				}
			}
		})
	}
}

func TestAlertParityFilesHoldTheSameAlerts(t *testing.T) {
	promql := readRules(t, "promql.yaml")
	logql := readRules(t, "logql.yaml")
	for _, f := range []struct {
		name  string
		rules map[string]rule
	}{{"promql.yaml", promql}, {"logql.yaml", logql}} {
		for alert := range f.rules {
			if now, ok := shippedAlerts[alert]; !ok || now != "" {
				t.Errorf("%s holds %s, which shippedAlerts does not record as current", f.name, alert)
			}
		}
	}
	for alert, now := range shippedAlerts {
		_, inProm := promql[alert]
		_, inLog := logql[alert]
		switch {
		case now == "" && (!inProm || !inLog):
			t.Errorf("%s left a rule file (promql %v, logql %v); record its successor or its removal", alert, inProm, inLog)
		case now != "" && !strings.HasPrefix(now, "removed: ") && (promql[now].expr == "" || logql[now].expr == ""):
			t.Errorf("%s's successor %s is not in both files", alert, now)
		}
	}
}

// stateTemplate is the label_format template the rules derive booting with:
// 0 on the line whose msg it captures, 1 on any other.
const stateTemplate = "`\\{\\{ if eq \\.msg \"([^\"]+)\" \\}\\}0\\{\\{ else \\}\\}1\\{\\{ end \\}\\}`"

// The LogQL shapes the rules use: line filters, alternatives joined by or,
// logfmt before the field unwrap reads, and booting derived from msg.
var (
	lineFilterRe = regexp.MustCompile("(?:\\|=|\\bor) `([^`]*)`")
	logfmtRe     = regexp.MustCompile(`\| logfmt \| unwrap (\w+)`)
	stateRe      = regexp.MustCompile("\\| logfmt\\s*\\| label_format booting=" + stateTemplate + "\\s*\\| unwrap booting\\b")
	logfmtPairRe = regexp.MustCompile(`(\w+)=("(?:[^"\\]|\\.)*"|\S*)`)
	integerRe    = regexp.MustCompile(`^-?[0-9]+$`)
)

// logLine is one line as slogx's text handler writes it, with its fields
// other than time, level and msg.
type logLine struct {
	fields map[string]string
	text   string
}

// recordLines answers, in order, the lines Boot, Booted and Pass write
// through slogx's text handler for a boot of 2.5 minutes and a second, then
// five passes: the start record, one boot record, the end line and one pass
// record.
func recordLines(t *testing.T) []logLine {
	t.Helper()
	var lines []logLine
	synctest.Test(t, func(t *testing.T) {
		var buf bytes.Buffer
		h, _ := slogx.NewHandler(slogx.Options{Output: &buf})
		m, err := newAt(load(t, defaultCapacity), slog.New(h), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			m.Boot(ctx)
		}()
		time.Sleep(bootEvery + time.Second)
		cancel()
		<-done
		m.Booted()
		for range passEvery {
			time.Sleep(30 * time.Second)
			m.Pass(t.Context(), true)
		}
		for text := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
			l := logLine{text: text, fields: map[string]string{}}
			for _, kv := range logfmtPairRe.FindAllStringSubmatch(text, -1) {
				if k := kv[1]; k != "time" && k != "level" && k != "msg" {
					l.fields[k] = kv[2]
				}
			}
			lines = append(lines, l)
		}
	})
	if len(lines) != 4 {
		t.Fatalf("want the start record, a boot record, the end line and a pass record, got %v", lines)
	}
	return lines
}

// TestAlertParityLogQLReadsTheRecords binds alerts/logql.yaml to the lines
// Boot, Booted and Pass write: each line carries the pool's POOL_NAME as
// pool, which every rule groups by, every line filter matches exactly one of
// them, every field a rule unwraps is an integer on a line its filters
// match, the booting template names one of those lines, and every field on
// the pass record is read by a rule.
func TestAlertParityLogQLReadsTheRecords(t *testing.T) {
	lines := recordLines(t)
	read := map[string]bool{"pool": true}
	for _, l := range lines {
		if l.fields["pool"] != testPool {
			t.Errorf("%q carries pool %q, want the POOL_NAME %s", l.text, l.fields["pool"], testPool)
		}
	}
	for alert, r := range readRules(t, "logql.yaml") {
		expr := r.expr
		t.Run(alert, func(t *testing.T) {
			filters := lineFilterRe.FindAllStringSubmatch(expr, -1)
			if len(filters) == 0 {
				t.Errorf("no line filter in %q", expr)
			}
			var matched []logLine
			for _, f := range filters {
				n := 0
				for _, l := range lines {
					if strings.Contains(l.text, f[1]) {
						matched = append(matched, l)
						n++
					}
				}
				if n != 1 {
					t.Errorf("line filter %q matches %d lines, want 1 of %v", f[1], n, lines)
				}
			}
			unwraps := logfmtRe.FindAllStringSubmatch(expr, -1)
			states := stateRe.FindAllStringSubmatch(expr, -1)
			if n := strings.Count(expr, "unwrap"); n != len(unwraps)+len(states) {
				t.Errorf("%d unwraps, %d in the `| logfmt | unwrap f` shape and %d in the booting template's: %q",
					n, len(unwraps), len(states), expr)
			}
			for _, st := range states {
				on := slices.ContainsFunc(matched, func(l logLine) bool { return strings.Contains(l.text, `msg="`+st[1]+`"`) })
				if !on {
					t.Errorf("the booting template's message %q is on no line the rule's filters match", st[1])
				}
			}
			for _, u := range unwraps {
				read[u[1]] = true
				if !slices.ContainsFunc(matched, func(l logLine) bool { return integerRe.MatchString(l.fields[u[1]]) }) {
					t.Errorf("no line the rule's filters match carries %s as an integer", u[1])
				}
			}
		})
	}
	// boot_seconds is for the reader of the boot lines; the pass record
	// exists for the rules, so each of its fields must be read.
	for _, l := range lines {
		if !strings.Contains(l.text, `msg="pass record"`) {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(l.fields)) {
			if !read[k] {
				t.Errorf("field %s of %q is read by no rule in alerts/logql.yaml", k, l.text)
			}
		}
	}
}

// servedSeries builds Metrics as cmd/manade does, with a disk budget, a
// Source and one observation per histogram, since a family with no sample
// prints no series, and returns the names /metrics serves.
func servedSeries(t *testing.T) map[string]bool {
	t.Helper()
	m, err := New(load(t, "capacity: {cpus: 16, memory: 64GiB, disk: 100GiB}"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	m.SetSource(func() Snapshot {
		return Snapshot{HeadSince: time.Now(), Counts: []Counts{{ScaleSet: appSmall}}}
	})
	m.JobAssigned("small", time.Second)
	m.JobFinished("small", JobSucceeded, time.Minute)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: %d", rec.Code)
	}
	out := map[string]bool{}
	for line := range strings.SplitSeq(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line[:strings.IndexAny(line, "{ ")]] = true
	}
	return out
}

var (
	promString   = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	promMatchers = regexp.MustCompile(`\{[^}]*\}|\[[^\]]*\]`)
	promLabels   = regexp.MustCompile(`\b(?:by|without|on|ignoring|group_left|group_right)\s*\([^)]*\)`)
	promToken    = regexp.MustCompile(`[A-Za-z_:][A-Za-z0-9_:]*(\s*\()?|[0-9][0-9A-Za-z_.]*`)
	promKeywords = map[string]bool{
		"and": true, "or": true, "unless": true, "offset": true, "bool": true,
		"by": true, "without": true, "on": true, "ignoring": true, "group_left": true, "group_right": true,
		"inf": true, "nan": true,
	}
)

// metricNames returns the metric names expr reads, sorted: every name that
// is not a function call, a keyword, a label or a number.
func metricNames(expr string) []string {
	expr = promString.ReplaceAllString(expr, `""`)
	expr = promMatchers.ReplaceAllString(expr, " ")
	expr = promLabels.ReplaceAllString(expr, " ")
	names := map[string]bool{}
	for _, m := range promToken.FindAllStringSubmatch(expr, -1) {
		tok := m[0]
		if m[1] != "" || tok[0] >= '0' && tok[0] <= '9' || promKeywords[strings.ToLower(tok)] {
			continue
		}
		names[tok] = true
	}
	return slices.Sorted(maps.Keys(names))
}
