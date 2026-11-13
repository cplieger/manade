package config

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// DefaultIPv6Echo is the endpoint a host learns its IPv6 egress address from
// when its section names none.
const DefaultIPv6Echo = "https://cloudflare.com/cdn-cgi/trace"

// Protocol is an allow entry's transport.
type Protocol int

// The protocols; the zero Protocol is none of them.
const (
	ProtocolTCP Protocol = iota + 1
	ProtocolUDP
	ProtocolBoth
)

// Egress is a host's validated egress policy.
type Egress struct {
	// IPv6Echo is set only when IPv6 and Internet are both true.
	IPv6Echo string
	Allow    []Allow
	Block6   []netip.Prefix
	Internet bool
	Open     bool
	IPv6     bool
	// The keys the file wrote, which the rules refuse beside open or where
	// nothing is learned, whatever their value.
	internetSet, ipv6Set, ipv6EchoSet bool
}

// Allow is one private destination jobs may reach.
type Allow struct {
	To       netip.Prefix
	Ports    []PortRange
	Protocol Protocol
}

// PortRange is an inclusive port range; a single port has First == Last.
type PortRange struct {
	First, Last uint16
}

type fileEgress struct {
	Internet string      `yaml:"internet"`
	Open     string      `yaml:"open"`
	IPv6     string      `yaml:"ipv6"`
	IPv6Echo string      `yaml:"ipv6_echo"`
	Allow    []fileAllow `yaml:"allow"`
	Block6   []string    `yaml:"block6"`
}

type fileAllow struct {
	To       string   `yaml:"to"`
	Protocol string   `yaml:"protocol"`
	Ports    []string `yaml:"ports"`
}

func (fe *fileEgress) parse(path string, p *problems) Egress {
	at := func(key string) string { return path + "." + key }
	e := Egress{
		Internet:    field(p, at("internet"), fe.Internet, "true", parseBool),
		Open:        field(p, at("open"), fe.Open, "false", parseBool),
		IPv6:        field(p, at("ipv6"), fe.IPv6, "false", parseBool),
		internetSet: fe.Internet != "",
		ipv6Set:     fe.IPv6 != "",
		ipv6EchoSet: fe.IPv6Echo != "",
	}
	if echo := field(p, at("ipv6_echo"), fe.IPv6Echo, DefaultIPv6Echo, parseEcho); e.IPv6 && e.Internet {
		e.IPv6Echo = echo
	}
	for i := range fe.Allow {
		e.Allow = append(e.Allow, fe.Allow[i].parse(at("allow["+strconv.Itoa(i)+"]"), p))
	}
	for i, raw := range fe.Block6 {
		b, err := parsePrefix(raw)
		if err == nil && !b.Addr().Is6() {
			err = invalid("want an IPv6 prefix such as 2001:db8:1::/48", raw)
		}
		p.add(at("block6["+strconv.Itoa(i)+"]"), err)
		e.Block6 = append(e.Block6, b)
	}
	return e
}

func (fa *fileAllow) parse(path string, p *problems) Allow {
	a := Allow{Protocol: ProtocolTCP}
	if fa.To == "" {
		p.addf(path+".to", "required: an address or prefix")
	} else {
		a.To = field(p, path+".to", fa.To, "", parsePrefix)
	}
	switch fa.Protocol {
	case "", "tcp":
	case "udp":
		a.Protocol = ProtocolUDP
	case "both":
		a.Protocol = ProtocolBoth
	default:
		p.add(path+".protocol", invalid("want tcp, udp or both", fa.Protocol))
	}
	if len(fa.Ports) == 0 {
		p.addf(path+".ports", "required: list the ports, such as [5432] or [8000-8100]")
	}
	for i, raw := range fa.Ports {
		r, err := parsePortRange(raw)
		p.add(path+".ports["+strconv.Itoa(i)+"]", err)
		a.Ports = append(a.Ports, r)
	}
	return a
}

func parsePortRange(raw string) (PortRange, error) {
	port := func(s string) (uint16, bool) {
		n, err := strconv.ParseUint(s, 10, 16)
		return uint16(n), err == nil && n > 0
	}
	lo, hi, isRange := strings.Cut(raw, "-")
	first, ok1 := port(lo)
	last, ok2 := first, true
	if isRange {
		last, ok2 = port(hi)
	}
	if !ok1 || !ok2 || last < first {
		return PortRange{}, invalid("want a port from 1 to 65535 or a range such as 8000-8100", raw)
	}
	return PortRange{First: first, Last: last}, nil
}

func parseEcho(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeHTTPS || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", invalid("want an https:// URL", raw)
	}
	return raw, nil
}
