package config

import (
	"slices"
	"strconv"
	"strings"
)

// Daemon is what the Docker daemon reports about itself, from /info.
type Daemon struct {
	// DataRoot is DockerRootDir.
	DataRoot string
	// ContainerdAddress is Containerd.Address, "" below API 1.46.
	ContainerdAddress string
	// Driver is the storage driver; DriverStatus its key and value pairs.
	Driver       string
	DriverStatus [][2]string
	Runtimes     []string
}

// containerdSnapshotter marks the containerd image store in DriverStatus's
// driver-type, which accepts StorageOpt size and silently ignores it.
const containerdSnapshotter = "io.containerd.snapshotter.v1"

// fixedProtected are protected bind sources /info never reports: containerd's
// default root and state directory, Docker's default exec root (which holds
// the socket of a containerd Docker runs itself), and the kernel's views.
var fixedProtected = []string{"/var/lib/containerd", "/run/containerd", "/run/docker", "/proc", "/sys"}

// protectedPaths lists each protected path in its /run and /var/run forms,
// because /var/run is a symlink to /run on current distributions.
func protectedPaths(extra ...string) []string {
	var out []string
	for _, p := range append(slices.Clone(fixedProtected), extra...) {
		if p == "" {
			continue
		}
		out = append(out, p)
		if rest, ok := strings.CutPrefix(p, "/var/run"); ok && (rest == "" || rest[0] == '/') {
			out = append(out, "/run"+rest)
		} else if rest, ok := strings.CutPrefix(p, "/run"); ok && (rest == "" || rest[0] == '/') {
			out = append(out, "/var/run"+rest)
		}
	}
	return out
}

// checkBind refuses a canonical bind source that equals a protected path, lies
// under one, or is an ancestor of one. The check is on the path as written:
// the pool cannot see the host's symlinks.
func checkBind(p *problems, path, src string, protected []string) {
	ancestor := strings.TrimSuffix(src, "/") + "/"
	for _, q := range protected {
		if src == q || strings.HasPrefix(src, q+"/") || strings.HasPrefix(q, ancestor) {
			p.addf(path, "%s would reach %s, which only docker: host-socket may grant", quote(src), q)
			return
		}
	}
}

// CheckDaemon runs the checks that need the daemon's facts: each profile's
// runtime exists, disk is set only on a driver that enforces it, and no bind
// source reaches DOCKER_SOCKET, the data root, the containerd socket the
// daemon reports or a fixed protected path. It joins every failure into one
// error, like Load, and must pass before any profile is served. A disk the
// daemon still refuses at create is the caller's probe to find.
func (c *Config) CheckDaemon(d *Daemon) error {
	var p problems
	protected := protectedPaths(c.DockerSocket, d.DataRoot, d.ContainerdAddress)
	diskRefusal := diskDriverRefusal(d)
	for i := range c.Profiles {
		pr := &c.Profiles[i]
		at := "profiles." + pr.Name
		if !slices.Contains(d.Runtimes, pr.Runtime) {
			p.addf(at+".runtime", "the Docker daemon has no runtime %s", quote(pr.Runtime))
		}
		if pr.Disk > 0 && diskRefusal != "" {
			p.addf(at+".disk", "%s", diskRefusal)
		}
		for j, m := range pr.Mounts {
			if m.Bind {
				checkBind(&p, at+".mounts["+strconv.Itoa(j)+"].bind", m.Source, protected)
			}
		}
	}
	return p.err()
}

// diskDriverRefusal names why the daemon cannot enforce a writable-layer size,
// or answers "". Only btrfs, zfs, and overlay2 on xfs enforce it; xfs also
// needs pquota, which /info does not show, so the caller probes for it.
func diskDriverRefusal(d *Daemon) string {
	status := map[string]string{}
	for _, kv := range d.DriverStatus {
		status[kv[0]] = kv[1]
	}
	switch {
	case status["driver-type"] == containerdSnapshotter:
		return "the containerd image store ignores a disk limit"
	case d.Driver == "btrfs", d.Driver == "zfs":
		return ""
	case d.Driver == "overlay2" && status["Backing Filesystem"] == "xfs":
		return ""
	}
	return "storage driver " + quote(d.Driver) + " does not enforce a disk limit; only btrfs, zfs, or overlay2 on xfs with pquota do"
}
