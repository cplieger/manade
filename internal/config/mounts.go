package config

import (
	"slices"
	"strconv"

	"github.com/cplieger/go/pathinside"
)

// fixedProtected are the paths in every host's container that hand a job
// the host's daemon, its channel to the control plane, the domain list
// its next start trusts, the supervisor's temporary files or the kernel's
// interfaces.
var fixedProtected = []string{
	"/run/docker.sock", "/var/run/docker.sock", "/run/docker", "/var/lib/docker",
	"/run/manade", "/tmp", "/proc", "/sys", "/dev",
}

// Protected answers the paths in host h's container that no job on a host
// that is not open may reach. The registry credentials file comes from the
// host's own environment, known only on the host itself, which checks its
// own section.
func (c *Config) Protected(h *Host) []string {
	out := slices.Clone(fixedProtected)
	if c.Self == nil || c.Self.Name != h.Name {
		return out
	}
	if f := c.Self.RegistryAuth.File; f != "" {
		out = append(out, f)
	}
	return out
}

// checkMounts refuses a bind source equal to, under or an ancestor of one
// of protected, compared as written.
func checkMounts(pr *Profile, h *Host, protected []string, at func(string) string, p *problems) {
	for i, m := range pr.Mounts {
		if !m.Bind || m.Source == "" {
			continue
		}
		for _, prot := range protected {
			if pathinside.Root(prot).Contains(m.Source) || pathinside.Root(m.Source).Contains(prot) {
				p.addf(at("mounts["+strconv.Itoa(i)+"].bind"), "%s reaches %s, which host %s protects; mount another path, or give the profile an open host",
					quote(m.Source), prot, quote(h.Name))
				break
			}
		}
	}
}

// checkNesting refuses, on each host that is not open, a bind source jobs
// may write that is a strict ancestor of another source on that host, in one
// profile or two: a job could rename or replace the directory another
// runner binds. Equal sources pass, and so does a writable source under
// another, since a job cannot rename its own mount's root.
func (c *Config) checkNesting(p *problems) {
	reported := map[[2]string]bool{}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if h.Egress.Open {
			continue
		}
		all := c.BindSources(h.Name)
		for k := range all {
			if !all[k].Mount.ReadOnly {
				checkAncestor(&all[k], all, h.Name, reported, p)
			}
		}
	}
}

// checkAncestor reports writable source a once per source of all it is a
// strict ancestor of.
func checkAncestor(a *BindSource, all []BindSource, host string, reported map[[2]string]bool, p *problems) {
	for _, b := range all {
		key := [2]string{a.at(), b.at()}
		if a.Mount.Source == b.Mount.Source || reported[key] || !pathinside.Root(a.Mount.Source).Contains(b.Mount.Source) {
			continue
		}
		reported[key] = true
		p.addf(a.at(), "%s is writable to jobs and an ancestor of %s, the source of %s, on host %s; mount it read-only, or mount the two apart",
			quote(a.Mount.Source), quote(b.Mount.Source), b.at(), quote(host))
	}
}

// BindSource is bind mount Index of profile Profile.
type BindSource struct {
	Profile string
	Mount   Mount
	Index   int
}

func (b *BindSource) at() string {
	return profilePath(b.Profile) + ".mounts[" + strconv.Itoa(b.Index) + "].bind"
}

// BindSources answers the bind mounts of every profile host serves, in the
// config's profile and mount order.
func (c *Config) BindSources(host string) []BindSource {
	var out []BindSource
	for i := range c.Profiles {
		pr := &c.Profiles[i]
		if !slices.Contains(pr.Hosts, host) {
			continue
		}
		for k, m := range pr.Mounts {
			if m.Bind && m.Source != "" {
				out = append(out, BindSource{Profile: pr.Name, Mount: m, Index: k})
			}
		}
	}
	return out
}
