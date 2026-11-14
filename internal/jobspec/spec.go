// Package jobspec builds, inside a runner host, the Docker create request for
// each runner from the host's own copy of its profile, and checks that the
// runner's image does not run as root. It is the only code that writes a
// create request, and it passes no raw Docker option through: every setting
// comes from a profile field, the host's platform, or is fixed here.
package jobspec

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// The labels a runner's, a probe's or an image check's container carries.
// LabelRunner also names a docker-image runner's /var/lib/docker volume.
const (
	LabelRunner   = "manade.runner"
	LabelRunnerID = "manade.runner-id"
	LabelScope    = "manade.scope"
	LabelProfile  = "manade.profile"
	// LabelProbe is the probe run a probe or listener container belongs to.
	LabelProbe = "manade.probe"
	// LabelImageCheck is the image ID an image check's container reads.
	LabelImageCheck = "manade.image-check"
)

// jitConfigEnv is the variable GitHub's runner reads its one-time config from.
const jitConfigEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// The host's daemon socket inside the host container, and where a
// host-socket job finds it: the Docker CLI's default.
const (
	hostSocket   = "/run/docker.sock"
	socketTarget = "/var/run/docker.sock"
)

// jobDockerRoot is where a docker-image runner's own daemon keeps its data,
// on a volume of its own: the host's data root is overlayfs to the job.
const jobDockerRoot = "/var/lib/docker"

// The probe and listener containers' limits: one static binary making a few
// connections. The host reserve counts both memory limits.
const (
	ProbeMemory = 32 << 20
	probePIDs   = 64
	probeUser   = "65534:65534"
)

// Platform is what the host's start found that a spec depends on.
type Platform struct {
	// Network is the job network every runner joins.
	Network string
	// Mounts are the host container's mount points as its start read them,
	// the root filesystem and Docker's per-container file mounts left out.
	Mounts []string
	// CPUs is the size of the host's effective cpuset.
	CPUs int
	// SocketGID owns the host's daemon socket, for a host-socket job.
	SocketGID uint32
	// Pinning is a kernel without CFS quota: cpus are whole pinned CPUs.
	Pinning bool
	// PIDs, Swap and Disk say whether the kernel enforces a pids limit, a
	// swap limit and a writable-layer size (overlay2 over XFS pquota).
	PIDs bool
	Swap bool
	Disk bool
}

// Host builds the specs of one runner host.
type Host struct {
	host *config.Host
	// protected are the paths no job on a host that is not open may
	// reach, each also with its symlinks resolved.
	protected []string
	// binds are the bind sources of the profiles the host serves.
	binds []config.BindSource
	plat  Platform
}

// NewHost builds the specs of host h of cfg on platform plat.
func NewHost(cfg *config.Config, h *config.Host, plat *Platform) *Host {
	paths := cfg.Protected(h)
	prot := make([]string, 0, 2*len(paths))
	for _, p := range paths {
		prot = append(prot, withResolved(p)...)
	}
	return &Host{host: h, protected: prot, binds: cfg.BindSources(h.Name), plat: *plat}
}

// errUnenforceable is a profile asking for a limit its host's kernel or
// filesystem cannot enforce.
var errUnenforceable = errors.New("jobspec: the host cannot enforce the profile's limits")

// Check refuses profile p on this host when it asks for a limit the host
// cannot enforce, naming the profile and the host. A pids limit the kernel
// does not enforce is left out of the spec instead: every profile has one.
func (h *Host) Check(p *config.Profile) error {
	var why []string
	if p.Disk > 0 && !h.plat.Disk {
		why = append(why, "disk needs overlay2 over XFS mounted with pquota")
	}
	if p.Swap > 0 && !h.plat.Swap {
		why = append(why, "swap needs the kernel's swap accounting")
	}
	if h.plat.Pinning && p.MilliCPUs%1000 != 0 {
		why = append(why, "cpus must be whole CPUs on a kernel without CFS quota")
	}
	if h.plat.Pinning && int(p.MilliCPUs/1000) > h.plat.CPUs {
		why = append(why, fmt.Sprintf("cpus %d is more than the host's %d CPUs", p.MilliCPUs/1000, h.plat.CPUs))
	}
	if len(why) == 0 {
		return nil
	}
	return fmt.Errorf("%w: profile %s on host %s: %s", errUnenforceable, p.Name, h.host.Name, strings.Join(why, "; "))
}

// Runner is one runner's identity, its one-time config and what the host
// gives it beyond its profile.
type Runner struct {
	Scope string
	Name  string
	// ImageID is the checked image ID the container is created from, never
	// a tag, so a tag moved since the check cannot slip past it.
	ImageID string
	// JITConfig is the runner's one-time config: a credential, which the
	// create request carries and nothing may log.
	JITConfig string
	// DockerVolume is the docker image's /var/lib/docker volume, required
	// for that image and refused for any other.
	DockerVolume string
	// CPUs are the runner's pinned CPUs on a pinning host, as many as the
	// profile's cpus; nil on a quota host.
	CPUs []int
	ID   int64
}

// Runner is the create request for r on profile p, which Check passed. Each
// bind source is resolved again here, every symlink followed, so call it
// just before the create: on a host that is not open a source that reaches
// a protected path or leaves the host's mounts fails with errMountRefused.
func (h *Host) Runner(p *config.Profile, r *Runner) (*docker.CreateRequest, error) {
	hc, err := h.limits(p, r.CPUs)
	if err != nil {
		return nil, err
	}
	if hc.Mounts, err = h.mounts(p); err != nil {
		return nil, err
	}
	switch {
	case (p.RunnerImage == config.ShippedDocker) != (r.DockerVolume != ""):
		return nil, fmt.Errorf("jobspec: profile %s: a /var/lib/docker volume goes with runner_image %s and only with it", p.Name, config.ShippedDocker)
	case r.DockerVolume != "":
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: "volume", Source: r.DockerVolume, Target: jobDockerRoot})
	}
	if p.Docker == config.DockerHostSocket {
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: "bind", Source: hostSocket, Target: socketTarget})
		hc.GroupAdd = []string{strconv.FormatUint(uint64(h.plat.SocketGID), 10)}
	}
	return &docker.CreateRequest{
		Image:      r.ImageID,
		User:       p.User,
		Entrypoint: slices.Clone(p.Command),
		Env:        []string{jitConfigEnv + "=" + r.JITConfig},
		Labels: map[string]string{
			LabelScope: r.Scope, LabelProfile: p.Name, LabelRunner: r.Name, LabelRunnerID: strconv.FormatInt(r.ID, 10),
		},
		HostConfig: *hc,
	}, nil
}

// limits is p's HostConfig without its mounts.
func (h *Host) limits(p *config.Profile, cpus []int) (*docker.HostConfig, error) {
	initOn := true
	hc := &docker.HostConfig{
		Memory:      p.Memory,
		ShmSize:     p.ShmSize,
		Init:        &initOn,
		Runtime:     p.Runtime,
		Privileged:  p.Privileged,
		CapDrop:     []string{"ALL"},
		CapAdd:      slices.Clone(p.Capabilities),
		NetworkMode: h.plat.Network,
		LogConfig:   docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	switch {
	case !h.plat.Pinning && cpus != nil:
		return nil, fmt.Errorf("jobspec: profile %s: pinned CPUs on a host with CFS quota", p.Name)
	case !h.plat.Pinning:
		hc.NanoCPUs = p.MilliCPUs * 1_000_000
	case int64(len(cpus))*1000 != p.MilliCPUs:
		return nil, fmt.Errorf("jobspec: profile %s: %d pinned CPUs for cpus %d", p.Name, len(cpus), p.MilliCPUs/1000)
	default:
		hc.CpusetCpus = cpuList(cpus)
	}
	hc.MemorySwap = h.memorySwap(p.Memory + p.Swap)
	if h.plat.PIDs {
		pids := p.PIDs
		hc.PidsLimit = &pids
	}
	if !p.AllowPrivilegeEscalation {
		hc.SecurityOpt = []string{"no-new-privileges:true"}
	}
	if p.Disk > 0 {
		hc.StorageOpt = map[string]string{"size": strconv.FormatInt(p.Disk, 10)}
	}
	for _, t := range p.Tmpfs {
		if hc.Tmpfs == nil {
			hc.Tmpfs = map[string]string{}
		}
		// Docker mounts a tmpfs noexec by default, which would stop a job
		// running what it builds there, such as go test's binaries.
		hc.Tmpfs[t.Path] = "size=" + strconv.FormatInt(t.Size, 10) + ",exec"
	}
	return hc, nil
}

// memorySwap is the MemorySwap for a memory-plus-swap limit of total. On a
// kernel without swap accounting it is -1: Docker warns on any other value
// with a memory limit (verifyPlatformContainerResources,
// https://github.com/moby/moby/blob/docker-v29.0.0/daemon/daemon_unix.go),
// and a create with a warning is removed and failed.
func (h *Host) memorySwap(total int64) int64 {
	if !h.plat.Swap {
		return -1
	}
	return total
}

func cpuList(cpus []int) string {
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = strconv.Itoa(c)
	}
	return strings.Join(s, ",")
}

// Probe is a probe run's container: the probe stepped over its attached
// standard input when stdin is true, the listener otherwise. It is created
// as a job's container is, on the job network with Docker's default
// capabilities, a non-root user and no mounts.
type Probe struct {
	// Run labels both of a run's containers.
	Run     string
	ImageID string
	Command []string
	Stdin   bool
}

// Probe is the create request for pr.
func (h *Host) Probe(pr *Probe) *docker.CreateRequest {
	initOn := true
	hc := docker.HostConfig{
		Memory:      ProbeMemory,
		Init:        &initOn,
		NetworkMode: h.plat.Network,
		SecurityOpt: []string{"no-new-privileges:true"},
		LogConfig:   docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "1m", "max-file": "1"}},
	}
	hc.MemorySwap = h.memorySwap(ProbeMemory)
	if h.plat.PIDs {
		pids := int64(probePIDs)
		hc.PidsLimit = &pids
	}
	return &docker.CreateRequest{
		Image:       pr.ImageID,
		User:        probeUser,
		Entrypoint:  slices.Clone(pr.Command),
		Labels:      map[string]string{LabelProbe: pr.Run},
		HostConfig:  hc,
		OpenStdin:   pr.Stdin,
		StdinOnce:   pr.Stdin,
		AttachStdin: pr.Stdin, AttachStdout: pr.Stdin, AttachStderr: pr.Stdin,
	}
}

// imageCheckEntrypoint names no file an image is expected to hold, so a
// container started by mistake exits at once.
const imageCheckEntrypoint = "/manade-image-check"

// ImageCheck is the create request of the container the user check reads
// imageID's passwd and group files from. It is never started: it has no
// network and no command an image could run.
func ImageCheck(imageID string) *docker.CreateRequest {
	return &docker.CreateRequest{
		Image:      imageID,
		Entrypoint: []string{imageCheckEntrypoint},
		Labels:     map[string]string{LabelImageCheck: imageID},
		HostConfig: docker.HostConfig{NetworkMode: "none"},
	}
}
