package obs

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Loki ships no offline rule test (no lokitool command evaluates a rule), so
// this file evaluates alerts/logql.yaml's expressions itself, as Loki's
// ruler does: a range [w] at t holds the lines in (t-w, t], a range
// aggregation over no line answers no series, absent_over_time then answers
// the selector's matchers, a binary operator matches series on every label,
// and a rule fires for a series once it has held at every 1-minute
// evaluation for its `for`.

// logEntry is one line of one stream, written at an offset into a scenario,
// with its logfmt fields unquoted.
type logEntry struct {
	at     time.Duration
	stream map[string]string
	text   string
	fields map[string]string
}

// series is one sample of an instant vector.
type series struct {
	labels map[string]string
	v      float64
}

// vector is an instant vector, keyed by labelKey.
type vector map[string]series

// logqlExpr evaluates at one instant over log, sorted by time.
type logqlExpr func(log []logEntry, at time.Duration) vector

// labelKey is labels' identity; an empty value is no label, as in Loki.
func labelKey(labels map[string]string) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		if labels[k] != "" {
			parts = append(parts, k+"="+strconv.Quote(labels[k]))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// rangeShape is what one range aggregation reads and how it is grouped.
type rangeShape struct {
	fn, selector string
	logfmt       bool
	by           []string // nil when ungrouped
}

var logqlTokenRe = regexp.MustCompile("^\\s*(\\{[^}]*\\}|`[^`]*`|\\[[0-9]+[smh]\\]|\\|=|==|<=|[|(),>=]|[A-Za-z_]+|[0-9]+)")

var (
	matcherRe        = regexp.MustCompile(`^\s*([a-z_]+)="([^"\\]*)"\s*$`)
	stateTemplateRe  = regexp.MustCompile("^" + stateTemplate + "$")
	logqlComparisons = []string{">", "==", "<="}
)

type logqlParser struct {
	t      *testing.T
	toks   []string
	shapes []*rangeShape
}

// parseLogQL parses the subset of LogQL the rule file uses and refuses
// anything else, so a rule outside it cannot pass unevaluated. It also
// answers the shape of every range aggregation in src.
func parseLogQL(t *testing.T, src string) (logqlExpr, []*rangeShape, error) {
	p := &logqlParser{t: t}
	for rest := src; strings.TrimSpace(rest) != ""; {
		m := logqlTokenRe.FindStringSubmatch(rest)
		if m == nil {
			return nil, nil, fmt.Errorf("cannot read %q", rest)
		}
		p.toks = append(p.toks, m[1])
		rest = rest[len(m[0]):]
	}
	e, err := p.union()
	if err == nil && len(p.toks) > 0 {
		err = fmt.Errorf("left over: %v", p.toks)
	}
	return e, p.shapes, err
}

func (p *logqlParser) next() string {
	if len(p.toks) == 0 {
		return ""
	}
	tok := p.toks[0]
	p.toks = p.toks[1:]
	return tok
}

func (p *logqlParser) peek() string {
	if len(p.toks) == 0 {
		return ""
	}
	return p.toks[0]
}

func (p *logqlParser) want(toks ...string) error {
	for _, tok := range toks {
		if got := p.next(); got != tok {
			return fmt.Errorf("want %q, got %q", tok, got)
		}
	}
	return nil
}

// union is difference ("or" difference)*: or binds loosest, as in LogQL.
// It keeps the left series and adds each right one whose labels it lacks.
func (p *logqlParser) union() (logqlExpr, error) {
	left, err := p.difference()
	for err == nil && p.peek() == "or" {
		p.next()
		var right logqlExpr
		if right, err = p.difference(); err == nil {
			l := left
			left = func(log []logEntry, at time.Duration) vector {
				out := l(log, at)
				for k, s := range right(log, at) {
					if _, ok := out[k]; !ok {
						out[k] = s
					}
				}
				return out
			}
		}
	}
	return left, err
}

// difference is cmp ("unless" cmp)*, keeping each left series whose labels
// no right series carries.
func (p *logqlParser) difference() (logqlExpr, error) {
	left, err := p.cmp()
	for err == nil && p.peek() == "unless" {
		p.next()
		var right logqlExpr
		if right, err = p.cmp(); err == nil {
			l := left
			left = func(log []logEntry, at time.Duration) vector {
				out, drop := l(log, at), right(log, at)
				for k := range drop {
					delete(out, k)
				}
				return out
			}
		}
	}
	return left, err
}

// cmp is a term with an optional filtering comparison against an integer.
func (p *logqlParser) cmp() (logqlExpr, error) {
	e, err := p.term()
	if err != nil || !slices.Contains(logqlComparisons, p.peek()) {
		return e, err
	}
	op := p.next()
	n, err := strconv.ParseFloat(p.next(), 64)
	if err != nil {
		return nil, err
	}
	return func(log []logEntry, at time.Duration) vector {
		out := e(log, at)
		for k, s := range out {
			if !(op == ">" && s.v > n || op == "==" && s.v == n || op == "<=" && s.v <= n) {
				delete(out, k)
			}
		}
		return out
	}, nil
}

// term is sum by (labels) (union), or a range aggregation optionally
// followed by by (labels), which groups its lines before it aggregates.
func (p *logqlParser) term() (logqlExpr, error) {
	fn := p.next()
	if fn == "sum" {
		if err := p.want("by"); err != nil {
			return nil, err
		}
		by, err := p.labelList()
		if err != nil {
			return nil, err
		}
		inner := len(p.shapes)
		if err := p.want("("); err != nil {
			return nil, err
		}
		e, err := p.union()
		if err == nil {
			err = p.want(")")
		}
		for _, sh := range p.shapes[inner:] {
			if sh.by == nil {
				sh.by = by
			}
		}
		return func(log []logEntry, at time.Duration) vector { return sumBy(e(log, at), by) }, err
	}
	if err := p.want("("); err != nil {
		return nil, err
	}
	sh := &rangeShape{fn: fn}
	p.shapes = append(p.shapes, sh)
	agg, err := p.rangeAgg(sh)
	if err == nil {
		err = p.want(")")
	}
	var by []string
	if err == nil && p.peek() == "by" {
		p.next()
		by, err = p.labelList()
		sh.by = by
	}
	return func(log []logEntry, at time.Duration) vector { return agg(log, at, by) }, err
}

// labelList reads "(" name ("," name)* ")".
func (p *logqlParser) labelList() ([]string, error) {
	if err := p.want("("); err != nil {
		return nil, err
	}
	var names []string
	for {
		names = append(names, p.next())
		if p.peek() != "," {
			break
		}
		p.next()
	}
	return names, p.want(")")
}

func sumBy(in vector, by []string) vector {
	out := vector{}
	for _, s := range in {
		labels := pick(s.labels, by)
		k := labelKey(labels)
		out[k] = series{labels, out[k].v + s.v}
	}
	return out
}

func pick(labels map[string]string, names []string) map[string]string {
	out := map[string]string{}
	for _, n := range names {
		if v := labels[n]; v != "" {
			out[n] = v
		}
	}
	return out
}

// parseSelector reads a stream selector of equality matchers.
func parseSelector(sel string) (map[string]string, error) {
	inner, open := strings.CutPrefix(sel, "{")
	inner, closed := strings.CutSuffix(inner, "}")
	if !open || !closed || strings.TrimSpace(inner) == "" {
		return nil, fmt.Errorf("selector %s", sel)
	}
	out := map[string]string{}
	for m := range strings.SplitSeq(inner, ",") {
		kv := matcherRe.FindStringSubmatch(m)
		if kv == nil {
			return nil, fmt.Errorf("matcher %q of %s is not an equality matcher", m, sel)
		}
		out[kv[1]] = kv[2]
	}
	return out, nil
}

// rangeLine is one line's pipeline: its line filters, whether logfmt
// extracts its fields, the label label_format derives from msg and the
// label unwrap reads.
type rangeLine struct {
	filters                    [][]string
	logfmt                     bool
	formatted, finished, value string
}

// rangeAgg reads a selector, its line filters, its parser stages and the
// range, inside fn's parentheses. Its result groups the selected lines by
// the by labels, or by every label when by is nil, before it aggregates.
func (p *logqlParser) rangeAgg(sh *rangeShape) (func(log []logEntry, at time.Duration, by []string) vector, error) {
	sh.selector = p.next()
	matchers, err := parseSelector(sh.selector)
	if err != nil {
		return nil, err
	}
	var rl rangeLine
	for p.peek() == "|=" {
		p.next()
		alts := []string{strings.Trim(p.next(), "`")}
		for p.peek() == "or" {
			p.next()
			alts = append(alts, strings.Trim(p.next(), "`"))
		}
		rl.filters = append(rl.filters, alts)
	}
	for err == nil && p.peek() == "|" {
		p.next()
		err = p.stage(&rl)
	}
	if err != nil {
		return nil, err
	}
	sh.logfmt = rl.logfmt
	w, err := time.ParseDuration(strings.Trim(p.next(), "[]"))
	if err != nil {
		return nil, err
	}
	fn := sh.fn
	switch fn {
	case "count_over_time", "absent_over_time", "last_over_time", "sum_over_time":
	default:
		return nil, fmt.Errorf("range function %s", fn)
	}
	if (rl.value != "") != (fn == "last_over_time" || fn == "sum_over_time") {
		return nil, fmt.Errorf("%s with unwrap %q", fn, rl.value)
	}
	t := p.t
	return func(log []logEntry, at time.Duration, by []string) vector {
		type agg struct {
			labels         map[string]string
			n, sum, latest float64
		}
		groups := map[string]*agg{}
		for _, e := range log {
			if e.at <= at-w || e.at > at || !selects(e.stream, matchers) || !matchesStages(e.text, rl.filters) {
				continue
			}
			labels, v, err := rl.labels(e)
			if err != nil {
				t.Errorf("%q: %v", e.text, err)
				continue
			}
			if by != nil {
				labels = pick(labels, by)
			}
			k := labelKey(labels)
			g := groups[k]
			if g == nil {
				g = &agg{labels: labels}
				groups[k] = g
			}
			g.n, g.sum, g.latest = g.n+1, g.sum+v, v
		}
		out := vector{}
		if fn == "absent_over_time" {
			if len(groups) == 0 {
				out[labelKey(matchers)] = series{maps.Clone(matchers), 1}
			}
			return out
		}
		for k, g := range groups {
			v := g.latest
			switch fn {
			case "count_over_time":
				v = g.n
			case "sum_over_time":
				v = g.sum
			}
			out[k] = series{g.labels, v}
		}
		return out
	}, nil
}

// stage reads one parser stage after its "|": a bare logfmt, label_format
// with the booting template, or unwrap.
func (p *logqlParser) stage(rl *rangeLine) error {
	switch kind := p.next(); kind {
	case "logfmt":
		// Loki v3.7.9 answers no series for a range grouped by a label that
		// logfmt's parameters extracted (grafana/loki#11334).
		if tok := p.peek(); tok != "|" && !strings.HasPrefix(tok, "[") {
			return fmt.Errorf("logfmt %s: only the bare logfmt groups correctly in Loki", tok)
		}
		rl.logfmt = true
		return nil
	case "label_format":
		rl.formatted = p.next()
		if err := p.want("="); err != nil {
			return err
		}
		tmpl := p.next()
		m := stateTemplateRe.FindStringSubmatch(tmpl)
		if m == nil || !rl.logfmt {
			return fmt.Errorf("label_format %s=%s is not the booting template over an extracted msg", rl.formatted, tmpl)
		}
		rl.finished = m[1]
		return nil
	case "unwrap":
		rl.value = p.next()
		return nil
	default:
		return fmt.Errorf("stage %q", kind)
	}
}

// labels answers e's series labels after the pipeline, and the value unwrap
// reads from them, which leaves the series' labels.
func (rl *rangeLine) labels(e logEntry) (map[string]string, float64, error) {
	labels := maps.Clone(e.stream)
	for k, v := range e.fields {
		if rl.logfmt && v != "" {
			labels[k] = v
		}
	}
	if rl.formatted != "" {
		labels[rl.formatted] = "1"
		if labels["msg"] == rl.finished {
			labels[rl.formatted] = "0"
		}
	}
	if rl.value == "" {
		return labels, 0, nil
	}
	v, err := strconv.ParseFloat(labels[rl.value], 64)
	delete(labels, rl.value)
	return labels, v, err
}

func selects(stream, matchers map[string]string) bool {
	for k, v := range matchers {
		if stream[k] != v {
			return false
		}
	}
	return true
}

func matchesStages(text string, stages [][]string) bool {
	for _, alts := range stages {
		hit := false
		for _, a := range alts {
			hit = hit || strings.Contains(text, a)
		}
		if !hit {
			return false
		}
	}
	return true
}

// firing answers, for each minute from 0 to end, the labelKeys of the series
// a rule of expression e and for hold fires for over log.
func firing(e logqlExpr, hold time.Duration, log []logEntry, end time.Duration) []map[string]bool {
	since := map[string]time.Duration{}
	var out []map[string]bool
	for at := time.Duration(0); at <= end; at += time.Minute {
		vec := e(log, at)
		for k := range since {
			if _, ok := vec[k]; !ok {
				delete(since, k)
			}
		}
		fired := map[string]bool{}
		for k := range vec {
			if _, ok := since[k]; !ok {
				since[k] = at
			}
			if at-since[k] >= hold {
				fired[k] = true
			}
		}
		out = append(out, fired)
	}
	return out
}

// scenario writes a timeline of manade processes from the lines production
// writes: lines[0] the start record, [1] a boot record, [2] the end line and
// [3] a pass record. Each process writes to a stream of its own, the
// selector's labels plus an instance label, as a collector that labels each
// container apart would.
type scenario struct {
	lines    []logLine
	matchers map[string]string
	// pool is the POOL_NAME of the processes process writes next.
	pool  string
	procs int
	log   []logEntry
}

// process is one manade process starting at start whose boot lasts boot.
// A boot that finishes is followed by a pass record every 2.5 minutes up to
// until; one that does not ends the process there, as a failed boot exits.
func (s *scenario) process(start, boot time.Duration, booted bool, until time.Duration) {
	s.procs++
	stream := maps.Clone(s.matchers)
	stream["instance"] = fmt.Sprintf("%s-%d", s.pool, s.procs)
	s.write(0, stream, start, start)
	for at := start + bootEvery; at < start+boot; at += bootEvery {
		s.write(1, stream, start, at)
	}
	if !booted {
		return
	}
	s.write(2, stream, start, start+boot)
	for at := start + boot + bootEvery; at <= until; at += bootEvery {
		s.write(3, stream, start, at)
	}
}

var (
	bootSecondsRe = regexp.MustCompile(`\bboot_seconds=[0-9]+`)
	poolFieldRe   = regexp.MustCompile(`\bpool=\S+`)
)

// write adds lines[i] to stream at at, with the scenario's pool and its
// boot_seconds set to the time since its process started at start, as
// Boot and Booted would write it then.
func (s *scenario) write(i int, stream map[string]string, start, at time.Duration) {
	text := bootSecondsRe.ReplaceAllString(s.lines[i].text, fmt.Sprintf("boot_seconds=%d", int64((at-start)/time.Second)))
	text = poolFieldRe.ReplaceAllString(text, "pool="+s.pool)
	fields := map[string]string{}
	for _, kv := range logfmtPairRe.FindAllStringSubmatch(text, -1) {
		fields[kv[1]] = kv[2]
		if u, err := strconv.Unquote(kv[2]); err == nil {
			fields[kv[1]] = u
		}
	}
	s.log = append(s.log, logEntry{at, stream, text, fields})
}

// loop is a process every period from start to end, each failing its boot
// after boot, or, with booted, finishing it and then exiting before its
// first pass record.
func (s *scenario) loop(start, period, boot, end time.Duration, booted bool) {
	for at := start; at < end; at += period {
		s.process(at, boot, booted, at+boot)
	}
}

// window is when a rule must first fire for a series.
type window struct{ from, to time.Duration }

// fires maps a pool to the window its series must first fire in; "" names
// the series of the selector's matchers. From the first firing to the end,
// the rule must fire for some series at every evaluation.
type fires map[string]window

// selectorVariants are the selectors the rules must work under: the
// shipped one, the label renamed, and one matcher added.
var selectorVariants = []struct{ name, selector string }{
	{"shippedSelector", `{container="manade"}`},
	{"renamedSelector", `{service_name="manade"}`},
	{"selectorWithAHost", `{container="manade", host="h1"}`},
}

// logqlRule is one parsed rule of alerts/logql.yaml.
type logqlRule struct {
	expr logqlExpr
	hold time.Duration
}

// readLogQLRules parses alerts/logql.yaml with selector in place of the
// shipped {container="manade"}, failing unless every range reads it.
func readLogQLRules(t *testing.T, selector string) map[string]logqlRule {
	t.Helper()
	out := map[string]logqlRule{}
	for alert, r := range readRules(t, "logql.yaml") {
		e, shapes, err := parseLogQL(t, strings.ReplaceAll(r.expr, `{container="manade"}`, selector))
		if err != nil {
			t.Fatalf("%s: %v in %q", alert, err, r.expr)
		}
		for _, sh := range shapes {
			if sh.selector != selector {
				t.Fatalf("%s reads %s, want the shipped {container=\"manade\"}", alert, sh.selector)
			}
		}
		hold, err := time.ParseDuration(r.hold)
		if err != nil {
			t.Fatalf("%s: for %q: %v", alert, r.hold, err)
		}
		out[alert] = logqlRule{e, hold}
	}
	return out
}

// TestLogQLRulesGroupByPool: each rule's ranges, but absent_over_time's,
// parse the line's fields and group by pool alone, so whatever selector
// finds the lines, each alert names its pool and no pool's lines answer for
// another's.
func TestLogQLRulesGroupByPool(t *testing.T) {
	for alert, r := range readRules(t, "logql.yaml") {
		_, shapes, err := parseLogQL(t, r.expr)
		if err != nil {
			t.Fatalf("%s: %v", alert, err)
		}
		for _, sh := range shapes {
			if sh.fn != "absent_over_time" && (!sh.logfmt || !slices.Equal(sh.by, []string{"pool"})) {
				t.Errorf("%s: %s parses logfmt %v and groups by %v, want logfmt and by (pool)", alert, sh.fn, sh.logfmt, sh.by)
			}
		}
	}
}

func TestLogQLBootRules(t *testing.T) {
	lines := recordLines(t)
	const m, s = time.Minute, time.Second
	const end = 90 * m
	const h = time.Hour
	scenarios := []struct {
		name          string
		arrange       func(sc *scenario)
		down, stalled fires
		span          time.Duration // evaluated from 0 to span, end when 0
	}{
		{name: "fastFirstStart", arrange: func(sc *scenario) { sc.process(0, 20*s, true, end) }},
		{name: "slowFirstStart", arrange: func(sc *scenario) { sc.process(0, 15*m+10*s, true, end) }},
		{name: "firstStartJustInsideTheBound", arrange: func(sc *scenario) { sc.process(0, 28*m+50*s, true, end) }},
		{
			name: "failedFirstStartThatDockerRetries",
			arrange: func(sc *scenario) {
				sc.process(0, 5*s, false, 0)
				sc.process(6*s, 2*m+50*s, true, end)
			},
		},
		{
			name: "redeployBeforeTheFirstBootRecord",
			arrange: func(sc *scenario) {
				sc.process(0, 90*s, false, 0)
				sc.process(90*s, 2*m+50*s, true, end)
			},
		},
		{
			name: "restartOfAServingPool",
			arrange: func(sc *scenario) {
				sc.process(0, m, true, 30*m)
				sc.process(30*m+30*s, 70*s, true, end)
			},
		},
		{
			name:    "startThatNeverEnds",
			arrange: func(sc *scenario) { sc.process(0, end, false, 0) },
			stalled: fires{"manade": {30 * m, 31 * m}},
		},
		// The third start is at 140s.
		{
			name:    "loopOfStartsFailingAtOnce",
			arrange: func(sc *scenario) { sc.loop(0, 70*s, 5*s, end, false) },
			down:    fires{"manade": {140 * s, 3 * m}},
			stalled: fires{"manade": {30 * m, 31 * m}},
		},
		{
			name:    "loopOfStartsFailingAfterABootRecord",
			arrange: func(sc *scenario) { sc.loop(0, 4*m, 3*m, end, false) },
			stalled: fires{"manade": {30 * m, 31 * m}},
		},
		{
			name:    "loopOfStartsThatFinishThenExit",
			arrange: func(sc *scenario) { sc.loop(0, 90*s, 20*s, end, true) },
			down:    fires{"manade": {180 * s, 4 * m}},
		},
		// The last pass record is at 38m30s.
		{
			name:    "servingPoolStops",
			arrange: func(sc *scenario) { sc.process(0, m, true, 40*m) },
			down:    fires{"manade": {48*m + 30*s, 49*m + 30*s}},
		},
		// The last pass record is at 8m30s. A day later the pool is out of
		// every range, and the selector's series takes over.
		{
			name:    "poolSilentForADay",
			arrange: func(sc *scenario) { sc.process(0, m, true, 10*m) },
			down:    fires{"manade": {18*m + 30*s, 19*m + 30*s}, "": {24*h + 8*m + 30*s, 24*h + 9*m + 30*s}},
			span:    25 * h,
		},
		// Pool b's lines keep the selector's series away, so a stays paged
		// by name.
		{
			name: "stoppedPoolStaysPagedWhileAnotherServes",
			arrange: func(sc *scenario) {
				sc.pool = "a"
				sc.process(0, m, true, 40*m)
				sc.pool = "b"
				sc.process(0, m, true, 3*h)
			},
			down: fires{"a": {48*m + 30*s, 49*m + 30*s}},
			span: 3 * h,
		},
		// The restart's start record holds the deadman until it is 10
		// minutes old.
		{
			name: "servingPoolStopsDuringARestart",
			arrange: func(sc *scenario) {
				sc.process(0, m, true, 40*m)
				sc.process(40*m, 5*s, false, 0)
			},
			down: fires{"manade": {49*m + 30*s, 50*m + 30*s}},
		},
		{
			name: "servingPoolFallsIntoALoopOfFailingStarts",
			arrange: func(sc *scenario) {
				sc.process(0, m, true, 40*m)
				sc.loop(41*m, 70*s, 5*s, end, false)
			},
			down:    fires{"manade": {48*m + 30*s, 49*m + 30*s}},
			stalled: fires{"manade": {71 * m, 72 * m}},
		},
		// Pool a's last pass record is at 38m30s; pool b's start record at
		// 47m is the only line under 10 minutes old when a's page is due.
		{
			name: "poolStopsWhileAnotherColdStarts",
			arrange: func(sc *scenario) {
				sc.pool = "a"
				sc.process(0, m, true, 40*m)
				sc.pool = "b"
				sc.process(47*m, 15*m, true, end)
			},
			down: fires{"a": {48*m + 30*s, 49*m + 30*s}},
		},
	}
	for _, v := range selectorVariants {
		t.Run(v.name, func(t *testing.T) {
			matchers, err := parseSelector(v.selector)
			if err != nil {
				t.Fatal(err)
			}
			rules := readLogQLRules(t, v.selector)
			for _, tc := range scenarios {
				t.Run(tc.name, func(t *testing.T) {
					span := cmp.Or(tc.span, end)
					sc := &scenario{lines: lines, matchers: matchers, pool: "manade"}
					tc.arrange(sc)
					slices.SortStableFunc(sc.log, func(a, b logEntry) int { return int(a.at - b.at) })
					for alert, want := range map[string]fires{"ManadeDown": tc.down, "ManadeBootStalled": tc.stalled} {
						r := rules[alert]
						checkFiring(t, alert, firing(r.expr, r.hold, sc.log, span), want, matchers)
					}
				})
			}
		})
	}
}

// checkFiring compares a rule's firing, per minute, with want.
func checkFiring(t *testing.T, alert string, got []map[string]bool, want fires, matchers map[string]string) {
	t.Helper()
	keys := map[string]window{}
	for pool, w := range want {
		k := labelKey(map[string]string{"pool": pool})
		if pool == "" {
			k = labelKey(matchers)
		}
		keys[k] = w
	}
	first := map[string]time.Duration{}
	for i, fired := range got {
		for k := range fired {
			if _, seen := first[k]; !seen {
				first[k] = time.Duration(i) * time.Minute
			}
		}
	}
	for k, at := range first {
		if _, ok := keys[k]; !ok {
			t.Errorf("%s fired for %s at %v, want never", alert, k, at)
		}
	}
	held := time.Duration(-1)
	for k, w := range keys {
		at, ok := first[k]
		switch {
		case !ok:
			t.Errorf("%s never fired for %s, want it first in [%v, %v]", alert, k, w.from, w.to)
		case at < w.from || at > w.to:
			t.Errorf("%s first fired for %s at %v, want in [%v, %v]", alert, k, at, w.from, w.to)
		}
		if ok && (held < 0 || at < held) {
			held = at
		}
	}
	for i := int(held / time.Minute); held >= 0 && i < len(got); i++ {
		if len(got[i]) == 0 {
			t.Errorf("%s stopped firing at %v, want it held to the end", alert, time.Duration(i)*time.Minute)
			return
		}
	}
}
