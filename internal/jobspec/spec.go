// Package jobspec turns a validated profile into the Docker create request
// for its runner, and checks that the runner's image does not run as root. It
// is the only code that writes a create request, and it passes no raw Docker
// option through: every setting comes from a profile field or is fixed here.
package jobspec

import (
	"slices"
	"strconv"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
)

// RunnerRepo is the package the four shipped runner images are published in.
const RunnerRepo = "ghcr.io/cplieger/manade-runner"

// jitConfigEnv is the variable GitHub's runner reads its one-time config from.
const jitConfigEnv = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// socketTarget is where a host-socket job finds the Docker socket: the Docker
// CLI's default.
const socketTarget = "/var/run/docker.sock"

// ProbeKind names what a probe container is created for.
type ProbeKind string

// The probe kinds, the values of the manade.probe label.
const (
	ProbeUser ProbeKind = "user"
	ProbeDisk ProbeKind = "disk"
)

// Host holds what every spec shares: the pool's settings and the Docker
// socket's group.
type Host struct {
	poolName     string
	dockerSocket string
	profiles     []config.Profile
	network      config.Network
	socketGID    uint32
}

// NewHost builds the specs for cfg's pool. socketGID is the Docker socket's
// group, added to a host-socket job's user: the caller reads it only when a
// profile sets docker: host-socket, and passes 0 otherwise.
func NewHost(cfg *config.Config, socketGID uint32) *Host {
	return &Host{
		poolName:     cfg.PoolName,
		dockerSocket: cfg.DockerSocket,
		profiles:     cfg.Profiles,
		network:      cfg.Network,
		socketGID:    socketGID,
	}
}

// Runner is one runner's identity and its one-time config.
type Runner struct {
	Scope string
	Name  string
	// ImageID is the checked image ID the container is created from, never
	// a tag, so a tag moved since the user check cannot slip past it.
	ImageID string
	// JITConfig is the runner's one-time config: a credential, which the
	// create request carries and nothing may log.
	JITConfig string
	ID        int64
}

// Runner is the create request for r on profile p.
func (h *Host) Runner(p *config.Profile, r *Runner) *docker.CreateRequest {
	pids, initOn := p.PIDs, true
	hc := docker.HostConfig{
		Memory:      p.Memory,
		MemorySwap:  p.Memory + p.Swap,
		NanoCPUs:    p.MilliCPUs * 1_000_000,
		PidsLimit:   &pids,
		ShmSize:     p.ShmSize,
		Init:        &initOn,
		Runtime:     p.Runtime,
		Privileged:  p.Privileged,
		CapDrop:     []string{"ALL"},
		CapAdd:      slices.Clone(p.Capabilities),
		NetworkMode: h.networkMode(p),
		LogConfig:   docker.LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
	}
	if !p.AllowPrivilegeEscalation {
		hc.SecurityOpt = []string{"no-new-privileges:true"}
	}
	if p.Disk > 0 {
		hc.StorageOpt = map[string]string{"size": strconv.FormatInt(p.Disk, 10)}
	}
	// Docker refuses Dns beside the host's network namespace.
	if p.Network != config.NetworkHost {
		for _, a := range h.network.DNS {
			hc.DNS = append(hc.DNS, a.String())
		}
	}
	for _, t := range p.Tmpfs {
		if hc.Tmpfs == nil {
			hc.Tmpfs = map[string]string{}
		}
		// Docker mounts a tmpfs noexec by default, which would stop a job
		// running what it builds there, such as go test's binaries.
		hc.Tmpfs[t.Path] = "size=" + strconv.FormatInt(t.Size, 10) + ",exec"
	}
	for _, m := range p.Mounts {
		typ := "volume"
		if m.Bind {
			typ = "bind"
		}
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: typ, Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	if p.Docker == config.DockerHostSocket {
		hc.Mounts = append(hc.Mounts, docker.Mount{Type: "bind", Source: h.dockerSocket, Target: socketTarget})
		hc.GroupAdd = []string{strconv.FormatUint(uint64(h.socketGID), 10)}
	}
	return &docker.CreateRequest{
		Image:      r.ImageID,
		User:       p.User,
		Entrypoint: slices.Clone(p.Command),
		Env:        []string{jitConfigEnv + "=" + r.JITConfig},
		Labels: map[string]string{
			docker.LabelScope:    r.Scope,
			docker.LabelProfile:  p.Name,
			docker.LabelRunner:   r.Name,
			docker.LabelRunnerID: strconv.FormatInt(r.ID, 10),
		},
		HostConfig: hc,
	}
}

// Probe is the create request for a probe container of kind on profile p's
// image: the profile's command as its entrypoint, so it creates from any image
// the runner could start, no network, and nothing else but the disk probe's
// size. A probe is created and removed, never started.
func (h *Host) Probe(p *config.Profile, kind ProbeKind, imageID string) *docker.CreateRequest {
	req := &docker.CreateRequest{
		Image:      imageID,
		Entrypoint: slices.Clone(p.Command),
		Labels:     map[string]string{docker.LabelProbe: string(kind)},
		HostConfig: docker.HostConfig{NetworkMode: "none"},
	}
	if kind == ProbeDisk {
		req.HostConfig.StorageOpt = map[string]string{"size": strconv.FormatInt(p.Disk, 10)}
	}
	return req
}

// Networks are the job networks the profiles use, for EnsureNetwork at boot:
// <pool>-isolated on bridge <pool>0 and <pool>-bridge on bridge <pool>1.
// POOL_NAME's 14-byte bound keeps both names within IFNAMSIZ.
func (h *Host) Networks() []docker.NetworkSpec {
	var isolated, bridge bool
	for i := range h.profiles {
		isolated = isolated || h.profiles[i].Network == config.NetworkIsolated
		bridge = bridge || h.profiles[i].Network == config.NetworkBridge
	}
	var out []docker.NetworkSpec
	if isolated {
		out = append(out, docker.NetworkSpec{Name: h.networkName(config.NetworkIsolated), Bridge: h.poolName + "0", Subnet: h.network.Subnet})
	}
	if bridge {
		out = append(out, docker.NetworkSpec{Name: h.networkName(config.NetworkBridge), Bridge: h.poolName + "1", Subnet: h.network.BridgeSubnet, ICC: true})
	}
	return out
}

func (h *Host) networkName(network string) string { return h.poolName + "-" + network }

func (h *Host) networkMode(p *config.Profile) string {
	if p.Network == config.NetworkHost {
		return "host"
	}
	return h.networkName(p.Network)
}

// ImageRef is the reference profile p's image is pulled by: its image field,
// or a shipped image's moving tag on the channel this binary was built on.
func ImageRef(p *config.Profile, dev bool) string {
	if p.Image != "" {
		return p.Image
	}
	if dev {
		return RunnerRepo + ":" + p.RunnerImage + "-dev"
	}
	return RunnerRepo + ":" + p.RunnerImage
}
