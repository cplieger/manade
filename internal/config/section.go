package config

import (
	"cmp"
	"encoding/json/v2"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// docSection is a host's section: the settings document's vocabulary
// holding only what that host applies.
type docSection struct {
	Hosts    map[string]docHost    `json:"hosts"`
	Profiles map[string]docProfile `json:"profiles"`
	Images   docImages             `json:"images"`
	Pool     struct {
		HostAddress string `json:"host_address"`
	} `json:"pool"`
}

// ParseSection validates settings, the section the control plane delivered
// to host self, with the parser every write path runs, against self's
// ceiling. A section names
// hosts.<self.Name> alone, the profiles that host serves with no host
// list, images.refresh and pool.host_address; capacity, a drain, a
// profile's host order and the pool's state are the pool's, never a host's. It answers the
// model holding that one host, each profile listing it, or FieldErrors.
func ParseSection(self *Self, settings []byte) (*Config, error) {
	var d docSection
	if err := json.Unmarshal(settings, &d, json.RejectUnknownMembers(true)); err != nil {
		return nil, FieldErrors{decodeError(err)}
	}
	var p problems
	cfg := &Config{Self: self, ceilings: map[string]*Ceiling{self.Name: &self.Ceiling}}
	refresh := parseRefresh(&p, "images.refresh", cmp.Or(string(d.Images.Refresh), "1h"))
	if h, ok := d.host(self.Name, refresh, &p); ok {
		cfg.Hosts = []Host{h}
	}
	cfg.Profiles = d.profiles(self.Name, &p)
	if a := d.Pool.HostAddress; a != "" {
		p.add("pool.host_address", checkDialAddress(a))
	}
	if len(cfg.Hosts) == 1 {
		cfg.check(&p)
	}
	if err := p.err(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// host parses the section's one host, name, refusing any other.
func (d *docSection) host(name string, refresh time.Duration, p *problems) (Host, bool) {
	for _, other := range slices.Sorted(maps.Keys(d.Hosts)) {
		if other != name {
			p.addf(hostPath(other), "a section holds only this host's settings, and this host is %s", quote(name))
		}
	}
	dh, ok := d.Hosts[name]
	if !ok {
		p.addf("hosts", "names no host %s, the MANADE_HOST of this host", quote(name))
		return Host{}, false
	}
	path := hostPath(name)
	if dh.Capacity != nil {
		p.addf(path+".capacity", "refused: capacity is the pool's setting, never part of a host's section")
	}
	if dh.Accepting != "" {
		p.addf(path+".accepting", "refused: a drain is the pool's setting, never part of a host's section")
	}
	return dh.parse(name, path, refresh, p), true
}

// profiles parses the section's profiles, each served by host, refusing
// what is the pool's: their host order and state.
func (d *docSection) profiles(host string, p *problems) []Profile {
	out := make([]Profile, 0, len(d.Profiles))
	for _, name := range slices.Sorted(maps.Keys(d.Profiles)) {
		dp := d.Profiles[name]
		at := profilePath(name) + "."
		if dp.Hosts != nil {
			p.addf(at+"hosts", "refused: the profile's hosts are the pool's; a section lists only the profiles this host serves")
		}
		if dp.Deleting != "" {
			p.addf(at+"deleting", "refused: the pool's state, never part of a host's section")
		}
		if dp.Retiring != nil {
			p.addf(at+"retiring", "refused: the pool's state, never part of a host's section")
		}
		dp.Hosts, dp.Deleting, dp.Retiring = nil, "", nil
		pr := dp.parse(name, p)
		pr.Hosts = []string{host}
		out = append(out, pr)
	}
	return out
}

// checkDialAddress refuses an address a host could not dial: host:port with
// a host and a port from 1 to 65535.
func checkDialAddress(raw string) error {
	host, port, err := net.SplitHostPort(raw)
	n, perr := strconv.ParseUint(port, 10, 16)
	if err != nil || perr != nil || n < 1 || host == "" || strings.ContainsAny(host, "/@") {
		return invalid("want host:port, a name or an address with a port", raw)
	}
	return nil
}
