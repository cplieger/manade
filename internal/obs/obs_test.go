package obs

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/measure/v2"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
)

var at = time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)

// series is every series this build declares, by area,
// with each family's exposition type.
var series = map[string]measure.Type{
	// Build
	"manade_build_info":      measure.TypeGauge,
	"manade_host_build_info": measure.TypeGauge,
	// Boot and generations
	"manade_booting": measure.TypeGauge,
	"manade_generation_applied_timestamp_seconds": measure.TypeGauge,
	// GitHub
	"manade_scope_serving":                             measure.TypeGauge,
	"manade_poll_last_success_timestamp_seconds":       measure.TypeGauge,
	"manade_queue_read_last_success_timestamp_seconds": measure.TypeGauge,
	"manade_queue_read_deadline_timestamp_seconds":     measure.TypeGauge,
	"manade_rest_reads_total":                          measure.TypeCounter,
	"manade_github_errors_total":                       measure.TypeCounter,
	"manade_scale_sets_orphaned":                       measure.TypeGauge,
	// Queue
	"manade_jobs_queued":             measure.TypeGauge,
	"manade_jobs_waiting":            measure.TypeGauge,
	"manade_jobs_assigned_unstarted": measure.TypeGauge,
	"manade_queue_head_wait_seconds": measure.TypeGauge,
	"manade_job_queue_seconds":       measure.TypeHistogram,
	"manade_job_duration_seconds":    measure.TypeHistogram,
	// Runners
	"manade_runners":             measure.TypeGauge,
	"manade_runner_starts_total": measure.TypeCounter,
	"manade_runner_exits_total":  measure.TypeCounter,
	// Capacity
	"manade_capacity_cpus":         measure.TypeGauge,
	"manade_capacity_memory_bytes": measure.TypeGauge,
	"manade_capacity_disk_bytes":   measure.TypeGauge,
	"manade_reserved_cpus":         measure.TypeGauge,
	"manade_reserved_memory_bytes": measure.TypeGauge,
	"manade_reserved_disk_bytes":   measure.TypeGauge,
	"manade_host_accepting":        measure.TypeGauge,
	// Hosts
	"manade_host_connected":                              measure.TypeGauge,
	"manade_host_serving":                                measure.TypeGauge,
	"manade_host_egress_open":                            measure.TypeGauge,
	"manade_host_egress_proved_timestamp_seconds":        measure.TypeGauge,
	"manade_host_probe_deadline_timestamp_seconds":       measure.TypeGauge,
	"manade_host_probe_failed":                           measure.TypeGauge,
	"manade_host_probe_result":                           measure.TypeGauge,
	"manade_host_profile_ready":                          measure.TypeGauge,
	"manade_host_image_checked_timestamp_seconds":        measure.TypeGauge,
	"manade_host_image_check_deadline_timestamp_seconds": measure.TypeGauge,
	"manade_host_data_free_bytes":                        measure.TypeGauge,
	"manade_host_own_memory_bytes":                       measure.TypeGauge,
	"manade_host_admissible_memory_bytes":                measure.TypeGauge,
	"manade_host_ipv6_prefix_learned":                    measure.TypeGauge,
	"manade_host_github_domains_timestamp_seconds":       measure.TypeGauge,
	"manade_host_github_addresses":                       measure.TypeGauge,
	"manade_host_section_pending":                        measure.TypeGauge,
	"manade_host_section_refused":                        measure.TypeGauge,
	"manade_host_section_current":                        measure.TypeGauge,
	"manade_host_waiting_for_section":                    measure.TypeGauge,
	// Host counts
	"manade_host_rules_restored_total":           measure.TypeCounter,
	"manade_host_oom_kills_total":                measure.TypeCounter,
	"manade_host_github_addresses_refused_total": measure.TypeCounter,
	"manade_host_runner_start_seconds":           measure.TypeHistogram,
	"manade_host_link_errors_total":              measure.TypeCounter,
	// Reconcile
	"manade_reconcile_last_success_timestamp_seconds": measure.TypeGauge,
	"manade_reconcile_deadline_timestamp_seconds":     measure.TypeGauge,
	// Control plane
	"manade_store_size_bytes":                        measure.TypeGauge,
	"manade_store_free_bytes":                        measure.TypeGauge,
	"manade_store_write_failures_total":              measure.TypeCounter,
	"manade_housekeeping_deadline_timestamp_seconds": measure.TypeGauge,
}

// settings is two hosts, two profiles and two scopes: small runs on both
// hosts and serves both scopes, large runs on default and serves
// acme/app only.
func settings() *config.Config {
	return &config.Config{
		Hosts: []config.Host{{Name: "default", Accepting: true}, {Name: "nas", Accepting: true}},
		Profiles: []config.Profile{
			{Name: "small", Hosts: []string{"default", "nas"}},
			{Name: "large", Hosts: []string{"default"}, Scopes: []string{"acme/app"}},
		},
		Scopes: []config.Scope{
			{Key: "acme/app", Owner: "acme", Repo: "app", Kind: config.ScopeRepo},
			{Key: "acme", Owner: "acme", Kind: config.ScopeOrg},
		},
	}
}

// fakeStore is the store's usage as a test sets it.
type fakeStore struct {
	sizeErr, freeErr error
	size             int64
	free, failures   uint64
}

func (f *fakeStore) Size() (int64, error)        { return f.size, f.sizeErr }
func (f *fakeStore) VolumeFree() (uint64, error) { return f.free, f.freeErr }
func (f *fakeStore) WriteFailures() uint64       { return f.failures }

// newMetrics answers Metrics over a snapshot the test sets, an empty
// store and no housekeeper run.
func newMetrics(t *testing.T, snap *pool.Snapshot) *Metrics {
	t.Helper()
	return newMetricsOver(t, &Sources{
		Pool: func() pool.Snapshot { return *snap }, Store: &fakeStore{}, Housekeeping: func() time.Time { return time.Time{} },
	})
}

func newMetricsOver(t *testing.T, src *Sources) *Metrics {
	t.Helper()
	m, err := New("v1.0.0-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Ready(src)
	return m
}

// gather reads m, failing on any sample the read series dropped.
func gather(t *testing.T, m *Metrics) []measure.Family {
	t.Helper()
	fams, err := m.reg.Gather(t.Context())
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	return fams
}

func family(t *testing.T, fams []measure.Family, name string) *measure.Family {
	t.Helper()
	f, ok := measure.Find(fams, name)
	if !ok {
		t.Fatalf("no family %s", name)
	}
	return &f
}

// value answers the sample of name with labels, and false when absent.
func value(t *testing.T, fams []measure.Family, name string, labels ...string) (float64, bool) {
	t.Helper()
	f := family(t, fams, name)
	s, ok := f.Sample(labels...)
	return s.Value, ok
}

func wantValue(t *testing.T, fams []measure.Family, want float64, name string, labels ...string) {
	t.Helper()
	got, ok := value(t, fams, name, labels...)
	switch {
	case !ok:
		t.Errorf("%s%v absent, want %v", name, labels, want)
	case got != want:
		t.Errorf("%s%v = %v, want %v", name, labels, got, want)
	}
}

func wantAbsent(t *testing.T, fams []measure.Family, name string, labels ...string) {
	t.Helper()
	if got, ok := value(t, fams, name, labels...); ok {
		t.Errorf("%s%v = %v, want absent", name, labels, got)
	}
}

func unix(t time.Time) float64 { return float64(t.UnixMilli()) / 1000 }

func TestEveryDeclarationRegisters(t *testing.T) {
	snap := fixture()
	m := newMetrics(t, &snap)
	got := map[string]measure.Type{}
	for _, f := range gather(t, m) {
		if strings.HasPrefix(f.Name, "manade_") {
			got[f.Name] = f.Type
		}
	}
	for name, typ := range series {
		if g, ok := got[name]; !ok || g != typ {
			t.Errorf("%s: declared %v (%v), want a %s", name, g, ok, typ)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if _, ok := series[name]; !ok {
			t.Errorf("%s is declared and is not in the series table", name)
		}
	}
}

func TestBuildInfoNamesTheVersion(t *testing.T) {
	snap := pool.Snapshot{}
	fams := gather(t, newMetrics(t, &snap))
	wantValue(t, fams, 1, "manade_build_info", "v1.0.0-test")
	wantValue(t, fams, 1, "manade_booting")
	wantValue(t, fams, 0, "manade_generation_applied_timestamp_seconds")
}

// Before its sources are ready (the store not open, no pool yet) the page
// says the control plane is booting and reads nothing else.
func TestBeforeReadyTheControlPlaneReadsBooting(t *testing.T) {
	m, err := New("v1.0.0-test", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	fams := gather(t, m)
	wantValue(t, fams, 1, "manade_build_info", "v1.0.0-test")
	wantValue(t, fams, 1, "manade_booting")
	wantValue(t, fams, 0, "manade_generation_applied_timestamp_seconds")
	for _, name := range []string{"manade_store_size_bytes", "manade_store_write_failures_total", "manade_housekeeping_deadline_timestamp_seconds", "manade_queue_head_wait_seconds"} {
		wantAbsent(t, fams, name)
	}
	m.Ready(&Sources{
		Pool:  func() pool.Snapshot { return pool.Snapshot{Booted: true} },
		Store: &fakeStore{size: 1 << 20}, Housekeeping: func() time.Time { return time.Time{} },
	})
	fams = gather(t, m)
	wantValue(t, fams, 0, "manade_booting")
	wantValue(t, fams, 1<<20, "manade_store_size_bytes")
}

// fixture is a booted pool over settings(): default serving with a status,
// a passing probe and a runner of each state; nas disconnected, its last
// status seen; acme/app served and read, acme not served.
func fixture() pool.Snapshot {
	small := github.ScaleSet{Scope: "acme/app", Profile: "small", ID: 1}
	kills := uint64(2)
	st := &hostapi.Status{
		Version: "v1.0.0-host",
		Section: hostapi.SectionStatus{State: hostapi.SectionRunning, Hash: "h1"},
		Profiles: []hostapi.ProfileStatus{
			{Name: "small", Ready: true, CheckedAt: at.Add(-time.Hour)},
			{Name: "large", Ready: false},
			{Name: "gone", Ready: true},
		},
		Memory: hostapi.MemoryStatus{Own: 300 << 20, Admissible: 30 << 30},
		Data:   hostapi.DataStatus{Free: 100 << 30},
		IPv6:   &hostapi.IPv6Status{},
		GitHub: &hostapi.GitHubStatus{DomainsAt: at.Add(-2 * time.Hour), Addresses: 40, Refused: 3},
		Rules:  hostapi.RulesStatus{Restored: 5},
		OOM:    hostapi.OOMStatus{Kills: &kills},
		StartLatency: []hostapi.CallLatency{
			{Call: "create", Buckets: []uint64{0, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2}, Count: 2, Sum: 0.3},
		},
	}
	nasSeen := &hostapi.Status{Version: "v1.0.0-old", Rules: hostapi.RulesStatus{Restored: 1}, OOM: hostapi.OOMStatus{Kills: &kills}}
	return pool.Snapshot{
		At: at, HeadSince: at.Add(-90 * time.Second), Booted: true, Profiles: []string{"small", "large"},
		Scopes: []pool.ScopeStatus{
			{Scope: "acme/app", Kind: config.ScopeRepo, Serving: true, ReadAt: at.Add(-5 * time.Second), ReadDue: at.Add(5 * time.Second)},
			{Scope: "acme", Kind: config.ScopeOrg, Detail: pool.DetailVisibilityUnproved},
		},
		Sets: []pool.SetCounts{
			{ScaleSet: small, AssignedUnstarted: 1, Queued: 2, Polled: at.Add(-3 * time.Second)},
			{Scope: "acme/app", Profile: "large", Queued: 1},
		},
		Orphaned: []pool.Orphan{{Scope: "acme/old", Profile: "tiny", ID: 9}},
		Waiting: []pool.Waiting{
			{ScaleSet: small, Reason: pool.JobStarting, Assigned: true},
			{ScaleSet: small, Reason: pool.JobWaitingForSlot},
			{ScaleSet: small, Reason: pool.JobWaitingForSlot},
		},
		Hosts: []pool.HostView{
			{
				Host: "default", Connected: true, Serving: true, Accepting: true, Current: true,
				Status: st, Seen: st, Profiles: []string{"small", "large"}, ImageRefresh: 24 * time.Hour,
				Probe:    &hostapi.ProbeReport{Proven: true, Checks: []hostapi.ProbeCheck{{Name: "dns", Pass: true}, {Name: "blocked", Pass: false}}},
				ProvedAt: at.Add(-time.Minute), Reconciled: at.Add(-10 * time.Second), ReconcileDue: at.Add(170 * time.Second),
				Capacity: &pool.Resources{MilliCPUs: 8000, Memory: 32 << 30, Disk: 100 << 30},
				Reserved: pool.Resources{MilliCPUs: 2500, Memory: 6 << 30, Disk: 10 << 30},
				Runners: []pool.RunnerCounts{
					{ScaleSet: small, Starting: 1, Idle: 2, Busy: 3},
					{Scope: "acme/app", Profile: "large", ID: 2, Busy: 1},
				},
			},
			{
				Host: "nas", Accepting: false, EgressOpen: true, Seen: nasSeen, Profiles: []string{"small"},
				ProbeFailed: true, ProbeUntil: at.Add(time.Minute),
				Probe:    &hostapi.ProbeReport{Checks: []hostapi.ProbeCheck{{Name: "dns"}}},
				Capacity: &pool.Resources{MilliCPUs: 2000, Memory: 4 << 30},
			},
		},
	}
}

func TestReadSeriesComeFromTheSnapshot(t *testing.T) {
	snap := fixture()
	fams := gather(t, newMetrics(t, &snap))
	t.Run("boot and queue", func(t *testing.T) {
		wantValue(t, fams, 0, "manade_booting")
		wantValue(t, fams, 90, "manade_queue_head_wait_seconds")
		wantValue(t, fams, 2, "manade_jobs_queued", "acme/app", "small")
		wantValue(t, fams, 1, "manade_jobs_queued", "acme/app", "large")
		wantValue(t, fams, 1, "manade_jobs_assigned_unstarted", "acme/app", "small")
		wantValue(t, fams, 1, "manade_jobs_waiting", "small", string(pool.JobStarting))
		wantValue(t, fams, 2, "manade_jobs_waiting", "small", string(pool.JobWaitingForSlot))
		for _, r := range pool.JobReasons {
			if _, ok := value(t, fams, "manade_jobs_waiting", "large", string(r)); !ok {
				t.Errorf("manade_jobs_waiting{large,%s} absent, want every profile's reasons on the page", r)
			}
		}
	})
	t.Run("scopes", func(t *testing.T) {
		wantValue(t, fams, 1, "manade_scope_serving", "acme/app")
		wantValue(t, fams, 0, "manade_scope_serving", "acme")
		wantValue(t, fams, unix(at.Add(-3*time.Second)), "manade_poll_last_success_timestamp_seconds", "acme/app", "small")
		wantAbsent(t, fams, "manade_poll_last_success_timestamp_seconds", "acme/app", "large")
		wantValue(t, fams, unix(at.Add(-5*time.Second)), "manade_queue_read_last_success_timestamp_seconds", "acme/app")
		wantValue(t, fams, unix(at.Add(5*time.Second)), "manade_queue_read_deadline_timestamp_seconds", "acme/app")
		wantAbsent(t, fams, "manade_queue_read_last_success_timestamp_seconds", "acme")
		wantValue(t, fams, 0, "manade_scale_sets_orphaned", "acme/app")
		wantValue(t, fams, 1, "manade_scale_sets_orphaned", "acme/old")
	})
	t.Run("a serving host", func(t *testing.T) {
		for state, n := range map[string]float64{"starting": 1, "idle": 2, "busy": 4} {
			wantValue(t, fams, n, "manade_runners", "default", state)
		}
		wantValue(t, fams, 8, "manade_capacity_cpus", "default")
		wantValue(t, fams, 32<<30, "manade_capacity_memory_bytes", "default")
		wantValue(t, fams, 100<<30, "manade_capacity_disk_bytes", "default")
		wantValue(t, fams, 2.5, "manade_reserved_cpus", "default")
		wantValue(t, fams, 6<<30, "manade_reserved_memory_bytes", "default")
		wantValue(t, fams, 10<<30, "manade_reserved_disk_bytes", "default")
		wantValue(t, fams, 1, "manade_host_build_info", "default", "v1.0.0-host")
		wantValue(t, fams, 1, "manade_host_connected", "default")
		wantValue(t, fams, 1, "manade_host_serving", "default")
		wantValue(t, fams, 1, "manade_host_accepting", "default")
		wantValue(t, fams, 0, "manade_host_egress_open", "default")
		wantValue(t, fams, unix(at.Add(-time.Minute)), "manade_host_egress_proved_timestamp_seconds", "default")
		wantValue(t, fams, 0, "manade_host_probe_deadline_timestamp_seconds", "default")
		wantValue(t, fams, 0, "manade_host_probe_failed", "default")
		wantValue(t, fams, 1, "manade_host_probe_result", "default", "dns")
		wantValue(t, fams, 0, "manade_host_probe_result", "default", "blocked")
		wantValue(t, fams, 1, "manade_host_profile_ready", "default", "small")
		wantValue(t, fams, 0, "manade_host_profile_ready", "default", "large")
		wantAbsent(t, fams, "manade_host_profile_ready", "default", "gone")
		wantValue(t, fams, unix(at.Add(-time.Hour)), "manade_host_image_checked_timestamp_seconds", "default", "small")
		wantValue(t, fams, unix(at.Add(23*time.Hour)), "manade_host_image_check_deadline_timestamp_seconds", "default", "small")
		wantValue(t, fams, 0, "manade_host_image_checked_timestamp_seconds", "default", "large")
		wantValue(t, fams, 0, "manade_host_image_check_deadline_timestamp_seconds", "default", "large")
		wantValue(t, fams, 100<<30, "manade_host_data_free_bytes", "default")
		wantValue(t, fams, 300<<20, "manade_host_own_memory_bytes", "default")
		wantValue(t, fams, 30<<30, "manade_host_admissible_memory_bytes", "default")
		wantValue(t, fams, 0, "manade_host_ipv6_prefix_learned", "default")
		wantValue(t, fams, unix(at.Add(-2*time.Hour)), "manade_host_github_domains_timestamp_seconds", "default")
		wantValue(t, fams, 40, "manade_host_github_addresses", "default", "ipv4")
		wantValue(t, fams, 1, "manade_host_section_current", "default")
		wantValue(t, fams, 0, "manade_host_section_refused", "default")
		wantValue(t, fams, 0, "manade_host_section_pending", "default", "drain")
		wantValue(t, fams, 0, "manade_host_section_pending", "default", "restart")
		wantValue(t, fams, 0, "manade_host_waiting_for_section", "default")
		wantValue(t, fams, 5, "manade_host_rules_restored_total", "default")
		wantValue(t, fams, 2, "manade_host_oom_kills_total", "default")
		wantValue(t, fams, 3, "manade_host_github_addresses_refused_total", "default", "ipv4")
		wantValue(t, fams, unix(at.Add(-10*time.Second)), "manade_reconcile_last_success_timestamp_seconds", "default")
		wantValue(t, fams, unix(at.Add(170*time.Second)), "manade_reconcile_deadline_timestamp_seconds", "default")
		f := family(t, fams, "manade_host_runner_start_seconds")
		s, ok := f.Sample("default", "create")
		if !ok || s.Histogram == nil || s.Histogram.Count != 2 || s.Histogram.Buckets[1].Count != 1 || s.Histogram.Buckets[1].UpperBound != hostapi.LatencyBuckets[1] {
			t.Errorf("start latency %+v, want the host's create histogram on its own bounds", s.Histogram)
		}
	})
}

// A disconnected host's figures leave the page, so absent() sees them go,
// while its pool verdicts stay and its counters keep their last value.
func TestADisconnectedHostsReadSeriesAreAbsent(t *testing.T) {
	snap := fixture()
	fams := gather(t, newMetrics(t, &snap))
	for _, name := range []string{
		"manade_host_build_info", "manade_host_profile_ready", "manade_host_image_checked_timestamp_seconds",
		"manade_host_image_check_deadline_timestamp_seconds", "manade_host_data_free_bytes", "manade_host_own_memory_bytes",
		"manade_host_admissible_memory_bytes", "manade_host_ipv6_prefix_learned", "manade_host_github_domains_timestamp_seconds",
		"manade_host_github_addresses", "manade_host_section_pending", "manade_host_section_refused", "manade_host_section_current",
		"manade_host_waiting_for_section", "manade_host_probe_result",
	} {
		for _, s := range family(t, fams, name).Samples {
			if s.Labels[0].Value == "nas" {
				t.Errorf("%s%v = %v for the disconnected host, want absent", name, s.Labels, s.Value)
			}
		}
	}
	wantValue(t, fams, 0, "manade_host_connected", "nas")
	wantValue(t, fams, 0, "manade_host_serving", "nas")
	wantValue(t, fams, 0, "manade_host_accepting", "nas")
	wantValue(t, fams, 1, "manade_host_egress_open", "nas")
	wantValue(t, fams, 1, "manade_host_probe_failed", "nas")
	wantValue(t, fams, unix(at.Add(time.Minute)), "manade_host_probe_deadline_timestamp_seconds", "nas")
	wantValue(t, fams, 0, "manade_host_egress_proved_timestamp_seconds", "nas")
	wantValue(t, fams, 0, "manade_reconcile_last_success_timestamp_seconds", "nas")
	wantValue(t, fams, 0, "manade_reconcile_deadline_timestamp_seconds", "nas")
	wantValue(t, fams, 1, "manade_host_rules_restored_total", "nas")
	wantValue(t, fams, 2, "manade_host_oom_kills_total", "nas")
	wantAbsent(t, fams, "manade_capacity_disk_bytes", "nas")
	wantAbsent(t, fams, "manade_reserved_disk_bytes", "nas")
	for _, state := range []string{"starting", "idle", "busy"} {
		wantValue(t, fams, 0, "manade_runners", "nas", state)
	}
}

func TestASectionStateIsReadFromTheStatus(t *testing.T) {
	for _, tc := range []struct {
		state                                     hostapi.SectionState
		current, refused, drain, restart, waiting float64
	}{
		{state: hostapi.SectionRunning, current: 1},
		{state: hostapi.SectionWaiting, waiting: 1},
		{state: hostapi.SectionRefused, refused: 1},
		{state: hostapi.SectionEgressPending, drain: 1},
		{state: hostapi.SectionRestartPending, restart: 1},
		{state: hostapi.SectionEgressFailed},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			snap := fixture()
			h := &snap.Hosts[0]
			st := *h.Status
			st.Section.State = tc.state
			h.Status, h.Current = &st, tc.state == hostapi.SectionRunning
			fams := gather(t, newMetrics(t, &snap))
			wantValue(t, fams, tc.current, "manade_host_section_current", "default")
			wantValue(t, fams, tc.refused, "manade_host_section_refused", "default")
			wantValue(t, fams, tc.drain, "manade_host_section_pending", "default", "drain")
			wantValue(t, fams, tc.restart, "manade_host_section_pending", "default", "restart")
			wantValue(t, fams, tc.waiting, "manade_host_waiting_for_section", "default")
		})
	}
}

// Every label set the settings allow is on the page at 0, so an increase()
// rule sees the first event; a generation adds the sets its settings add
// and deletes the ones they drop, and an event outside them is dropped.
func TestRecordedSeriesFollowTheGenerations(t *testing.T) {
	snap := fixture()
	m := newMetrics(t, &snap)
	t.Run("closed sets before any generation", func(t *testing.T) {
		fams := gather(t, m)
		for _, op := range github.Ops {
			wantValue(t, fams, 0, "manade_github_errors_total", string(op))
		}
		for _, r := range github.ReadResults {
			wantValue(t, fams, 0, "manade_rest_reads_total", string(r))
		}
		if f := family(t, fams, "manade_runner_starts_total"); len(f.Samples) != 0 {
			t.Errorf("starts %v before a generation, want none", f.Samples)
		}
	})
	m.Generation(settings(), 1, at)
	fams := gather(t, m)
	t.Run("seeded from the first settings", func(t *testing.T) {
		wantValue(t, fams, unix(at), "manade_generation_applied_timestamp_seconds")
		triples := [][3]string{
			{"default", "acme/app", "small"},
			{"nas", "acme/app", "small"},
			{"default", "acme", "small"},
			{"nas", "acme", "small"},
			{"default", "acme/app", "large"},
		}
		if n := len(family(t, fams, "manade_runner_starts_total").Samples); n != 2*len(triples) {
			t.Errorf("start series %d, want ok and error for each of %d host, scope and profile sets", n, len(triples))
		}
		if n := len(family(t, fams, "manade_runner_exits_total").Samples); n != len(pool.ExitReasons)*len(triples) {
			t.Errorf("exit series %d, want every reason for each of %d sets", n, len(triples))
		}
		for _, tr := range triples {
			wantValue(t, fams, 0, "manade_runner_starts_total", tr[0], tr[1], tr[2], "ok")
			wantValue(t, fams, 0, "manade_runner_starts_total", tr[0], tr[1], tr[2], "error")
			for _, r := range pool.ExitReasons {
				wantValue(t, fams, 0, "manade_runner_exits_total", tr[0], tr[1], tr[2], string(r))
			}
		}
		wantAbsent(t, fams, "manade_runner_starts_total", "nas", "acme/app", "large", "ok")
		wantAbsent(t, fams, "manade_runner_starts_total", "default", "acme", "large", "ok")
		for _, h := range []string{"default", "nas"} {
			wantValue(t, fams, 0, "manade_host_link_errors_total", h)
		}
		for _, p := range []string{"small", "large"} {
			if s, ok := family(t, fams, "manade_job_queue_seconds").Sample(p); !ok || s.Histogram.Count != 0 {
				t.Errorf("job queue %s %+v, want seeded at 0", p, s)
			}
			for _, r := range pool.JobResults {
				if s, ok := family(t, fams, "manade_job_duration_seconds").Sample(p, string(r)); !ok || s.Histogram.Count != 0 {
					t.Errorf("job duration %s %s %+v, want seeded at 0", p, r, s)
				}
			}
		}
	})
	small := github.ScaleSet{Scope: "acme/app", Profile: "small", ID: 1}
	m.RunnerStarted("nas", small, false)
	m.RunnerExited("nas", small, pool.ExitOOM)
	m.HostLinkError("nas")
	m.GitHubError(github.OpPoll)
	m.QueueRead(github.ReadUnchanged)
	m.JobAssigned("small", 3*time.Second)
	m.JobFinished("small", pool.JobFailed, time.Minute)
	t.Run("events counted", func(t *testing.T) {
		fams := gather(t, m)
		wantValue(t, fams, 1, "manade_runner_starts_total", "nas", "acme/app", "small", "error")
		wantValue(t, fams, 1, "manade_runner_exits_total", "nas", "acme/app", "small", "oom")
		wantValue(t, fams, 1, "manade_host_link_errors_total", "nas")
		wantValue(t, fams, 1, "manade_github_errors_total", "poll")
		wantValue(t, fams, 1, "manade_rest_reads_total", "unchanged")
		if s, _ := family(t, fams, "manade_job_queue_seconds").Sample("small"); s.Histogram.Count != 1 || s.Histogram.Sum != 3 {
			t.Errorf("job queue %+v, want one 3-second wait", s.Histogram)
		}
		if s, _ := family(t, fams, "manade_job_duration_seconds").Sample("small", "failed"); s.Histogram.Count != 1 || s.Histogram.Sum != 60 {
			t.Errorf("job duration %+v, want one failed minute", s.Histogram)
		}
	})
	next := settings()
	next.Hosts = next.Hosts[:1]
	next.Profiles[0].Hosts = []string{"default"}
	next.Profiles = append(next.Profiles[:1], config.Profile{Name: "gpu", Hosts: []string{"default"}, Scopes: []string{"acme"}})
	m.Generation(next, 2, at.Add(time.Hour))
	t.Run("a later generation adds and deletes", func(t *testing.T) {
		fams := gather(t, m)
		wantValue(t, fams, unix(at.Add(time.Hour)), "manade_generation_applied_timestamp_seconds")
		wantAbsent(t, fams, "manade_runner_starts_total", "nas", "acme/app", "small", "error")
		wantAbsent(t, fams, "manade_runner_exits_total", "default", "acme/app", "large", "oom")
		wantAbsent(t, fams, "manade_host_link_errors_total", "nas")
		wantValue(t, fams, 0, "manade_runner_starts_total", "default", "acme", "gpu", "ok")
		wantValue(t, fams, 0, "manade_runner_exits_total", "default", "acme", "gpu", "lost")
		wantValue(t, fams, 0, "manade_runner_starts_total", "default", "acme/app", "small", "ok")
		if _, ok := family(t, fams, "manade_job_queue_seconds").Sample("large"); ok {
			t.Error("the dropped profile's queue histogram is still on the page")
		}
		if _, ok := family(t, fams, "manade_job_duration_seconds").Sample("gpu", "succeeded"); !ok {
			t.Error("the new profile's duration histogram is not seeded")
		}
		if s, _ := family(t, fams, "manade_job_queue_seconds").Sample("small"); s.Histogram.Count != 1 {
			t.Errorf("small's queue histogram %+v, want its wait kept across the generation", s.Histogram)
		}
	})
	t.Run("an event outside the settings is dropped", func(t *testing.T) {
		m.RunnerStarted("nas", small, true)
		m.RunnerExited("default", github.ScaleSet{Scope: "acme/app", Profile: "large"}, pool.ExitFailed)
		m.HostLinkError("nas")
		m.JobAssigned("large", time.Second)
		m.JobFinished("large", pool.JobSucceeded, time.Second)
		fams := gather(t, m)
		wantAbsent(t, fams, "manade_runner_starts_total", "nas", "acme/app", "small", "ok")
		wantAbsent(t, fams, "manade_runner_exits_total", "default", "acme/app", "large", "failed")
		wantAbsent(t, fams, "manade_host_link_errors_total", "nas")
		if _, ok := family(t, fams, "manade_job_queue_seconds").Sample("large"); ok {
			t.Error("a dropped profile's wait put its histogram back")
		}
		if _, ok := family(t, fams, "manade_job_duration_seconds").Sample("large", "succeeded"); ok {
			t.Error("a dropped profile's run put its histogram back")
		}
	})
}

// A profile leaving a scope keeps that scope's series while it finishes
// the scope's jobs, since it still serves it.
func TestARetiringProfileKeepsItsSeries(t *testing.T) {
	snap := fixture()
	m := newMetrics(t, &snap)
	cfg := settings()
	cfg.Profiles[1].Scopes, cfg.Profiles[1].Retiring = []string{"acme"}, []string{"acme/app"}
	m.Generation(cfg, 1, at)
	fams := gather(t, m)
	wantValue(t, fams, 0, "manade_runner_starts_total", "default", "acme/app", "large", "ok")
	wantValue(t, fams, 0, "manade_runner_starts_total", "default", "acme", "large", "ok")
}

// The buckets are chartdata's ladder up to its first edge at or above 7
// days, so a job at max_lifetime's 5-day cap lands in a finite bucket.
func TestDurationsUseTheLadderToSevenDays(t *testing.T) {
	const day = 24 * 3600.0
	want := []float64{0, 1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600, 7200, 21600, 43200, day, 3 * day, 7 * day}
	snap := fixture()
	m := newMetrics(t, &snap)
	m.Generation(settings(), 1, at)
	m.JobFinished("small", pool.JobSucceeded, 5*24*time.Hour)
	m.JobAssigned("small", 24*time.Hour)
	fams := gather(t, m)
	for _, c := range []struct {
		name   string
		labels []string
		upper  float64
	}{
		{"manade_job_duration_seconds", []string{"small", "succeeded"}, 7 * day},
		{"manade_job_queue_seconds", []string{"small"}, day},
	} {
		s, _ := family(t, fams, c.name).Sample(c.labels...)
		var bounds []float64
		for _, b := range s.Histogram.Buckets {
			bounds = append(bounds, b.UpperBound)
		}
		if !slices.Equal(bounds, want) {
			t.Errorf("%s bounds %v, want %v", c.name, bounds, want)
			continue
		}
		for _, b := range s.Histogram.Buckets {
			want := uint64(0)
			if b.UpperBound >= c.upper {
				want = 1
			}
			if b.Count != want {
				t.Errorf("%s le=%v = %d, want %d for one observation at %vs", c.name, b.UpperBound, b.Count, want, c.upper)
			}
		}
	}
}

func TestStoreAndHousekeepingSeries(t *testing.T) {
	st := &fakeStore{size: 3 << 20, free: 40 << 30, failures: 2}
	due := at.Add(2 * time.Hour)
	m := newMetricsOver(t, &Sources{
		Pool: func() pool.Snapshot { return pool.Snapshot{} }, Store: st, Housekeeping: func() time.Time { return due },
	})
	fams := gather(t, m)
	wantValue(t, fams, 3<<20, "manade_store_size_bytes")
	wantValue(t, fams, 40<<30, "manade_store_free_bytes")
	wantValue(t, fams, 2, "manade_store_write_failures_total")
	wantValue(t, fams, unix(due), "manade_housekeeping_deadline_timestamp_seconds")
	st.sizeErr = errors.New("test: the file is gone")
	fams = gather(t, m)
	wantAbsent(t, fams, "manade_store_size_bytes")
	wantValue(t, fams, 40<<30, "manade_store_free_bytes")
	st.sizeErr, st.freeErr = nil, errors.New("test: statfs failed")
	fams = gather(t, m)
	wantValue(t, fams, 3<<20, "manade_store_size_bytes")
	wantAbsent(t, fams, "manade_store_free_bytes")
	wantValue(t, fams, 2, "manade_store_write_failures_total")
}

func TestHousekeepingDeadlineIsZeroBeforeItsRun(t *testing.T) {
	snap := pool.Snapshot{}
	wantValue(t, gather(t, newMetrics(t, &snap)), 0, "manade_housekeeping_deadline_timestamp_seconds")
}

// A failing store read is warned once while it fails, and again once it
// fails after a good read.
func TestAFailingStoreReadIsWarnedOncePerStreak(t *testing.T) {
	var log bytes.Buffer
	st := &fakeStore{sizeErr: errors.New("test: the file is gone")}
	m, err := New("v1.0.0-test", slog.New(slog.NewTextHandler(&log, nil)))
	if err != nil {
		t.Fatal(err)
	}
	m.Ready(&Sources{
		Pool: func() pool.Snapshot { return pool.Snapshot{} }, Store: st, Housekeeping: func() time.Time { return time.Time{} },
	})
	const line = "Reading the store's size failed"
	gather(t, m)
	gather(t, m)
	if n := strings.Count(log.String(), line); n != 1 {
		t.Errorf("warnings over two failing reads = %d, want 1", n)
	}
	st.sizeErr = nil
	gather(t, m)
	st.sizeErr = errors.New("test: the file is gone again")
	gather(t, m)
	if n := strings.Count(log.String(), line); n != 2 {
		t.Errorf("warnings after a good read and a new failure = %d, want 2", n)
	}
}
