package config

import (
	"net/netip"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/go/pathinside"
)

// ipv6AllowRanges are the IPv6 destinations an allow entry may name: unique
// local and global unicast.
var ipv6AllowRanges = []netip.Prefix{netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("2000::/3")}

// dockerHubNames are the registry names Docker reads as Docker Hub.
var dockerHubNames = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// registryPort is a registry's port when an image reference names none.
const registryPort = 443

// check runs the rules that join a host's section to the profiles it serves
// and to the environment. Every field is parsed by then.
func (c *Config) check(p *problems) {
	for _, s := range c.secretPaths() {
		if c.underConfig(s.path) {
			p.addf(s.key, "%s lies at or under the config file's directory %s, which every host on this machine mounts; give %s",
				quote(s.path), quote(c.configDir()), s.remedy)
		}
	}
	for i := range c.Hosts {
		checkHost(&c.Hosts[i], p)
	}
	for i := range c.Profiles {
		c.checkProfile(&c.Profiles[i], p)
	}
	for i := range c.Scopes {
		c.checkDefault(&c.Scopes[i], p)
	}
	c.checkNesting(p)
}

// checkDefault refuses a served scope whose default profile its new jobs
// could not be routed to: the runner variable would name a dead label.
func (c *Config) checkDefault(s *Scope, p *problems) {
	if s.Stopping {
		return
	}
	at := s.path + ".default_profile"
	pr, ok := c.Profile(s.DefaultProfile)
	switch {
	case !ok:
		p.addf(at, "names no profile %s; define it or choose another", quote(s.DefaultProfile))
		return
	case pr.usable(s.Key):
		return
	case pr.Deleting:
		p.addf(at, "%s is being deleted; choose another default profile", quote(pr.Name))
		return
	}
	if slices.ContainsFunc(c.Profiles, func(o Profile) bool { return o.usable(s.Key) }) {
		p.addf(at, "%s is this scope's default profile but %s may not use it; choose another", quote(pr.Name), quote(s.Key))
		return
	}
	p.addf(s.path, "%s is the only profile %s may use: turn the scope off, or let it use another profile", quote(pr.Name), quote(s.Key))
}

// checkDeleting refuses a profile prev holds as being deleted that c holds
// as an ordinary one: only the pool ends a deletion, by removing the
// profile once its queue is empty.
func (c *Config) checkDeleting(prev *Config, p *problems) {
	for i := range prev.Profiles {
		old := &prev.Profiles[i]
		if pr, ok := c.Profile(old.Name); ok && old.Deleting && !pr.Deleting {
			p.addf(profilePath(old.Name), "%s is still being deleted; choose another name, or wait until it is gone", quote(old.Name))
		}
	}
}

func (c *Config) configDir() string { return path.Dir(c.File) }

// secretPath is a path of this role's own that holds a secret, and what its
// refusal asks the operator to give.
type secretPath struct{ key, path, remedy string }

// secretPaths answers a host's own secret paths from its environment.
func (c *Config) secretPaths() []secretPath {
	if c.Self == nil {
		return nil
	}
	out := []secretPath{{"TLS_DIR", c.Self.TLSDir, "the TLS files a mount of their own"}}
	if f := c.Self.RegistryAuth.File; f != "" {
		out = append(out, secretPath{"MANADE_REGISTRY_AUTH_FILE", f, "the file a mount of its own"})
	}
	return out
}

// underConfig reports whether p, a path of this host's own container unless
// empty, lies at or under the settings document's directory, compared as
// written and with its and the directory's symlinks resolved; a path that
// does not exist yet is compared as written.
func (c *Config) underConfig(p string) bool {
	if p == "" {
		return false
	}
	dirs, paths := withResolved(c.configDir()), withResolved(p)
	for _, d := range dirs {
		if slices.ContainsFunc(paths, pathinside.Root(d).Contains) {
			return true
		}
	}
	return false
}

// withResolved answers p and, when it exists, p with its symlinks resolved.
func withResolved(p string) []string {
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		return []string{p, r}
	}
	return []string{p}
}

func checkHost(h *Host, p *problems) {
	at := func(key string) string { return hostPath(h.Name) + "." + key }
	checkEgress(&h.Egress, at, p)
	checkSubnet(h, at, p)
	if !h.Egress.IPv6 && !h.Egress.Open {
		for _, r := range h.DNS {
			if r.Is6() {
				p.addf(at("dns"), "%s is IPv6, which needs egress.ipv6: true: without it the host turns IPv6 off in its own network", r)
			}
		}
	}
}

func checkEgress(e *Egress, at func(string) string, p *problems) {
	if e.Open {
		beside := []struct {
			key string
			set bool
		}{{"internet", e.internetSet}, {"allow", len(e.Allow) > 0}, {"ipv6", e.ipv6Set}, {"block6", len(e.Block6) > 0}}
		for _, b := range beside {
			if b.set {
				p.addf(at("egress."+b.key), "refused beside egress.open: true, which writes no egress rule at all")
			}
		}
	}
	if !e.IPv6 && len(e.Block6) > 0 {
		p.addf(at("egress.block6"), "needs egress.ipv6: true")
	}
	for i, a := range e.Allow {
		checkAllow(a.To, e.IPv6, at("egress.allow["+strconv.Itoa(i)+"].to"), p)
	}
}

// checkSubnet refuses a configured job subnet that would capture the host's
// own traffic to a resolver or an allowed destination.
func checkSubnet(h *Host, at func(string) string, p *problems) {
	s := h.Subnet
	if !s.IsValid() {
		return
	}
	for _, r := range h.DNS {
		if s.Contains(r) {
			p.addf(at("network.subnet"), "%s contains the resolver %s of %s: the job network would capture the host's own lookups", s, r, at("dns"))
		}
	}
	for i, a := range h.Egress.Allow {
		if a.To.IsValid() && s.Overlaps(a.To) {
			p.addf(at("network.subnet"), "%s overlaps %s of %s: jobs could not reach that destination", s, show(a.To), at("egress.allow["+strconv.Itoa(i)+"]"))
		}
	}
}

func checkAllow(to netip.Prefix, ipv6 bool, at string, p *problems) {
	switch {
	case !to.IsValid():
	case to.Addr().Is4():
		if !within(to, PrivateRanges) {
			p.addf(at, "%s is not private: allow names destinations inside %s, and egress.internet governs public ones", show(to), prefixList(PrivateRanges))
		}
	case !ipv6:
		p.addf(at, "%s is IPv6, which needs egress.ipv6: true", show(to))
	case !within(to, ipv6AllowRanges):
		p.addf(at, "%s lies outside %s", show(to), prefixList(ipv6AllowRanges))
	}
}

func (c *Config) checkProfile(pr *Profile, p *problems) {
	at := func(key string) string { return profilePath(pr.Name) + "." + key }
	for _, name := range pr.Hosts {
		h, ok := c.Host(name)
		if !ok {
			p.addf(at("hosts"), "names no host %s; define it under hosts or list another", quote(name))
			continue
		}
		if why := h.elevated(); why != "" && pr.Scopes == nil {
			p.addf(at("scopes"), "required: host %s %s, so every profile it serves lists its scopes", quote(name), why)
		}
		checkFits(pr, h, at, p)
		if h.Egress.Open {
			continue
		}
		checkConfined(pr, h, at, p)
		checkMounts(pr, h, c.Protected(h), at, p)
		if !h.Egress.Internet {
			checkRegistry(pr, h, at, p)
		}
	}
}

// checkFits refuses a profile one runner of which is larger than host h's
// budget: its jobs could never be placed there. A host with no budget set
// refuses nothing.
func checkFits(pr *Profile, h *Host, at func(string) string, p *problems) {
	c := h.Capacity
	refuse := func(key string) {
		p.addf(at(key), "never fits hosts.%s.capacity.%s; raise it or list another host", h.Name, key)
	}
	if c.MilliCPUs > 0 && pr.MilliCPUs > c.MilliCPUs {
		refuse("cpus")
	}
	if c.Memory > 0 && pr.Memory > c.Memory {
		refuse("memory")
	}
	if c.Disk > 0 && pr.Disk > c.Disk {
		refuse("disk")
	}
}

// elevated answers why jobs on h reach more than the internet, or "".
func (h *Host) elevated() string {
	switch {
	case h.Egress.Open:
		return "is open"
	case len(h.Egress.Allow) > 0:
		return "has allow entries"
	}
	return ""
}

// checkConfined refuses on a host that is not open what lets a job change
// the host's network stack or leave its container.
func checkConfined(pr *Profile, h *Host, at func(string) string, p *problems) {
	refuse := func(key, what string) {
		p.addf(at(key), "%s is refused on host %s, whose egress is not open; give the profile an open host", what, quote(h.Name))
	}
	if pr.Privileged {
		refuse("privileged", "privileged: true")
	}
	if pr.Docker == DockerHostSocket {
		refuse("docker", "docker: host-socket")
	}
	extra := slices.DeleteFunc(slices.Clone(pr.Capabilities), func(c string) bool {
		return c == "NET_ADMIN" || slices.Contains(dockerDefaultCapabilities, c)
	})
	if len(extra) > 0 {
		refuse("capabilities", strings.Join(extra, ", ")+", outside Docker's default set and NET_ADMIN,")
	}
}

// checkRegistry refuses an image an internet: false host could not pull:
// one on Docker Hub, or on an IP address no allow entry covers. A registry
// given by name is the host's to resolve and check.
func checkRegistry(pr *Profile, h *Host, at func(string) string, p *problems) {
	if pr.Image == "" {
		return
	}
	name, port := Registry(pr.Image)
	addr, err := netip.ParseAddr(name)
	addr = addr.Unmap()
	switch {
	case name == "":
		p.addf(at("image"), "%s is on Docker Hub, which host %s cannot reach with egress.internet: false; use an image on ghcr.io or on a registry its allow entries cover",
			quote(pr.Image), quote(h.Name))
	case err == nil && !h.Egress.AllowsTCP(addr, port):
		p.addf(at("image"), "%s is on the registry %s, which no allow entry of host %s covers on TCP port %d, and the host has egress.internet: false",
			quote(pr.Image), addr, quote(h.Name), port)
	}
}

// Registry answers an image reference's registry name and port, with ""
// for Docker Hub, an IPv6 host without its brackets and port 0 for one out
// of range. The first component is a registry only when it holds a dot, a
// colon or an upper-case letter, or is localhost, as in splitDockerDomain of
// https://github.com/distribution/reference/blob/main/normalize.go.
func Registry(ref string) (name string, port uint16) {
	first, _, ok := strings.Cut(ref, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost" && strings.ToLower(first) == first) {
		return "", registryPort
	}
	name, rawPort, hasPort := strings.Cut(first, ":")
	if rest, ok := strings.CutPrefix(first, "["); ok {
		name, rest, _ = strings.Cut(rest, "]")
		rawPort, hasPort = strings.CutPrefix(rest, ":")
	}
	port = registryPort
	if hasPort {
		n, err := strconv.ParseUint(rawPort, 10, 16)
		if err != nil {
			return name, 0
		}
		port = uint16(n)
	}
	if slices.Contains(dockerHubNames, strings.ToLower(name)) {
		return "", registryPort
	}
	return name, port
}

// show prints a single-address prefix as its address, as the document wrote
// it.
func show(pfx netip.Prefix) string {
	if pfx.IsSingleIP() {
		return pfx.Addr().String()
	}
	return pfx.String()
}

func prefixList(ps []netip.Prefix) string {
	s := make([]string, len(ps))
	for i, pfx := range ps {
		s[i] = pfx.String()
	}
	return strings.Join(s[:len(s)-1], ", ") + " or " + s[len(s)-1]
}
