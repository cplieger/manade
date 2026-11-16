package config

import (
	"encoding/json/jsontext"
	"net"
	"net/netip"
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

// hostTLSDir is a host's TLS_DIR when its environment names none.
const hostTLSDir = "/tls"

const schemeHTTPS = "https"

// Address is where a host API is served: exactly one of Socket, a Unix
// socket path, and HostPort, a TCP address served with mutual TLS.
type Address struct {
	Socket   string
	HostPort string
	// Port is HostPort's port, 0 for a socket.
	Port uint16
}

// Host is one validated hosts entry.
type Host struct {
	// Subnet is the job network's IPv4 subnet; the zero Prefix lets the host
	// pick one.
	Subnet netip.Prefix
	Name   string
	// DNS is never empty.
	DNS          []netip.Addr
	Egress       Egress
	Capacity     Capacity
	ImageRefresh time.Duration
}

// Capacity is a host's budget, all zero when its section sets none; Disk
// and MaxRunners are 0 when unset.
type Capacity struct {
	MilliCPUs  int64
	Memory     int64
	Disk       int64
	MaxRunners int
}

type docHost struct {
	Capacity *docCapacity `json:"capacity"`
	DNS      *[]string    `json:"dns"`
	Images   docImages    `json:"images"`
	Network  struct {
		Subnet scalar `json:"subnet"`
	} `json:"network"`
	Egress docEgress `json:"egress"`
	// Refused by name, so the refusal names what replaces each.
	Address      jsontext.Value `json:"address"`
	RegistryAuth jsontext.Value `json:"registry_auth"`
}

type docCapacity struct {
	CPUs       scalar `json:"cpus"`
	Memory     scalar `json:"memory"`
	Disk       scalar `json:"disk"`
	MaxRunners scalar `json:"max_runners"`
}

type docImages struct {
	Refresh scalar `json:"refresh"`
}

// parse parses the section at path.
func (dh *docHost) parse(name, path string, refresh time.Duration, p *problems) Host {
	at := func(key string) string { return path + "." + key }
	h := Host{Name: name, ImageRefresh: refresh}
	if dh.Address != nil {
		p.addf(at("address"), "refused: hosts dial the control plane, so a host has no address to set")
	}
	if dh.RegistryAuth != nil {
		p.addf(at("registry_auth"), "refused: set MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE on the host's machine instead")
	}
	h.Capacity = dh.Capacity.parse(at("capacity"), p)
	if dh.Images.Refresh != "" {
		h.ImageRefresh = parseRefresh(p, at("images.refresh"), string(dh.Images.Refresh))
	}
	h.Subnet = field(p, at("network.subnet"), dh.Network.Subnet, "", parseSubnet)
	h.Egress = dh.Egress.parse(at("egress"), p)
	h.DNS = DefaultDNS
	if dh.DNS != nil {
		h.DNS = parseDNS(p, at("dns"), *dh.DNS)
	}
	return h
}

func (dc *docCapacity) parse(path string, p *problems) Capacity {
	if dc == nil {
		return Capacity{}
	}
	if dc.CPUs == "" {
		p.addf(path+".cpus", "required")
	}
	if dc.Memory == "" {
		p.addf(path+".memory", "required")
	}
	return Capacity{
		MilliCPUs:  field(p, path+".cpus", dc.CPUs, "", parseCPUs),
		Memory:     field(p, path+".memory", dc.Memory, "", parsePositiveSize),
		Disk:       field(p, path+".disk", dc.Disk, "", parsePositiveSize),
		MaxRunners: int(field(p, path+".max_runners", dc.MaxRunners, "", positive(10000))),
	}
}

// parseRefresh parses an idle image check interval, at least 10 minutes.
func parseRefresh(p *problems, path, raw string) time.Duration {
	d := field(p, path, scalar(raw), "", parseDuration)
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

// parseHostPort accepts a listen address, host:port with a port from 1 to
// 65535 and the host possibly empty.
func parseHostPort(raw string) (Address, error) {
	host, port, err := net.SplitHostPort(raw)
	n, perr := strconv.ParseUint(port, 10, 16)
	if err != nil || perr != nil || n < 1 || strings.ContainsAny(host, "/@") {
		return Address{}, invalid("want host:port", raw)
	}
	return Address{HostPort: raw, Port: uint16(n)}, nil
}
