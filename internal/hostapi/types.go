// Package hostapi is the runner host's API: the only way the orchestrator
// reaches a host. The host serves it on a Unix socket or on TCP with mutual
// TLS; the orchestrator's Client calls it. Requests name a profile and a
// runner, never a container spec, a mount, a capability or an image: the host
// builds every container from its own copy of the config.
package hostapi

import (
	"net/netip"
	"time"

	"github.com/cplieger/manade/internal/docker"
)

// The bounds of each request, shared by the server, which cuts a request at
// its bound, and the client, which waits a little longer for that answer.
const (
	// quickTimeout bounds a request the host answers from memory.
	quickTimeout = 30 * time.Second
	// callBound bounds a request that makes one daemon call.
	callBound = docker.CallTimeout + quickTimeout
	// DeleteStopGrace is the longest stop grace DeleteRunner gives a
	// runner before its kill: Docker's own default stop timeout.
	DeleteStopGrace = 10 * time.Second
	// deleteBound bounds a DELETE by Backend.DeleteRunner's waits, one
	// CallTimeout each: a refused start's own removal (its 2 s baseline
	// read inside quickTimeout), the inspect (a window when a cut create may
	// still commit), the diag copy, the stop past its grace, the removal's
	// inspect and removal, and the docker volume's read and removal.
	deleteBound = DeleteStopGrace + 8*docker.CallTimeout + quickTimeout
	// eventsBound ends an events stream, after which the client reconnects
	// and lists again.
	eventsBound = time.Hour
	// Heartbeat is how often an events stream writes a line when no runner
	// changes, so a client can tell an idle stream from a dead link.
	Heartbeat = 30 * time.Second
)

// clientSlack is how much longer the client waits than the server's bound,
// so the server's own answer to a cut request arrives; a test seam.
var clientSlack = 15 * time.Second

// The size bounds.
const (
	// requestLimit caps a request body.
	requestLimit = 64 << 10
	// jitLimit caps a JIT config; GitHub's are a few KiB of base64.
	jitLimit = 48 << 10
	// LogTailDefault and LogTailMax bound a log tail's lines.
	LogTailDefault = 50
	LogTailMax     = 1000
	// logLimit caps a log tail's bytes; docker.LogKeep owns the value.
	logLimit = docker.LogKeep
	// DiagLimit caps a runner's _diag tar.
	DiagLimit = 8 << 20
	// answerLimit caps a JSON answer the client reads.
	answerLimit = 16 << 20
	// eventLimit caps one events line.
	eventLimit = 64 << 10
)

// OrchestratorName is the DNS name the orchestrator's client certificate
// carries and the host requires.
const OrchestratorName = "manade-orchestrator"

// Status is the host's account of itself (GET /v1/status). The orchestrator
// gives a host work only while its hashes match its own copy and its proof
// holds, and exports each figure.
type Status struct {
	// IPv6 is set only on a host with ipv6 and internet both true.
	IPv6 *IPv6Status `json:"ipv6,omitempty"`
	// GitHub is set only on an internet: false host.
	GitHub      *GitHubStatus   `json:"github,omitempty"`
	InstanceID  string          `json:"instance_id"`
	Version     string          `json:"version"`
	PolicyHash  string          `json:"policy_hash"`
	ProfileHash string          `json:"profile_hash"`
	Daemon      DaemonStatus    `json:"daemon"`
	Rules       RulesStatus     `json:"rules"`
	DNS         DNSStatus       `json:"dns"`
	Profiles    []ProfileStatus `json:"profiles"`
	// StartLatency holds one histogram per daemon call a runner start
	// makes (create, start), over LatencyBuckets.
	StartLatency []CallLatency `json:"start_latency"`
	Outward      Outward       `json:"outward"`
	OOM          OOMStatus     `json:"oom"`
	Limits       HostLimits    `json:"limits"`
	Memory       MemoryStatus  `json:"memory"`
	Data         DataStatus    `json:"data"`
	// ProbeDeadline bounds a POST /v1/probe: two runs of the host's
	// current probe plan, the one a request may wait out and its own.
	ProbeDeadline time.Duration `json:"probe_deadline_ns"`
}

// Outward is the host container's own network: where its traffic leaves.
type Outward struct {
	Address  netip.Addr `json:"address"`
	Gateway  netip.Addr `json:"gateway"`
	Address6 netip.Addr `json:"address6,omitzero"`
	Gateway6 netip.Addr `json:"gateway6,omitzero"`
}

// CPUMode is how a host enforces a profile's cpus.
type CPUMode string

// The CPU modes.
const (
	// CPUQuota is a CFS quota per runner.
	CPUQuota CPUMode = "quota"
	// CPUPinning gives each runner whole CPUs of the host's cpuset, on a
	// kernel without CFS quota.
	CPUPinning CPUMode = "pinning"
)

// HostLimits are the limits the host's kernel enforces on all its jobs
// together, read from its cgroup.
type HostLimits struct {
	CPUMode CPUMode `json:"cpu_mode"`
	// CPUSet is the host's effective cpuset, as the kernel lists it.
	CPUSet    string `json:"cpuset"`
	MilliCPUs int64  `json:"milli_cpus"`
	Memory    int64  `json:"memory"`
	// PIDs is 0 where the kernel has no pids controller, so a profile's
	// pids is not enforced.
	PIDs int64 `json:"pids"`
	// Disk is true only on overlay2 over XFS with pquota, the one place a
	// profile's disk is enforced.
	Disk bool `json:"disk"`
}

// MemoryStatus is what jobs may use of the host's memory limit.
type MemoryStatus struct {
	// Limit is the host cgroup's memory limit, 0 for none, which admits
	// nothing.
	Limit int64 `json:"limit"`
	// Admissible is the cgroup limit less Reserve: the most a capacity's
	// memory may be.
	Admissible int64 `json:"admissible"`
	Reserve    int64 `json:"reserve"`
	// ReserveRunners is the runner count Reserve covers.
	ReserveRunners int `json:"reserve_runners"`
	// Own is the host's own non-job memory: resident and file-mapped,
	// never unmapped page cache.
	Own int64 `json:"own"`
}

// OOMStatus is the host cgroup's OOM kill count, its jobs' kills included.
type OOMStatus struct {
	// Kills is nil where the kernel's count is not hierarchical; Absent
	// then says why.
	Kills  *uint64 `json:"kills,omitempty"`
	Absent string  `json:"absent,omitempty"`
	// SubtreeWide is true on cgroup v1, where one kill at the host's limit
	// sets every running job's OOM flag.
	SubtreeWide bool `json:"subtree_wide"`
}

// DaemonStatus is the host's own Docker daemon.
type DaemonStatus struct {
	Version       string `json:"version"`
	StorageDriver string `json:"storage_driver"`
}

// DataStatus is the data root's filesystem, the host's disk budget.
type DataStatus struct {
	Size uint64 `json:"size"`
	Free uint64 `json:"free"`
}

// RulesState is whether the loaded rules are the config's.
type RulesState string

// The rules states.
const (
	RulesLoaded RulesState = "loaded"
	// RulesKept is a SIGHUP whose section did not validate: the old rules
	// stay loaded and ConfigError says why.
	RulesKept RulesState = "kept"
)

// RulesStatus is the host's egress rules.
type RulesStatus struct {
	State RulesState `json:"state"`
	// Backend is "nftables" or "iptables".
	Backend     string `json:"backend"`
	ConfigError string `json:"config_error,omitempty"`
	// Restored counts the rule sets loaded again after the 5-second check
	// found them changed.
	Restored uint64 `json:"restored"`
	// RawOff is true where the kernel has no iptables raw table, so
	// Docker's raw rules are off.
	RawOff bool `json:"raw_off"`
}

// DNSStatus is the host's resolvers.
type DNSStatus struct {
	Configured []netip.Addr `json:"configured"`
	// Original are the resolvers the host's resolv.conf named before the
	// host wrote its own; OriginalUnknown when it named only Docker's
	// embedded resolver with no ExtServers comment.
	Original        []netip.Addr `json:"original"`
	OriginalUnknown bool         `json:"original_unknown"`
}

// Reason is why a profile is not ready on a host.
type Reason string

// The reasons.
const (
	// ReasonPulling is a profile whose image has not come into service.
	ReasonPulling Reason = "pulling"
	// ReasonImageMissing is an image the daemon no longer held at a create;
	// the host pulls it again.
	ReasonImageMissing Reason = "image_missing"
	// ReasonRefused is a profile whose registry resolved outside an
	// internet: false host's allow entries.
	ReasonRefused Reason = "refused"
	// ReasonDataFull is every profile while a start the daemon refused for
	// want of space holds the host's starts; Detail says when it lifts.
	ReasonDataFull Reason = "data_full"
	// ReasonEgressChangePending is every profile while the host applies a
	// change of its open or internet key: until each runner on it has
	// exited or been removed, then while it rebuilds its egress; Detail
	// names the runners.
	ReasonEgressChangePending Reason = "egress_change_pending"
	// ReasonEgressChangeFailed is every profile once such a rebuild failed
	// and the host went back to its old egress; Detail is the failure, which
	// a later SIGHUP or a restart retries.
	ReasonEgressChangeFailed Reason = "egress_change_failed"
)

// ProfileStatus is one profile's readiness and image on a host.
type ProfileStatus struct {
	// CheckedAt is the last good check of the image in service.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// FailedAt is when Failure happened.
	FailedAt time.Time `json:"failed_at,omitzero"`
	Name     string    `json:"name"`
	Reason   Reason    `json:"reason,omitempty"`
	// Detail says what Reason means for this profile now: the failure, the
	// addresses refused and the ways out.
	Detail string `json:"detail,omitempty"`
	// Image is the reference the profile names; Digest and ImageID the
	// image in service.
	Image   string `json:"image"`
	Digest  string `json:"digest,omitempty"`
	ImageID string `json:"image_id,omitempty"`
	// Failure is the last failed pull or check; Failures counts them since
	// the host started.
	Failure  string `json:"failure,omitempty"`
	Failures uint64 `json:"failures"`
	Ready    bool   `json:"ready"`
}

// IPv6Status is the machine's /64 the host learned.
type IPv6Status struct {
	// Reason says why Prefix is unknown.
	Reason string       `json:"reason,omitempty"`
	Prefix netip.Prefix `json:"prefix,omitzero"`
}

// GitHubStatus is an internet: false host's GitHub names and addresses.
type GitHubStatus struct {
	DomainsAt time.Time `json:"domains_at"`
	// Source is "read" for a list read from GitHub, "saved" for the copy
	// on the data volume.
	Source string `json:"source"`
	// Addresses are the learned IPv4 addresses held, the only family an
	// internet: false host learns; Refused counts adds a full set refused.
	Addresses int    `json:"addresses"`
	Refused   uint64 `json:"refused"`
}

// LatencyBuckets are the upper bounds, in seconds, of a CallLatency's
// buckets: from an idle daemon's tens of milliseconds to docker.CallTimeout.
var LatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 180}

// CallLatency is a histogram of one daemon call's latency.
type CallLatency struct {
	Call string `json:"call"`
	// Buckets are cumulative counts, one per LatencyBuckets bound.
	Buckets []uint64 `json:"buckets"`
	Count   uint64   `json:"count"`
	// Sum is in seconds.
	Sum float64 `json:"sum"`
}

// RunnerRequest is a POST /v1/runners body.
type RunnerRequest struct {
	Profile    string `json:"profile"`
	RunnerName string `json:"runner_name"`
	Scope      string `json:"scope"`
	// JITConfig is GitHub's one-time runner config, a credential: never
	// log it.
	JITConfig string `json:"jit_config"`
	RunnerID  int64  `json:"runner_id"`
}

// RunnerState is a runner's state in a list.
type RunnerState string

// The runner states.
const (
	StateCreating RunnerState = "creating"
	StateStarting RunnerState = "starting"
	StateRunning  RunnerState = "running"
	StateExited   RunnerState = "exited"
	StateFailed   RunnerState = "failed"
)

// Limits are what a runner's container was built with, or for a runner
// still creating, what it will be built with.
type Limits struct {
	// CPUSet is the pinned CPUs on a pinning host; MilliCPUs the quota
	// elsewhere.
	CPUSet    string `json:"cpuset,omitempty"`
	MilliCPUs int64  `json:"milli_cpus,omitempty"`
	Memory    int64  `json:"memory"`
	Disk      int64  `json:"disk,omitempty"`
}

// Runner is one runner the host has accepted, as a list shows it.
type Runner struct {
	StartedAt time.Time `json:"started_at,omitzero"`
	ExitedAt  time.Time `json:"exited_at,omitzero"`
	// ExitCode is set for an exited runner only.
	ExitCode *int        `json:"exit_code,omitempty"`
	Name     string      `json:"name"`
	Profile  string      `json:"profile"`
	Scope    string      `json:"scope"`
	State    RunnerState `json:"state"`
	// Reason says why a failed runner failed.
	Reason string `json:"reason,omitempty"`
	// RemovedBy names who removed the runner's container and why, so its
	// exit is never read as the runner's own error.
	RemovedBy string `json:"removed_by,omitempty"`
	// Orphans are containers of this runner's name that outlived its
	// failed entry; its DELETE removes them.
	Orphans   []string `json:"orphans,omitempty"`
	Limits    Limits   `json:"limits"`
	RunnerID  int64    `json:"runner_id"`
	OOMKilled bool     `json:"oom_killed"`
}

// RunnerAnswer is a POST /v1/runners answer.
type RunnerAnswer struct {
	InstanceID string `json:"instance_id"`
	Runner     Runner `json:"runner"`
}

// RunnerList is a GET /v1/runners answer: every runner the supervisor with
// InstanceID holds, from its POST's answer on.
type RunnerList struct {
	InstanceID string   `json:"instance_id"`
	Runners    []Runner `json:"runners"`
}

// EventType is what an events line carries.
type EventType string

// The event types.
const (
	EventRunner    EventType = "runner"
	EventHeartbeat EventType = "heartbeat"
)

// Event is one line of GET /v1/events: a runner's new state, or a
// heartbeat with no runner.
type Event struct {
	Time       time.Time `json:"time"`
	Runner     *Runner   `json:"runner,omitempty"`
	Type       EventType `json:"type"`
	InstanceID string    `json:"instance_id"`
}

// ProbeReport is a POST /v1/probe answer.
type ProbeReport struct {
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at"`
	InstanceID string    `json:"instance_id"`
	// Unproven says why the run ended before its checks did: a daemon
	// call, an attach or an exchange that timed out.
	Unproven string       `json:"unproven,omitempty"`
	Checks   []ProbeCheck `json:"checks"`
	// Proven holds only when the run finished and every required check
	// passed.
	Proven bool `json:"proven"`
}

// ProbeCheck is one check of a probe run.
type ProbeCheck struct {
	Name string `json:"name"`
	// Leg is "job" or "output".
	Leg     string `json:"leg"`
	Target  string `json:"target"`
	Outcome string `json:"outcome"`
	// Reason says why the check failed; "" when it passed.
	Reason   string        `json:"reason,omitempty"`
	Duration time.Duration `json:"duration_ns"`
	Pass     bool          `json:"pass"`
	Required bool          `json:"required"`
	// Unproven is a timeout no drop counted.
	Unproven bool `json:"unproven"`
}
