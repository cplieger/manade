package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"syscall"
)

// ErrKernelLimits is returned by Info.CheckLimits when the kernel lacks a
// cgroup controller a profile's hard limits need.
var ErrKernelLimits = errors.New("docker: the kernel cannot enforce the runner limits")

// Info is what the pool reads from /info.
type Info struct {
	// FirewallBackend is FirewallBackend.Driver, "" below API 1.49, which
	// does not report it.
	FirewallBackend string `json:"-"`
	DataRoot        string `json:"DockerRootDir"`
	// ContainerdAddress is Containerd.Address, "" below API 1.46.
	ContainerdAddress string `json:"-"`
	Driver            string
	DriverStatus      [][2]string
	// Runtimes is sorted.
	Runtimes    []string `json:"-"`
	MemoryLimit bool
	SwapLimit   bool
	PidsLimit   bool
	CPUCFSQuota bool `json:"CpuCfsQuota"`
}

// The API versions that added the /info fields read past the floor.
const (
	containerdVersion apiVersion = 1<<16 | 46
	firewallVersion   apiVersion = 1<<16 | 49
)

// Info reads /info. A field added after the floor is read only at a
// negotiated version that has it, because a daemon answering an older
// version strips it.
func (c *Client) Info(ctx context.Context) (Info, error) {
	// The outer fields decode the wire forms of Info's derived fields.
	var w struct {
		Runtimes        map[string]struct{}
		Containerd      *struct{ Address string }
		FirewallBackend *struct{ Driver string }
		Info
	}
	if err := c.call(ctx, "info", http.MethodGet, "/info", nil, nil, &w, jsonLimit); err != nil {
		return Info{}, err
	}
	info := w.Info
	info.Runtimes = slices.Sorted(maps.Keys(w.Runtimes))
	if w.Containerd != nil && c.version >= containerdVersion {
		info.ContainerdAddress = w.Containerd.Address
	}
	if w.FirewallBackend != nil && c.version >= firewallVersion {
		info.FirewallBackend = w.FirewallBackend.Driver
	}
	return info, nil
}

// CheckLimits fails with ErrKernelLimits naming each limit the kernel cannot
// enforce. Docker does not fail a create that sets one: it drops the setting
// and answers a warning, so a job would run unbounded.
func (i *Info) CheckLimits() error {
	var missing []string
	for _, l := range []struct {
		name string
		ok   bool
	}{{"MemoryLimit", i.MemoryLimit}, {"SwapLimit", i.SwapLimit}, {"PidsLimit", i.PidsLimit}, {"CpuCfsQuota", i.CPUCFSQuota}} {
		if !l.ok {
			missing = append(missing, l.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: /info reports false for %s", ErrKernelLimits, strings.Join(missing, ", "))
	}
	return nil
}

// SocketGID is the numeric group of the daemon's socket, for a job granted
// the socket. It refuses a path that is not a Unix socket.
func (c *Client) SocketGID() (uint32, error) {
	fi, err := os.Stat(c.socket)
	if err != nil {
		return 0, fmt.Errorf("docker socket: %w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if fi.Mode().Type() != os.ModeSocket || !ok {
		return 0, fmt.Errorf("docker socket: %s is not a Unix socket", c.socket)
	}
	return st.Gid, nil
}
