package obs

import (
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewSeedsEverySeries(t *testing.T) {
	m, _, _ := newTest(t, defaultCapacity)
	got := gather(t, m)
	ts := float64(start.Unix())
	want := map[string]float64{
		"manade_capacity_cpus{}":                                     16,
		"manade_capacity_memory_bytes{}":                             64 << 30,
		"manade_reconcile_last_success_timestamp_seconds{}":          ts,
		"manade_booting{}":                                           1,
		"manade_poll_last_success_timestamp_seconds{acme/app,small}": ts,
		"manade_poll_last_success_timestamp_seconds{acme,small}":     ts,
		"manade_poll_last_success_timestamp_seconds{acme/app,dind}":  ts,
		"manade_queue_read_last_success_timestamp_seconds{acme/app}": ts,
		"manade_scope_serving{acme/app}":                             0,
		"manade_scope_serving{acme}":                                 0,
		"manade_scale_sets_orphaned{acme/app}":                       0,
		"manade_scale_sets_orphaned{acme}":                           0,
		"manade_image_checked_timestamp_seconds{small}":              0,
		"manade_image_checked_timestamp_seconds{dind}":               0,
		"manade_profile_elevated{dind,network}":                      1,
	}
	for _, r := range []string{"changed", "unchanged", "error"} {
		want["manade_rest_reads_total{"+r+"}"] = 0
	}
	for _, op := range []string{"session", "poll", "jit", "scaleset", "visibility", "installation", "queue"} {
		want["manade_github_errors_total{"+op+"}"] = 0
	}
	for _, op := range []string{"pull", "create", "start", "inspect", "remove", "list", "events", "archive"} {
		want["manade_docker_errors_total{"+op+"}"] = 0
	}
	for _, ss := range []ScaleSet{appSmall, orgSmall, appDind} {
		pair := ss.Scope + "," + ss.Profile + ","
		want["manade_runner_starts_total{"+pair+"ok}"] = 0
		want["manade_runner_starts_total{"+pair+"error}"] = 0
		for _, r := range []string{"completed", "failed", "oom", "lifetime", "idle", "lost"} {
			want["manade_runner_exits_total{"+pair+r+"}"] = 0
		}
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}

	absent := map[string]int{
		// dind may not run in the org scope, so it is no scale set there.
		"manade_runner_starts_total":                       6,
		"manade_runner_exits_total":                        18,
		"manade_poll_last_success_timestamp_seconds":       3,
		"manade_queue_read_last_success_timestamp_seconds": 1,
		"manade_profile_elevated":                          1,
	}
	for name, n := range absent {
		if c := count(t, m, name); c != n {
			t.Errorf("%s has %d series, want %d", name, c, n)
		}
	}
	if _, ok := got["manade_capacity_disk_bytes{}"]; ok {
		t.Error("capacity_disk_bytes served without a disk budget")
	}
}

func TestNewDiskBudget(t *testing.T) {
	m, _, _ := newTest(t, "capacity: {cpus: 4, memory: 8GiB, disk: 100GiB}")
	m.SetSource(func() Snapshot { return Snapshot{Reserved: Resources{Disk: 10 << 30}} })
	got := gather(t, m)
	if v := got["manade_capacity_disk_bytes{}"]; v != 100<<30 {
		t.Errorf("capacity_disk_bytes = %v, want %v", v, 100<<30)
	}
	if v, ok := got["manade_reserved_disk_bytes{}"]; !ok || v != 10<<30 {
		t.Errorf("reserved_disk_bytes = %v (present %v), want %v", v, ok, 10<<30)
	}
}

func TestRecorders(t *testing.T) {
	m, _, c := newTest(t, defaultCapacity)
	at := start.Add(90 * time.Second)
	m.PollOK(orgSmall, at)
	m.QueueRead(ReadUnchanged)
	m.QueueRead(ReadUnchanged)
	m.QueueScopeRead("acme/app", at)
	m.ScopeServing("acme", true)
	m.Orphaned("acme", 2)
	m.RunnerStarted(appDind, true)
	m.RunnerStarted(appDind, false)
	m.RunnerExited(appSmall, ExitOOM)
	m.JobAssigned("small", 40*time.Second)
	m.JobFinished("dind", JobCanceled, time.Hour)
	m.ImageChecked("dind", at)
	m.GitHubError(GitHubInstallation)
	m.DockerError(DockerArchive)
	c.t = at
	m.Pass(t.Context(), true)

	got := gather(t, m)
	for k, v := range map[string]float64{
		"manade_poll_last_success_timestamp_seconds{acme,small}":     float64(at.Unix()),
		"manade_poll_last_success_timestamp_seconds{acme/app,small}": float64(start.Unix()),
		"manade_rest_reads_total{unchanged}":                         2,
		"manade_rest_reads_total{changed}":                           0,
		"manade_queue_read_last_success_timestamp_seconds{acme/app}": float64(at.Unix()),
		"manade_scope_serving{acme}":                                 1,
		"manade_scope_serving{acme/app}":                             0,
		"manade_scale_sets_orphaned{acme}":                           2,
		"manade_scale_sets_orphaned{acme/app}":                       0,
		"manade_runner_starts_total{acme/app,dind,ok}":               1,
		"manade_runner_starts_total{acme/app,dind,error}":            1,
		"manade_runner_exits_total{acme/app,small,oom}":              1,
		"manade_job_queue_seconds{small}":                            1,
		"manade_job_duration_seconds{dind,canceled}":                 1,
		"manade_image_checked_timestamp_seconds{dind}":               float64(at.Unix()),
		"manade_image_checked_timestamp_seconds{small}":              0,
		"manade_github_errors_total{installation}":                   1,
		"manade_docker_errors_total{archive}":                        1,
		"manade_reconcile_last_success_timestamp_seconds{}":          float64(at.Unix()),
	} {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}

	c.t = at.Add(time.Minute)
	m.Pass(t.Context(), false)
	if g := gather(t, m)["manade_reconcile_last_success_timestamp_seconds{}"]; g != float64(at.Unix()) {
		t.Errorf("a failed pass moved the reconcile timestamp to %v", g)
	}
}

func TestJobHistogramBuckets(t *testing.T) {
	m, _, _ := newTest(t, defaultCapacity)
	m.JobAssigned("small", 20*time.Hour)
	m.JobFinished("small", JobSucceeded, 5*time.Hour)
	fams, err := m.reg.Gather(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// A wait up to GitHub's 24-hour queue limit and a run up to its 6-hour
	// job timeout fall in a finite bucket, so a p95 reads a value, not +Inf.
	tops := map[string]float64{}
	for _, f := range fams {
		for _, s := range f.Samples {
			if s.Histogram != nil {
				b := s.Histogram.Buckets
				tops[f.Name] = b[len(b)-1].UpperBound
				if b[len(b)-1].Count != 1 {
					t.Errorf("%s: top bucket holds %d, want the observation", f.Name, b[len(b)-1].Count)
				}
			}
		}
	}
	if tops["manade_job_queue_seconds"] < 24*3600 || tops["manade_job_duration_seconds"] < 6*3600 {
		t.Errorf("top buckets = %v", tops)
	}
}

func TestCollect(t *testing.T) {
	m, _, c := newTest(t, defaultCapacity)
	if n := count(t, m, "manade_runners"); n != 0 {
		t.Errorf("runners has %d series before SetSource, want 0", n)
	}
	head := start.Add(30 * time.Second)
	c.t = start.Add(10 * time.Minute)
	m.SetSource(func() Snapshot {
		return Snapshot{
			HeadSince: head,
			Reserved:  Resources{MilliCPUs: 2500, Memory: 3 << 30, Disk: 1 << 30},
			Counts: []Counts{
				{ScaleSet: appSmall, Starting: 1, Idle: 2, Busy: 3, Queued: 4, AssignedUnstarted: 5, Unassignable: 6, NotServing: 7, ImageMissing: 8},
				{ScaleSet: ScaleSet{"acme", "dind"}, NotAllowed: 2},
			},
		}
	})
	got := gather(t, m)
	for k, v := range map[string]float64{
		"manade_reserved_cpus{}":                               2.5,
		"manade_reserved_memory_bytes{}":                       3 << 30,
		"manade_queue_head_wait_seconds{}":                     570,
		"manade_runners{acme/app,small,starting}":              1,
		"manade_runners{acme/app,small,idle}":                  2,
		"manade_runners{acme/app,small,busy}":                  3,
		"manade_jobs_queued{acme/app,small}":                   4,
		"manade_jobs_assigned_unstarted{acme/app,small}":       5,
		"manade_jobs_unassignable{acme/app,small}":             6,
		"manade_jobs_unfundable{acme/app,small,not_serving}":   7,
		"manade_jobs_unfundable{acme/app,small,image_missing}": 8,
		"manade_jobs_unfundable{acme/app,small,not_allowed}":   0,
		"manade_jobs_unfundable{acme,dind,not_allowed}":        2,
	} {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
		}
	}
	if _, ok := got["manade_reserved_disk_bytes{}"]; ok {
		t.Error("reserved_disk_bytes served without a disk budget")
	}

	m.SetSource(func() Snapshot { return Snapshot{} })
	if g := gather(t, m)["manade_queue_head_wait_seconds{}"]; g != 0 {
		t.Errorf("head wait with no head = %v, want 0", g)
	}
}

func TestHandlerServesTextFormat(t *testing.T) {
	m, _, _ := newTest(t, defaultCapacity)
	rr := httptest.NewRecorder()
	m.Handler().ServeHTTP(rr, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	body, _ := io.ReadAll(rr.Body)
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q", ct)
	}
	for _, line := range []string{
		`manade_scope_serving{scope="acme/app"} 0`,
		"# TYPE manade_runner_exits_total counter",
		"# TYPE go_goroutines gauge",
	} {
		if !strings.Contains(string(body), line) {
			t.Errorf("page lacks %q", line)
		}
	}
}

func TestMetricsConcurrent(t *testing.T) {
	m, _, _ := newTest(t, defaultCapacity)
	m.SetSource(func() Snapshot {
		m.GitHubError(GitHubPoll)
		return Snapshot{Counts: []Counts{{ScaleSet: appSmall, Busy: 1}}}
	})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			at := start.Add(time.Duration(i) * time.Second)
			for range 50 {
				m.PollOK(appSmall, at)
				m.RunnerStarted(appSmall, i%2 == 0)
				m.ScopeServing("acme", i%2 == 0)
				m.Pass(t.Context(), true)
			}
		})
		wg.Go(func() {
			for range 20 {
				if _, err := m.reg.Gather(t.Context()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if g := gather(t, m)["manade_runner_starts_total{acme/app,small,ok}"]; g != 100 {
		t.Errorf("ok starts = %v, want 100", g)
	}
}
