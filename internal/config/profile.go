package config

import (
	"cmp"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

type docProfile struct {
	Capabilities             any        `json:"capabilities"`
	Scopes                   *[]string  `json:"scopes"`
	Hosts                    *[]string  `json:"hosts"`
	RunnerImage              string     `json:"runner_image"`
	Image                    string     `json:"image"`
	User                     string     `json:"user"`
	CPUs                     scalar     `json:"cpus"`
	Memory                   scalar     `json:"memory"`
	Swap                     scalar     `json:"swap"`
	PIDs                     scalar     `json:"pids"`
	Disk                     scalar     `json:"disk"`
	ShmSize                  scalar     `json:"shm_size"`
	MaxLifetime              scalar     `json:"max_lifetime"`
	IdleTimeout              scalar     `json:"idle_timeout"`
	MaxRunners               scalar     `json:"max_runners"`
	AllowPrivilegeEscalation scalar     `json:"allow_privilege_escalation"`
	AllowRoot                scalar     `json:"allow_root"`
	Docker                   string     `json:"docker"`
	Runtime                  string     `json:"runtime"`
	Privileged               scalar     `json:"privileged"`
	Deleting                 scalar     `json:"deleting"`
	Command                  []string   `json:"command"`
	Retiring                 []string   `json:"retiring"`
	Tmpfs                    []docTmpfs `json:"tmpfs"`
	Mounts                   []docMount `json:"mounts"`
}

type docTmpfs struct {
	Path string `json:"path"`
	Size scalar `json:"size"`
}

type docMount struct {
	Volume   string `json:"volume"`
	Bind     string `json:"bind"`
	Target   string `json:"target"`
	ReadOnly scalar `json:"read_only"`
}

// The vocabularies. A value outside one refuses the config.
const (
	dockerNone       = "none"
	DockerHostSocket = "host-socket"

	runtimeRunc = "runc"

	// ShippedDocker is the shipped image whose wrapper starts a daemon.
	ShippedDocker = "docker"
	// DockerWrapper is the shipped docker image's runner command.
	DockerWrapper = "/usr/local/bin/manade-dockerd-run"
	// runnerCommand is GitHub's runner image's start script.
	runnerCommand = "/home/runner/run.sh"
)

var (
	shippedImages = []string{"base", "build", "browser", ShippedDocker}
	dockerModes   = []string{dockerNone, DockerHostSocket}
	// runtimes holds only runc: runsc and sysbox-runc would need the runtime
	// inside the host image, which does not ship them.
	runtimes = []string{runtimeRunc}
)

// linuxCapabilities is capabilities(7)'s list without the CAP_ prefix.
var linuxCapabilities = []string{
	"AUDIT_CONTROL", "AUDIT_READ", "AUDIT_WRITE", "BLOCK_SUSPEND", "BPF",
	"CHECKPOINT_RESTORE", "CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER",
	"FSETID", "IPC_LOCK", "IPC_OWNER", "KILL", "LEASE", "LINUX_IMMUTABLE",
	"MAC_ADMIN", "MAC_OVERRIDE", "MKNOD", "NET_ADMIN", "NET_BIND_SERVICE",
	"NET_BROADCAST", "NET_RAW", "PERFMON", "SETFCAP", "SETGID", "SETPCAP",
	"SETUID", "SYSLOG", "SYS_ADMIN", "SYS_BOOT", "SYS_CHROOT", "SYS_MODULE",
	"SYS_NICE", "SYS_PACCT", "SYS_PTRACE", "SYS_RAWIO", "SYS_RESOURCE",
	"SYS_TIME", "SYS_TTY_CONFIG", "WAKE_ALARM",
}

// dockerDefaultCapabilities is Docker's default set, sorted, from
// https://docs.docker.com/engine/containers/run/#runtime-privilege-and-linux-capabilities.
var dockerDefaultCapabilities = []string{
	"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_BIND_SERVICE", "NET_RAW", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// parseMemory refuses a limit under 6 MiB, the least Docker accepts at create
// (linuxMinMemory in https://github.com/moby/moby/blob/docker-v29.0.0/daemon/daemon_unix.go).
func parseMemory(raw string) (int64, error) {
	n, err := parseSize(raw)
	if err == nil && n < 6<<20 {
		return 0, invalid("want at least 6MiB, the least Docker accepts", raw)
	}
	return n, err
}

var (
	// Profile names become scale set names, runs-on labels, Docker label
	// values and runner name prefixes: lowercase, because GitHub matches
	// labels without case, and short enough to leave a runner name room.
	profileNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	// imageRefRe is the Docker reference grammar (distribution/reference),
	// with sha256 the one digest algorithm.
	imageRefRe = regexp.MustCompile(`^(?:(?:(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*|\[[a-fA-F0-9:]+\])(?::[0-9]+)?/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[\w][\w.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
	// userRe admits uid:gid with neither 0; nine digits stay under the
	// kernel's 2^31-1 bound.
	userRe   = regexp.MustCompile(`^[1-9]\d{0,8}:[1-9]\d{0,8}$`)
	volumeRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{1,254}$`)
)

const profileNameRule = "want a name of 1 to 32 lowercase letters, digits, dots, underscores or dashes"

// defaultProfile is a scope's default profile when it names none.
const defaultProfile = "default"

// Profile is one validated runner shape. Sizes are bytes; 0 means unset.
type Profile struct {
	Name string
	// Exactly one of RunnerImage (a shipped image's name) and Image (an
	// operator's reference) is set.
	RunnerImage string
	Image       string
	// User is a numeric non-root "uid:gid", or "" for the image's own user.
	User    string
	Docker  string
	Runtime string
	Command []string
	// Capabilities is sorted; DockerDefaultCaps reports it came from
	// docker-default.
	Capabilities []string
	// Scopes holds scope keys as written; nil means every scope. A key
	// that names no configured scope serves nothing.
	Scopes []string
	// Hosts lists the hosts that may serve the profile, in order of
	// preference.
	Hosts []string
	// Retiring holds the scope keys the profile is leaving, and Deleting
	// marks a profile being deleted: the pool's state, which it clears as
	// each scope's scale set empties.
	Retiring                 []string
	Tmpfs                    []Tmpfs
	Mounts                   []Mount
	MilliCPUs                int64
	Memory                   int64
	Swap                     int64
	PIDs                     int64
	Disk                     int64
	ShmSize                  int64
	MaxLifetime              time.Duration
	IdleTimeout              time.Duration
	MaxRunners               int
	AllowPrivilegeEscalation bool
	DockerDefaultCaps        bool
	AllowRoot                bool
	Privileged               bool
	Deleting                 bool
}

// defaultMaxLifetime is a profile's max_lifetime when the document sets none.
const defaultMaxLifetime = 2 * time.Hour

// maxLifetimeCap is GitHub's limit on a job on a self-hosted runner
// (https://docs.github.com/en/actions/administering-github-actions/usage-limits-billing-and-administration), so the run-duration
// buckets hold every run.
const maxLifetimeCap = 5 * 24 * time.Hour

// Tmpfs is an in-memory mount, counted against the profile's memory.
type Tmpfs struct {
	Path string
	Size int64
}

// Mount is a named volume, or a host path when Bind is true.
type Mount struct {
	Source   string
	Target   string
	Bind     bool
	ReadOnly bool
}

// allowed reports whether the scope with key may use the profile. Keys match
// without case, as GitHub's owner and repository names do.
func (p *Profile) allowed(key string) bool {
	return p.Scopes == nil || slices.ContainsFunc(p.Scopes, func(s string) bool { return strings.EqualFold(s, key) })
}

// usable reports whether new jobs of the scope with key may be routed to the
// profile: allowed, not leaving the scope and not being deleted.
func (p *Profile) usable(key string) bool {
	return p.allowed(key) && !p.Deleting && !slices.ContainsFunc(p.Retiring, func(s string) bool { return strings.EqualFold(s, key) })
}

// Grants names each elevated field the profile sets away from its default,
// in the order of the elevated-access table.
func (p *Profile) Grants() []string {
	var g []string
	add := func(on bool, name string) {
		if on {
			g = append(g, name)
		}
	}
	add(p.AllowPrivilegeEscalation, "allow_privilege_escalation")
	add(len(p.Capabilities) > 0, "capabilities")
	add(p.AllowRoot, "allow_root")
	add(p.Docker != dockerNone, "docker")
	add(p.Privileged, "privileged")
	add(len(p.Mounts) > 0, "mounts")
	return g
}

// profilePath is the error path of the profile named name, quoted when the
// name is not a valid one.
func profilePath(name string) string {
	if !profileNameRe.MatchString(name) {
		return "profiles." + quote(name)
	}
	return "profiles." + name
}

func (d *document) profiles(p *problems) []Profile {
	if len(d.Profiles) == 0 {
		p.addf("profiles", "define at least one profile")
	}
	out := make([]Profile, 0, len(d.Profiles))
	for _, name := range slices.Sorted(maps.Keys(d.Profiles)) {
		dp := d.Profiles[name]
		out = append(out, dp.parse(name, p))
	}
	return out
}

func (fp *docProfile) parse(name string, p *problems) Profile {
	path := profilePath(name)
	if !profileNameRe.MatchString(name) {
		p.addf(path, profileNameRule)
	}
	at := func(key string) string { return path + "." + key }
	pr := Profile{Name: name}
	fp.image(&pr, at, p)
	pr.User = fp.User
	if fp.User != "" && !userRe.MatchString(fp.User) {
		p.add(at("user"), invalid("want a numeric non-root uid:gid such as 1001:1001", fp.User))
	}
	fp.resources(&pr, at, p)
	pr.Docker = oneOf(p, at("docker"), fp.Docker, dockerNone, dockerModes)
	pr.Runtime = oneOf(p, at("runtime"), fp.Runtime, runtimeRunc, runtimes)
	pr.AllowPrivilegeEscalation = field(p, at("allow_privilege_escalation"), fp.AllowPrivilegeEscalation, "", parseBool)
	pr.AllowRoot = field(p, at("allow_root"), fp.AllowRoot, "", parseBool)
	pr.Privileged = field(p, at("privileged"), fp.Privileged, "", parseBool)
	pr.Capabilities, pr.DockerDefaultCaps = capabilities(p, at("capabilities"), fp.Capabilities)
	fp.mounts(&pr, at, p)
	if fp.Scopes != nil {
		pr.Scopes = append([]string{}, *fp.Scopes...)
	}
	pr.Hosts = []string{defaultHost}
	if fp.Hosts != nil {
		pr.Hosts = fp.hosts(at("hosts"), p)
	}
	pr.Deleting = field(p, at("deleting"), fp.Deleting, "", parseBool)
	pr.Retiring = retiring(p, at("retiring"), fp.Retiring)
	ownRules(&pr, at, p)
	return pr
}

// retiring parses the scope keys a profile is leaving, none twice.
func retiring(p *problems, path string, keys []string) []string {
	var out []string
	for i, k := range keys {
		switch {
		case k == "":
			p.addf(path+"["+strconv.Itoa(i)+"]", "want a scope key")
		case slices.ContainsFunc(out, func(s string) bool { return strings.EqualFold(s, k) }):
			p.addf(path+"["+strconv.Itoa(i)+"]", "repeats scope %s", quote(k))
		default:
			out = append(out, k)
		}
	}
	return out
}

func (fp *docProfile) image(pr *Profile, at func(string) string, p *problems) {
	switch {
	case fp.Image != "" && fp.RunnerImage != "":
		p.addf(at("image"), "set image or runner_image, not both")
	case fp.Image != "":
		if len(fp.Image) > 512 || !imageRefRe.MatchString(fp.Image) {
			p.add(at("image"), invalid("want an image reference", fp.Image))
		} else if _, port := Registry(fp.Image); port == 0 {
			p.add(at("image"), invalid("want a registry port from 1 to 65535", fp.Image))
		}
		pr.Image = fp.Image
	default:
		pr.RunnerImage = oneOf(p, at("runner_image"), fp.RunnerImage, "base", shippedImages)
	}
	pr.Command = fp.Command
	if fp.Command == nil {
		pr.Command = []string{runnerCommand}
		if pr.RunnerImage == ShippedDocker {
			pr.Command = []string{DockerWrapper}
		}
		return
	}
	// The argv reaches Docker as a JSON list and no shell, so only the
	// program's path has a shape to hold.
	if len(fp.Command) == 0 {
		p.addf(at("command"), "want the runner's path and its arguments")
	} else if _, err := parseHostPath(fp.Command[0]); err != nil {
		p.add(at("command[0]"), err)
	}
}

func (fp *docProfile) resources(pr *Profile, at func(string) string, p *problems) {
	pr.MilliCPUs = field(p, at("cpus"), fp.CPUs, "2", parseCPUs)
	pr.Memory = field(p, at("memory"), fp.Memory, "4GiB", parseMemory)
	pr.Swap = field(p, at("swap"), fp.Swap, "0", parseSize)
	// Docker's MemorySwap is memory plus swap; a wrapped sum sends no swap cap.
	if pr.Swap > 0 && pr.Memory > math.MaxInt64-pr.Swap {
		p.addf(at("swap"), "memory plus swap is too large")
	}
	pr.PIDs = field(p, at("pids"), fp.PIDs, "4096", positive(1<<22))
	pr.Disk = field(p, at("disk"), fp.Disk, "", parsePositiveSize)
	pr.ShmSize = field(p, at("shm_size"), fp.ShmSize, "", parsePositiveSize)
	pr.MaxLifetime = field(p, at("max_lifetime"), fp.MaxLifetime, defaultMaxLifetime.String(), parseDuration)
	if pr.MaxLifetime > maxLifetimeCap {
		p.addf(at("max_lifetime"), "want at most 120h (5 days), GitHub's limit on a job on a self-hosted runner, got %s", quote(string(fp.MaxLifetime)))
	}
	pr.IdleTimeout = field(p, at("idle_timeout"), fp.IdleTimeout, "10m", parseDuration)
	pr.MaxRunners = int(field(p, at("max_runners"), fp.MaxRunners, "", positive(10000)))
}

func oneOf(p *problems, path, raw, def string, vocab []string) string {
	v := cmp.Or(raw, def)
	if !slices.Contains(vocab, v) {
		p.add(path, invalid("want one of "+strings.Join(vocab, ", "), raw))
	}
	return v
}

func capabilities(p *problems, path string, raw any) (caps []string, dockerDefault bool) {
	switch v := raw.(type) {
	case nil:
		return nil, false
	case string:
		if v == "docker-default" {
			return slices.Clone(dockerDefaultCapabilities), true
		}
	case []any:
		for i, item := range v {
			name, ok := item.(string)
			if !ok || !slices.Contains(linuxCapabilities, name) {
				p.addf(path+"["+strconv.Itoa(i)+"]", "want a Linux capability name such as NET_ADMIN, without CAP_")
			}
			caps = append(caps, name)
		}
		slices.Sort(caps)
		return slices.Compact(caps), false
	}
	p.addf(path, "want docker-default or a list of capability names")
	return nil, false
}

// mounts parses the mounts and tmpfs. Docker refuses a repeated or root
// target at create.
func (fp *docProfile) mounts(pr *Profile, at func(string) string, p *problems) {
	absPath := func(path, raw string) string {
		v, err := parseHostPath(raw)
		p.add(path, err)
		return v
	}
	for i, fm := range fp.Mounts {
		mp := at("mounts[" + strconv.Itoa(i) + "]")
		m := Mount{Target: absPath(mp+".target", fm.Target), ReadOnly: field(p, mp+".read_only", fm.ReadOnly, "", parseBool)}
		switch {
		case (fm.Volume == "") == (fm.Bind == ""):
			p.addf(mp, "set exactly one of volume or bind")
		case fm.Volume != "":
			if !volumeRe.MatchString(fm.Volume) {
				p.add(mp+".volume", invalid("want a volume name", fm.Volume))
			}
			m.Source = fm.Volume
		default:
			m.Source, m.Bind = absPath(mp+".bind", fm.Bind), true
		}
		pr.Mounts = append(pr.Mounts, m)
	}
	for i, ft := range fp.Tmpfs {
		tp := at("tmpfs[" + strconv.Itoa(i) + "]")
		t := Tmpfs{Path: absPath(tp+".path", ft.Path)}
		var err error
		t.Size, err = parsePositiveSize(string(ft.Size))
		p.add(tp+".size", err)
		pr.Tmpfs = append(pr.Tmpfs, t)
	}
}

// hosts parses the profile's host list, which names at least one host and
// none twice.
func (fp *docProfile) hosts(path string, p *problems) []string {
	if len(*fp.Hosts) == 0 {
		p.addf(path, "list at least one host, or leave hosts out for %s", defaultHost)
	}
	var out []string
	for i, name := range *fp.Hosts {
		at := path + "[" + strconv.Itoa(i) + "]"
		switch {
		case CheckHostName(name) != nil:
			p.add(at, CheckHostName(name))
		case slices.Contains(out, name):
			p.addf(at, "repeats host %s", quote(name))
		default:
			out = append(out, name)
		}
	}
	return out
}

// ownRules are the profile's own cross-field checks: the elevated-access table
// and the shipped docker image.
func ownRules(pr *Profile, at func(string) string, p *problems) {
	if g := pr.Grants(); len(g) > 0 && pr.Scopes == nil {
		p.addf(at("scopes"), "an elevated profile must list its scopes (it sets %s)", strings.Join(g, ", "))
	}
	// sudo cannot change its uid and gid with SETUID and SETGID dropped.
	if pr.AllowPrivilegeEscalation && !pr.Privileged &&
		(!slices.Contains(pr.Capabilities, "SETUID") || !slices.Contains(pr.Capabilities, "SETGID")) {
		p.addf(at("allow_privilege_escalation"), "needs capabilities holding SETUID and SETGID, or privileged: true")
	}
	if pr.RunnerImage != ShippedDocker {
		return
	}
	if !pr.AllowPrivilegeEscalation {
		p.addf(at("allow_privilege_escalation"), "runner_image: docker needs allow_privilege_escalation: true, because dockerd starts through sudo")
	}
	if !pr.Privileged {
		p.addf(at("privileged"), "runner_image: docker needs privileged: true, because a job's own daemon needs it, so it runs only on an open host")
	}
	if pr.Docker == DockerHostSocket {
		p.addf(at("docker"), "runner_image: docker cannot use host-socket: two daemons would claim one socket path")
	}
}
