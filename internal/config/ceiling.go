package config

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/cplieger/go/envx"
)

// Ceiling is what a host's own environment lets a delivered section set,
// which no section exceeds. The zero Ceiling is the shipped default: no
// open egress, the internet allowed, no IPv6 and no private destination.
type Ceiling struct {
	// Private is MANADE_ALLOW_PRIVATE: the private IPv4 and the IPv6
	// prefixes an allow entry or a resolver may lie inside.
	Private []netip.Prefix
	// Block6 is MANADE_BLOCK6: IPv6 prefixes dropped ahead of every accept
	// a section controls.
	Block6 []netip.Prefix
	// Warnings name each value read as its narrower meaning.
	Warnings []string
	// Open is MANADE_ALLOW_OPEN.
	Open bool
	// NoInternet is MANADE_ALLOW_INTERNET=false, which pins
	// egress.internet to false.
	NoInternet bool
	// IPv6 is MANADE_ALLOW_IPV6.
	IPv6 bool
}

// readCeiling reads the ceiling's variables. A toggle that is neither true
// nor false, and a malformed MANADE_ALLOW_PRIVATE entry, read as the
// narrower reach with a warning; a malformed MANADE_BLOCK6 entry is an
// error, since skipping it would widen reach.
func readCeiling() (Ceiling, error) {
	var c Ceiling
	c.Open = c.toggle("MANADE_ALLOW_OPEN", false)
	c.NoInternet = !c.toggle("MANADE_ALLOW_INTERNET", true)
	c.IPv6 = c.toggle("MANADE_ALLOW_IPV6", false)
	for _, raw := range listEnv("MANADE_ALLOW_PRIVATE") {
		pfx, err := parsePrefix(raw)
		if err != nil {
			c.Warnings = append(c.Warnings, fmt.Sprintf("MANADE_ALLOW_PRIVATE: skipped %s, which is not an address or a masked prefix", quote(raw)))
			continue
		}
		c.Private = append(c.Private, pfx)
	}
	var errs []error
	for _, raw := range listEnv("MANADE_BLOCK6") {
		pfx, err := parsePrefix(raw)
		if err == nil && !pfx.Addr().Is6() {
			err = invalid("want an IPv6 prefix such as 2001:db8:1::/48", raw)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("MANADE_BLOCK6: %w", err))
			continue
		}
		c.Block6 = append(c.Block6, pfx)
	}
	return c, errors.Join(errs...)
}

// toggle reads key, which only true or false sets; unset is def, and any
// other value reads false, the narrower reach of every ceiling toggle.
func (c *Ceiling) toggle(key envx.Key, def bool) bool {
	switch raw := envx.String(key); raw {
	case "":
		return def
	case "true":
		return true
	case "false":
		return false
	default:
		c.Warnings = append(c.Warnings, fmt.Sprintf("%s: %s is neither true nor false, so it reads false", key, quote(raw)))
		return false
	}
}

// listEnv answers key's comma-separated entries.
func listEnv(key envx.Key) []string {
	var out []string
	for entry := range strings.SplitSeq(envx.String(key), ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// globalUnicast6 is the IPv6 space a host pinned off the internet opens no
// resolver in.
var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// checkCeiling refuses the egress keys and resolvers of host h its ceiling
// c does not let it set; checkAllow bounds its allow entries.
func checkCeiling(h *Host, c *Ceiling, at func(string) string, p *problems) {
	e := &h.Egress
	name := quote(h.Name)
	if e.Open && !c.Open {
		p.addf(at("egress.open"), "refused: host %s's machine does not set MANADE_ALLOW_OPEN=true", name)
	}
	if e.Internet && c.NoInternet {
		key := "egress.internet"
		if e.Open {
			key = "egress.open"
		}
		p.addf(at(key), "refused: MANADE_ALLOW_INTERNET=false on host %s's machine pins egress.internet to false", name)
	}
	if e.IPv6 && !c.IPv6 {
		p.addf(at("egress.ipv6"), "refused: host %s's machine does not set MANADE_ALLOW_IPV6=true", name)
	}
	if e.Open {
		return
	}
	for i, r := range h.DNS {
		c.checkResolver(r, name, at("dns["+strconv.Itoa(i)+"]"), p)
	}
}

// checkAllow refuses an allow entry the ceiling does not reach, one the
// model's checkAllow accepts: a private IPv4 or an IPv6 prefix.
func (c *Ceiling) checkAllow(to netip.Prefix, host, at string, p *problems) {
	switch {
	case to.Addr().Is6() && within(to, c.Block6):
		p.addf(at, "%s lies wholly inside MANADE_BLOCK6 on host %s's machine, which drops it ahead of every allow entry", show(to), host)
	case !within(to, c.Private):
		p.addf(at, "%s lies outside %s; list it there on host %s's machine", show(to), c.privateBound(), host)
	}
}

// checkResolver refuses a resolver the ceiling does not reach. An open
// host's ceiling bounds no resolver, since it loads no egress rule.
func (c *Ceiling) checkResolver(r netip.Addr, host, at string, p *problems) {
	one := netip.PrefixFrom(r, r.BitLen())
	public := (r.Is4() && !within(one, PrivateRanges)) || globalUnicast6.Contains(r)
	switch {
	case r.Is6() && within(one, c.Block6):
		p.addf(at, "%s lies inside MANADE_BLOCK6 on host %s's machine, which drops it ahead of every resolver", r, host)
	case public && c.NoInternet && !slices.Contains(DefaultDNS, r):
		p.addf(at, "%s is a public resolver, and MANADE_ALLOW_INTERNET=false on host %s's machine opens none but %s; use a resolver on the LAN at a private IPv4 or unique-local IPv6 address listed in MANADE_ALLOW_PRIVATE",
			r, host, addrList(DefaultDNS))
	case (r.Is6() || !public) && !within(one, c.Private):
		p.addf(at, "%s lies outside %s; list it there on host %s's machine", r, c.privateBound(), host)
	}
}

// privateBound names MANADE_ALLOW_PRIVATE as the host's machine sets it.
func (c *Ceiling) privateBound() string {
	if len(c.Private) == 0 {
		return "MANADE_ALLOW_PRIVATE, which is empty"
	}
	s := make([]string, len(c.Private))
	for i, pfx := range c.Private {
		s[i] = pfx.String()
	}
	return "MANADE_ALLOW_PRIVATE (" + strings.Join(s, ", ") + ")"
}

func addrList(as []netip.Addr) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, " and ")
}
