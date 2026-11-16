package config

import (
	"encoding/json/jsontext"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

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
	Allow    []Allow
	Block6   []netip.Prefix
	Internet bool
	// Open holds only with Internet true, IPv6 false and no Allow or
	// Block6: checkEgress refuses those keys beside it.
	Open bool
	IPv6 bool
	// The keys the document wrote, which the rules refuse beside open
	// whatever their value.
	internetSet, ipv6Set bool
}

// Allow is one private destination jobs may reach.
type Allow struct {
	To       netip.Prefix
	Ports    []PortRange
	Protocol Protocol
}

// AllowsTCP reports whether an allow entry passes TCP to addr on port.
func (e *Egress) AllowsTCP(addr netip.Addr, port uint16) bool {
	return slices.ContainsFunc(e.Allow, func(a Allow) bool {
		return a.Protocol != ProtocolUDP && a.To.Contains(addr) &&
			slices.ContainsFunc(a.Ports, func(r PortRange) bool { return r.First <= port && port <= r.Last })
	})
}

// PortRange is an inclusive port range; a single port has First == Last.
type PortRange struct {
	First, Last uint16
}

type docEgress struct {
	Internet scalar     `json:"internet"`
	Open     scalar     `json:"open"`
	IPv6     scalar     `json:"ipv6"`
	Allow    []docAllow `json:"allow"`
	Block6   []string   `json:"block6"`
	// IPv6Echo moved to the host's environment; it is refused by name.
	IPv6Echo jsontext.Value `json:"ipv6_echo"`
}

type docAllow struct {
	To       string   `json:"to"`
	Protocol string   `json:"protocol"`
	Ports    []scalar `json:"ports"`
}

func (de *docEgress) parse(path string, p *problems) Egress {
	at := func(key string) string { return path + "." + key }
	e := Egress{
		Internet:    field(p, at("internet"), de.Internet, "true", parseBool),
		Open:        field(p, at("open"), de.Open, "false", parseBool),
		IPv6:        field(p, at("ipv6"), de.IPv6, "false", parseBool),
		internetSet: de.Internet != "",
		ipv6Set:     de.IPv6 != "",
	}
	if de.IPv6Echo != nil {
		p.addf(at("ipv6_echo"), "refused: set MANADE_IPV6_ECHO on the host's machine instead")
	}
	for i := range de.Allow {
		e.Allow = append(e.Allow, de.Allow[i].parse(at("allow["+strconv.Itoa(i)+"]"), p))
	}
	for i, raw := range de.Block6 {
		b, err := parsePrefix(raw)
		if err == nil && !b.Addr().Is6() {
			err = invalid("want an IPv6 prefix such as 2001:db8:1::/48", raw)
		}
		p.add(at("block6["+strconv.Itoa(i)+"]"), err)
		e.Block6 = append(e.Block6, b)
	}
	return e
}

func (da *docAllow) parse(path string, p *problems) Allow {
	a := Allow{Protocol: ProtocolTCP}
	if da.To == "" {
		p.addf(path+".to", "required: an address or prefix")
	} else {
		a.To = field(p, path+".to", scalar(da.To), "", parsePrefix)
	}
	switch da.Protocol {
	case "", "tcp":
	case "udp":
		a.Protocol = ProtocolUDP
	case "both":
		a.Protocol = ProtocolBoth
	default:
		p.add(path+".protocol", invalid("want tcp, udp or both", da.Protocol))
	}
	if len(da.Ports) == 0 {
		p.addf(path+".ports", "required: list the ports, such as [5432] or [8000-8100]")
	}
	for i, raw := range da.Ports {
		r, err := parsePortRange(string(raw))
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
