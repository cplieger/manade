package obs

import (
	"context"
	"slices"

	"github.com/cplieger/go/measure/v2"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
)

// familyIPv4 is the one family an internet: false host learns
// (hostapi.GitHubStatus).
const familyIPv4 = "ipv4"

// The section_pending kinds.
const (
	pendingDrain   = "drain"
	pendingRestart = "restart"
)

// descs is the read series' declarations.
type descs struct {
	booting, headWait                                                  *measure.Desc
	scopeServing, pollLast, queueRead, queueDue, orphaned              *measure.Desc
	queued, waiting, unstarted, runners                                *measure.Desc
	capCPUs, capMemory, capDisk, resCPUs, resMemory, resDisk, accept   *measure.Desc
	hostBuild, connected, serving, egressOpen, proved, probeDue        *measure.Desc
	probeFailed, probeResult, profileReady, imageChecked, imageDue     *measure.Desc
	dataFree, ownMemory, admissible, ipv6Learned, domainsAt, addresses *measure.Desc
	sectionPending, sectionRefused, sectionCurrent, waitingSection     *measure.Desc
	rulesRestored, oomKills, refused, startLatency                     *measure.Desc
	reconciled, reconcileDue                                           *measure.Desc
}

func newDescs() descs {
	host := []string{labelHost}
	gauge := func(name, help string, unit measure.Unit, labels ...string) *measure.Desc {
		return measure.NewGaugeDesc(name, help, unit, labels)
	}
	counter := func(name, help string, labels ...string) *measure.Desc {
		return measure.NewCounterDesc(name, help, measure.UnitCount, labels)
	}
	return descs{
		booting:      gauge("booting", "1 until the first pool generation's boot pass has run.", measure.UnitNone),
		headWait:     gauge("queue_head_wait_seconds", "How long the oldest claim on the queue has waited, 0 with none.", measure.UnitSeconds),
		scopeServing: gauge("scope_serving", "1 while the scope is served.", measure.UnitNone, labelScope),
		pollLast: gauge("poll_last_success_timestamp_seconds",
			"When the scale set's last good poll ended, 0 before the first.", measure.UnitSeconds, labelScope, labelProfile),
		queueRead: gauge("queue_read_last_success_timestamp_seconds",
			"When the last whole read of the repository scope's queued jobs ended, 0 before the first.", measure.UnitSeconds, labelScope),
		queueDue: gauge("queue_read_deadline_timestamp_seconds",
			"When the repository scope's next whole read of its queued jobs is due, 0 before the first.", measure.UnitSeconds, labelScope),
		orphaned: gauge("scale_sets_orphaned",
			"Scale sets the pool recorded that the current settings would not ensure.", measure.UnitCount, labelScope),
		queued:    gauge("jobs_queued", "Queued jobs the last whole read listed.", measure.UnitCount, labelScope, labelProfile),
		waiting:   gauge("jobs_waiting", "Jobs waiting for a runner, by why they wait.", measure.UnitCount, labelProfile, "reason"),
		unstarted: gauge("jobs_assigned_unstarted", "Jobs GitHub assigned that no runner has started.", measure.UnitCount, labelScope, labelProfile),
		runners:   gauge("runners", "Runners on the host by state.", measure.UnitCount, labelHost, "state"),
		capCPUs:   gauge("capacity_cpus", "The CPUs the pool funds on the host.", measure.UnitCount, host...),
		capMemory: gauge("capacity_memory_bytes", "The memory the pool funds on the host.", measure.UnitBytes, host...),
		capDisk:   gauge("capacity_disk_bytes", "The disk the pool funds on the host, where its capacity sets one.", measure.UnitBytes, host...),
		resCPUs:   gauge("reserved_cpus", "The CPUs the host's runners and slots reserve.", measure.UnitCount, host...),
		resMemory: gauge("reserved_memory_bytes", "The memory the host's runners and slots reserve.", measure.UnitBytes, host...),
		resDisk: gauge("reserved_disk_bytes",
			"The disk the host's runners and slots reserve, where its capacity sets one.", measure.UnitBytes, host...),
		accept:     gauge("host_accepting", "1 unless the host is drained.", measure.UnitNone, host...),
		hostBuild:  measure.NewInfoDesc("host_build_info", "The connected host's build version.", []string{labelHost, "version"}),
		connected:  gauge("host_connected", "1 while the host has an open connection.", measure.UnitNone, host...),
		serving:    gauge("host_serving", "1 while the host takes new jobs.", measure.UnitNone, host...),
		egressOpen: gauge("host_egress_open", "1 for a host whose egress is open.", measure.UnitNone, host...),
		proved: gauge("host_egress_proved_timestamp_seconds",
			"When the passing probe of the host's running policy passed, 0 while none holds.", measure.UnitSeconds, host...),
		probeDue: gauge("host_probe_deadline_timestamp_seconds",
			"The deadline of the host's probe in flight, 0 while none runs.", measure.UnitSeconds, host...),
		probeFailed: gauge("host_probe_failed",
			"1 while the host's last completed probe of its running policy failed and none passed since.", measure.UnitNone, host...),
		probeResult: gauge("host_probe_result", "1 for each check the host's last probe passed.", measure.UnitNone, labelHost, "check"),
		profileReady: gauge("host_profile_ready",
			"1 while the host reports the profile ready to start runners.", measure.UnitNone, labelHost, labelProfile),
		imageChecked: gauge("host_image_checked_timestamp_seconds",
			"The host's last good check of the profile's image, 0 before the first.", measure.UnitSeconds, labelHost, labelProfile),
		imageDue: gauge("host_image_check_deadline_timestamp_seconds",
			"When the host's next good check of the profile's image is due, 0 before the first.", measure.UnitSeconds, labelHost, labelProfile),
		dataFree:  gauge("host_data_free_bytes", "Free space on the host's data root.", measure.UnitBytes, host...),
		ownMemory: gauge("host_own_memory_bytes", "The host's own memory, its jobs' left out.", measure.UnitBytes, host...),
		admissible: gauge("host_admissible_memory_bytes",
			"The most the host's capacity memory may be: its memory limit less its reserve.", measure.UnitBytes, host...),
		ipv6Learned: gauge("host_ipv6_prefix_learned", "1 while the IPv6 host knows its machine's /64.", measure.UnitNone, host...),
		domainsAt: gauge("host_github_domains_timestamp_seconds",
			"When the internet: false host's GitHub domain list was read.", measure.UnitSeconds, host...),
		addresses: gauge("host_github_addresses",
			"GitHub addresses the internet: false host holds, by family.", measure.UnitCount, labelHost, "family"),
		sectionPending: gauge("host_section_pending",
			"1 while the host holds a delivered section until its runners end, by what then applies it.", measure.UnitNone, labelHost, "kind"),
		sectionRefused: gauge("host_section_refused", "1 while the host refuses the section last delivered.", measure.UnitNone, host...),
		sectionCurrent: gauge("host_section_current",
			"1 while the host runs the section the control plane last delivered.", measure.UnitNone, host...),
		waitingSection: gauge("host_waiting_for_section",
			"1 while the host has accepted no section and refused none.", measure.UnitNone, host...),
		rulesRestored: counter("host_rules_restored_total", "Egress rule sets the host loaded again after finding them changed.", host...),
		oomKills:      counter("host_oom_kills_total", "OOM kills in the host's cgroup, its jobs' included.", host...),
		refused: counter("host_github_addresses_refused_total",
			"GitHub addresses the internet: false host refused to add to a full set, by family.", labelHost, "family"),
		startLatency: measure.NewHistogramDesc("host_runner_start_seconds", "The host's daemon call latency in a runner start.",
			measure.UnitSeconds, []string{labelHost, "call"}, hostapi.LatencyBuckets),
		reconciled: gauge("reconcile_last_success_timestamp_seconds",
			"When the host's reconcile loop last ended a pass, 0 before the first.", measure.UnitSeconds, host...),
		reconcileDue: gauge("reconcile_deadline_timestamp_seconds",
			"When the host's reconcile loop is due to end its next pass, time inside host and GitHub calls left out; 0 before the first.",
			measure.UnitSeconds, host...),
	}
}

func (d *descs) all() []*measure.Desc {
	return []*measure.Desc{
		d.booting, d.headWait, d.scopeServing, d.pollLast, d.queueRead, d.queueDue, d.orphaned,
		d.queued, d.waiting, d.unstarted, d.runners,
		d.capCPUs, d.capMemory, d.capDisk, d.resCPUs, d.resMemory, d.resDisk, d.accept,
		d.hostBuild, d.connected, d.serving, d.egressOpen, d.proved, d.probeDue,
		d.probeFailed, d.probeResult, d.profileReady, d.imageChecked, d.imageDue,
		d.dataFree, d.ownMemory, d.admissible, d.ipv6Learned, d.domainsAt, d.addresses,
		d.sectionPending, d.sectionRefused, d.sectionCurrent, d.waitingSection,
		d.rulesRestored, d.oomKills, d.refused, d.startLatency,
		d.reconciled, d.reconcileDue,
	}
}

// collect emits the read series from one snapshot.
func (m *Metrics) collect(_ context.Context, e *measure.Emitter) {
	s := m.snapshot()
	d := &m.d
	e.Emit(d.booting, b2f(!s.Booted))
	head := 0.0
	if !s.HeadSince.IsZero() {
		head = s.At.Sub(s.HeadSince).Seconds()
	}
	e.Emit(d.headWait, head)
	m.emitScopes(e, &s)
	m.emitQueue(e, &s)
	for i := range s.Hosts {
		m.emitHost(e, &s.Hosts[i])
	}
}

func (m *Metrics) emitScopes(e *measure.Emitter, s *pool.Snapshot) {
	d := &m.d
	orphans := map[string]int{}
	for _, sc := range s.Scopes {
		orphans[sc.Scope] = 0
		e.Emit(d.scopeServing, b2f(sc.Serving), sc.Scope)
		if sc.Kind == config.ScopeRepo {
			e.Emit(d.queueRead, unixSeconds(sc.ReadAt), sc.Scope)
			e.Emit(d.queueDue, unixSeconds(sc.ReadDue), sc.Scope)
		}
	}
	for _, o := range s.Orphaned {
		orphans[o.Scope]++
	}
	for scope, n := range orphans {
		e.Emit(d.orphaned, float64(n), scope)
	}
}

func (m *Metrics) emitQueue(e *measure.Emitter, s *pool.Snapshot) {
	d := &m.d
	for _, set := range s.Sets {
		e.Emit(d.queued, float64(set.Queued), set.Scope, set.Profile)
		if set.ID != 0 {
			e.Emit(d.unstarted, float64(set.AssignedUnstarted), set.Scope, set.Profile)
			e.Emit(d.pollLast, unixSeconds(set.Polled), set.Scope, set.Profile)
		}
	}
	type key struct {
		profile string
		reason  pool.JobReason
	}
	counts := map[key]int{}
	for i := range s.Waiting {
		counts[key{s.Waiting[i].Profile, s.Waiting[i].Reason}]++
	}
	for _, p := range s.Profiles {
		for _, r := range pool.JobReasons {
			e.Emit(d.waiting, float64(counts[key{p, r}]), p, string(r))
		}
	}
}

// emitHost emits host v's series: the pool's verdicts and the host's
// counters, kept at their last value, always; the figures read from its
// status only while it answers, so they leave the page while it does not.
func (m *Metrics) emitHost(e *measure.Emitter, v *pool.HostView) {
	d, h := &m.d, v.Host
	var starting, idle, busy int
	for _, r := range v.Runners {
		starting, idle, busy = starting+r.Starting, idle+r.Idle, busy+r.Busy
	}
	e.Emit(d.runners, float64(starting), h, "starting")
	e.Emit(d.runners, float64(idle), h, "idle")
	e.Emit(d.runners, float64(busy), h, "busy")
	e.Emit(d.resCPUs, float64(v.Reserved.MilliCPUs)/1000, h)
	e.Emit(d.resMemory, float64(v.Reserved.Memory), h)
	if c := v.Capacity; c != nil {
		e.Emit(d.capCPUs, float64(c.MilliCPUs)/1000, h)
		e.Emit(d.capMemory, float64(c.Memory), h)
		if c.Disk > 0 {
			e.Emit(d.capDisk, float64(c.Disk), h)
			e.Emit(d.resDisk, float64(v.Reserved.Disk), h)
		}
	}
	e.Emit(d.accept, b2f(v.Accepting), h)
	e.Emit(d.connected, b2f(v.Connected), h)
	e.Emit(d.serving, b2f(v.Serving), h)
	e.Emit(d.egressOpen, b2f(v.EgressOpen), h)
	e.Emit(d.proved, unixSeconds(v.ProvedAt), h)
	e.Emit(d.probeDue, unixSeconds(v.ProbeUntil), h)
	e.Emit(d.probeFailed, b2f(v.ProbeFailed), h)
	e.Emit(d.reconciled, unixSeconds(v.Reconciled), h)
	e.Emit(d.reconcileDue, unixSeconds(v.ReconcileDue), h)
	if v.Seen != nil {
		m.emitCounters(e, h, v.Seen)
	}
	if v.Status != nil {
		m.emitStatus(e, v)
	}
}

// emitCounters emits host h's cumulative counts from st, the last status
// any pass read. A counter dropped while its host does not answer would
// come back with no earlier sample in the window, and increase() would
// miss a rise during the gap.
func (m *Metrics) emitCounters(e *measure.Emitter, h string, st *hostapi.Status) {
	d := &m.d
	e.Emit(d.rulesRestored, float64(st.Rules.Restored), h)
	if st.OOM.Kills != nil {
		e.Emit(d.oomKills, float64(*st.OOM.Kills), h)
	}
	if gh := st.GitHub; gh != nil {
		e.Emit(d.refused, float64(gh.Refused), h, familyIPv4)
	}
	for _, c := range st.StartLatency {
		e.EmitHistogram(d.startLatency, c.Buckets, c.Count, c.Sum, h, c.Call)
	}
}

func (m *Metrics) emitStatus(e *measure.Emitter, v *pool.HostView) {
	d, h, st := &m.d, v.Host, v.Status
	e.Emit(d.hostBuild, 1, h, st.Version)
	e.Emit(d.dataFree, float64(st.Data.Free), h)
	e.Emit(d.ownMemory, float64(st.Memory.Own), h)
	e.Emit(d.admissible, float64(st.Memory.Admissible), h)
	if st.IPv6 != nil {
		e.Emit(d.ipv6Learned, b2f(st.IPv6.Prefix.IsValid()), h)
	}
	if gh := st.GitHub; gh != nil {
		e.Emit(d.domainsAt, unixSeconds(gh.DomainsAt), h)
		e.Emit(d.addresses, float64(gh.Addresses), h, familyIPv4)
	}
	state := st.Section.State
	e.Emit(d.sectionCurrent, b2f(v.Current), h)
	e.Emit(d.sectionRefused, b2f(state == hostapi.SectionRefused), h)
	e.Emit(d.sectionPending, b2f(state == hostapi.SectionEgressPending), h, pendingDrain)
	e.Emit(d.sectionPending, b2f(state == hostapi.SectionRestartPending), h, pendingRestart)
	e.Emit(d.waitingSection, b2f(state == hostapi.SectionWaiting), h)
	for i := range st.Profiles {
		ps := &st.Profiles[i]
		if !slices.Contains(v.Profiles, ps.Name) {
			continue
		}
		e.Emit(d.profileReady, b2f(ps.Ready), h, ps.Name)
		e.Emit(d.imageChecked, unixSeconds(ps.CheckedAt), h, ps.Name)
		due := 0.0
		if !ps.CheckedAt.IsZero() {
			due = unixSeconds(ps.CheckedAt.Add(v.ImageRefresh))
		}
		e.Emit(d.imageDue, due, h, ps.Name)
	}
	if v.Probe != nil {
		for _, c := range v.Probe.Checks {
			e.Emit(d.probeResult, b2f(c.Pass), h, c.Name)
		}
	}
}
