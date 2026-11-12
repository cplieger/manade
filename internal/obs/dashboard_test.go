package obs

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// dashboard is what the contract tests read from grafana-dashboard.json.
type dashboard struct {
	panels    map[string]any
	attention []any // the Needs attention row's auto-grid items
	queries   []dashQuery
	preload   bool
}

type dashQuery struct{ where, expr string }

func readDashboard(t *testing.T) dashboard {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "grafana-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	spec := dig(doc, "spec")
	d := dashboard{panels: map[string]any{}, preload: dig(spec, "preload") == true}
	for name, el := range asMap(dig(spec, "elements")) {
		d.panels[name] = dig(el, "spec")
		for _, q := range list(dig(el, "spec", "data", "spec", "queries")) {
			d.queries = append(d.queries, dashQuery{name, str(dig(q, "spec", "query", "spec", "expr"))})
		}
	}
	for _, v := range list(dig(spec, "variables")) {
		if q := str(dig(v, "spec", "query", "spec", "query")); q != "" {
			d.queries = append(d.queries, dashQuery{"variable " + str(dig(v, "spec", "name")), q})
		}
	}
	for _, tab := range list(dig(spec, "layout", "spec", "tabs")) {
		for _, row := range list(dig(tab, "spec", "layout", "spec", "rows")) {
			if str(dig(row, "spec", "title")) == "Needs attention" {
				d.attention = list(dig(row, "spec", "layout", "spec", "items"))
			}
		}
	}
	return d
}

func TestDashboardQueriesReadServedSeries(t *testing.T) {
	served := servedSeries(t)
	d := readDashboard(t)
	if len(d.queries) < len(d.panels) {
		t.Fatalf("%d queries for %d panels, want at least one each", len(d.queries), len(d.panels))
	}
	for _, q := range d.queries {
		names := metricNames(q.expr)
		if len(names) == 0 {
			t.Errorf("%s: no metric name in %q", q.where, q.expr)
		}
		for _, name := range names {
			// ALERTS is the ruler's own series for the shipped rules.
			if name != "ALERTS" && !served[name] {
				t.Errorf("%s reads %s, which /metrics does not serve", q.where, name)
			}
		}
	}
}

var (
	alertSelectorRe = regexp.MustCompile(`ALERTS\{([^}]*)\}`)
	alertNameRe     = regexp.MustCompile(`^alertname="([^"]+)", alertstate="firing"$`)
)

// firingAlerts returns the alert names expr reads, failing when a selector
// is not the firing state of one named alert.
func firingAlerts(t *testing.T, where, expr string) []string {
	t.Helper()
	var out []string
	for _, m := range alertSelectorRe.FindAllStringSubmatch(expr, -1) {
		name := alertNameRe.FindStringSubmatch(m[1])
		if name == nil {
			t.Errorf("%s: %s is not one alert's firing series", where, m[0])
			continue
		}
		out = append(out, name[1])
	}
	slices.Sort(out)
	return out
}

func TestDashboardAttentionTilesMirrorEveryAlert(t *testing.T) {
	want := slices.Sorted(maps.Keys(readRules(t, "promql.yaml")))
	d := readDashboard(t)
	var tiles []string
	for _, item := range d.attention {
		name := str(dig(item, "spec", "element", "name"))
		queries := list(dig(d.panels[name], "data", "spec", "queries"))
		if len(queries) != 1 {
			t.Errorf("tile %s has %d queries, want 1", name, len(queries))
			continue
		}
		alerts := firingAlerts(t, name, str(dig(queries[0], "spec", "query", "spec", "expr")))
		if len(alerts) != 1 {
			t.Errorf("tile %s reads alerts %v, want exactly one", name, alerts)
		}
		tiles = append(tiles, alerts...)
	}
	slices.Sort(tiles)
	if !slices.Equal(tiles, want) {
		t.Errorf("attention tiles mirror %v, want one per rule in alerts/promql.yaml: %v", tiles, want)
	}
	i := slices.IndexFunc(d.queries, func(q dashQuery) bool { return q.where == "variable attention" })
	if i < 0 {
		t.Fatal("no attention variable")
	}
	if got := firingAlerts(t, "variable attention", d.queries[i].expr); !slices.Equal(got, want) {
		t.Errorf("the attention count reads %v, want %v, so the row shows exactly when a tile does", got, want)
	}
}

// A has-data rule is undecided while its query loads, and an undecided group
// shows its item, so each tile also needs an always-false rule, a pinned
// maxDataPoints and the dashboard's preload (grafana/grafana#134713).
func TestDashboardAttentionTilesStartHidden(t *testing.T) {
	d := readDashboard(t)
	if !d.preload {
		t.Error("preload is off, so a tile below the fold never queries and never hides")
	}
	if len(d.attention) == 0 {
		t.Fatal("no Needs attention tiles")
	}
	for _, item := range d.attention {
		name := str(dig(item, "spec", "element", "name"))
		group := dig(item, "spec", "conditionalRendering", "spec")
		var data, never bool
		for _, rule := range list(dig(group, "items")) {
			switch str(dig(rule, "kind")) {
			case "ConditionalRenderingData":
				data = dig(rule, "spec", "value") == true
			case "ConditionalRenderingTimeRangeSize":
				never = str(dig(rule, "spec", "value")) == "0s"
			}
		}
		if str(dig(group, "condition")) != "or" || !data || !never {
			t.Errorf("tile %s: rules %v, want has-data or a 0s time range", name, group)
		}
		if dig(d.panels[name], "data", "spec", "queryOptions", "maxDataPoints") == nil {
			t.Errorf("tile %s pins no maxDataPoints", name)
		}
	}
}

func dig(v any, keys ...string) any {
	for _, k := range keys {
		v = asMap(v)[k]
	}
	return v
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func list(v any) []any { l, _ := v.([]any); return l }

func str(v any) string { s, _ := v.(string); return s }
