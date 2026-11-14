package config

import (
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// defaultHost is the one host a config without hosts means.
const defaultHost = "default"

// hostNameRe is a lowercase DNS label. A host's name is its certificate's DNS
// name and the TLS server name, so an IP literal would make Go check IP SANs
// instead, and it builds the host's socket and TLS paths, which a / or ..
// would leave.
var hostNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const hostNameRule = "want a lowercase DNS label: letters, digits and dashes, at most 63, not starting or ending with a dash"

// reservedHostNames name the TLS directories manade pki writes beside the
// hosts' own, and "ca" also names the CA's file in every TLS_DIR.
var reservedHostNames = []string{"ca", "orchestrator"}

// CheckHostName refuses a name no hosts key may have.
func CheckHostName(name string) error {
	if !hostNameRe.MatchString(name) {
		return invalid(hostNameRule, name)
	}
	if slices.Contains(reservedHostNames, name) {
		return invalid("want a name other than ca and orchestrator, which name the pool's own TLS files", name)
	}
	return nil
}

// DefaultDNS are a host's resolvers when its section names none.
var DefaultDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}

// defaultSocket is the host API's socket path for the host named name.
func defaultSocket(name string) string { return "/run/manade/hosts/" + name + "/api.sock" }

// hostPath is the error path of the host named name, quoted when the name is
// not a valid one.
func hostPath(name string) string {
	if !hostNameRe.MatchString(name) {
		return "hosts." + quote(name)
	}
	return "hosts." + name
}

// apiPort is the host API's TCP port when an address names none.
const apiPort = "7464"

// hostTLSDir is a host's TLS_DIR when its environment names none.
const hostTLSDir = "/tls"

const schemeHTTPS = "https"

// Address is where a host API is reached or served: exactly one of Socket, a
// Unix socket path, and HostPort, a TCP address served with mutual TLS.
type Address struct {
	Socket   string
	HostPort string
}

// Host is one validated hosts entry.
type Host struct {
	Address Address
	// Subnet is the job network's IPv4 subnet; the zero Prefix lets the host
	// pick one.
	Subnet netip.Prefix
	Name   string
	// RegistryAuth is the path of a Docker client config inside the host's
	// container, or "".
	RegistryAuth string
	// DNS is never empty.
	DNS          []netip.Addr
	Egress       Egress
	Capacity     Capacity
	ImageRefresh time.Duration
}

// Capacity is a host's budget. Disk and MaxRunners are 0 when unset.
type Capacity struct {
	MilliCPUs  int64
	Memory     int64
	Disk       int64
	MaxRunners int
}

type fileHost struct {
	Capacity *fileCapacity `yaml:"capacity"`
	DNS      *[]string     `yaml:"dns"`
	Images   fileImages    `yaml:"images"`
	Network  struct {
		Subnet string `yaml:"subnet"`
	} `yaml:"network"`
	Address      string     `yaml:"address"`
	RegistryAuth string     `yaml:"registry_auth"`
	Egress       fileEgress `yaml:"egress"`
}

type fileCapacity struct {
	CPUs       string `yaml:"cpus"`
	Memory     string `yaml:"memory"`
	Disk       string `yaml:"disk"`
	MaxRunners string `yaml:"max_runners"`
}

type fileImages struct {
	Refresh string `yaml:"refresh"`
}

// parse parses the section at path; capPath is where its capacity sits,
// which for the one host of a config without hosts is the top level.
func (fh *fileHost) parse(name, path, capPath string, refresh time.Duration, p *problems) Host {
	at := func(key string) string { return path + "." + key }
	h := Host{Name: name, ImageRefresh: refresh}
	h.Address = field(p, at("address"), fh.Address, "unix://"+defaultSocket(name), parseAddress)
	h.Capacity = fh.Capacity.parse(capPath, p)
	if fh.Images.Refresh != "" {
		h.ImageRefresh = parseRefresh(p, at("images.refresh"), fh.Images.Refresh)
	}
	h.RegistryAuth = field(p, at("registry_auth"), fh.RegistryAuth, "", parseHostPath)
	h.Subnet = field(p, at("network.subnet"), fh.Network.Subnet, "", parseSubnet)
	h.Egress = fh.Egress.parse(at("egress"), p)
	h.DNS = DefaultDNS
	if fh.DNS != nil {
		h.DNS = parseDNS(p, at("dns"), *fh.DNS)
	}
	return h
}

func (fc *fileCapacity) parse(path string, p *problems) Capacity {
	if fc == nil {
		p.addf(path, "required: set cpus and memory")
		return Capacity{}
	}
	if fc.CPUs == "" {
		p.addf(path+".cpus", "required")
	}
	if fc.Memory == "" {
		p.addf(path+".memory", "required")
	}
	return Capacity{
		MilliCPUs:  field(p, path+".cpus", fc.CPUs, "", parseCPUs),
		Memory:     field(p, path+".memory", fc.Memory, "", parsePositiveSize),
		Disk:       field(p, path+".disk", fc.Disk, "", parsePositiveSize),
		MaxRunners: int(field(p, path+".max_runners", fc.MaxRunners, "", positive(10000))),
	}
}

// parseRefresh parses an idle image check interval, at least 10 minutes.
func parseRefresh(p *problems, path, raw string) time.Duration {
	d := field(p, path, raw, "", parseDuration)
	if d > 0 && d < 10*time.Minute {
		p.addf(path, "want at least 10m, got %s", quote(raw))
	}
	return d
}

func parseDNS(p *problems, path string, raws []string) []netip.Addr {
	if len(raws) == 0 {
		p.addf(path, "list at least one resolver, or leave dns out for 1.1.1.1 and 8.8.8.8")
	}
	out := make([]netip.Addr, 0, len(raws))
	for i, raw := range raws {
		a, err := netip.ParseAddr(raw)
		if err != nil || a.Zone() != "" || !a.IsGlobalUnicast() {
			p.add(path+"["+strconv.Itoa(i)+"]", invalid("want a unicast resolver address outside loopback and link-local", raw))
			continue
		}
		out = append(out, a.Unmap())
	}
	return out
}

// parseAddress parses a hosts entry's address: unix://<absolute path> or
// https://<host>[:port].
func parseAddress(raw string) (Address, error) {
	if path, ok := strings.CutPrefix(raw, "unix://"); ok {
		if _, err := parseHostPath(path); err != nil {
			return Address{}, err
		}
		return Address{Socket: path}, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeHTTPS || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Address{}, invalid("want unix:///path/api.sock or https://host:port", raw)
	}
	hp := u.Host
	if u.Port() == "" {
		hp = net.JoinHostPort(u.Hostname(), apiPort)
	}
	if hp, err = parseHostPort(hp, false); err != nil {
		return Address{}, err
	}
	return Address{HostPort: hp}, nil
}

// parseHostPort accepts host:port with a port from 1 to 65535; a listen
// address may leave the host empty.
func parseHostPort(raw string, listen bool) (string, error) {
	host, port, err := net.SplitHostPort(raw)
	n, perr := strconv.Atoi(port)
	if err != nil || perr != nil || n < 1 || n > 65535 || (host == "" && !listen) || strings.ContainsAny(host, "/@") {
		return "", invalid("want host:port", raw)
	}
	return raw, nil
}
