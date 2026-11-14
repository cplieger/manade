package images

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/manade/internal/config"
)

// errNoAddress is a lookup that answered no address.
var errNoAddress = errors.New("no address")

// refusal is a registry an internet: false host resolved outside its allow
// entries; detail is what the status and the not-ready log line say.
type refusal struct{ detail string }

// resolve decides whether r may be pulled now. Only a registry given by name
// on an internet: false host is resolved: an address the config already
// checked, and a name in GitHub's list, are reached as they are. A lookup
// that fails is an error, which takes the backoff like a failed pull.
func (m *Manager) resolve(ctx context.Context, r *ref) (served bool, err error) {
	m.mu.Lock()
	h := m.host
	m.mu.Unlock()
	name, port := config.Registry(r.name)
	if h.Egress.Open || h.Egress.Internet || name == "" || (m.byName != nil && m.byName(name)) {
		m.setRefused(r, nil)
		return true, nil
	}
	if _, perr := netip.ParseAddr(name); perr == nil {
		m.setRefused(r, nil)
		return true, nil
	}
	addrs, err := m.lookup(ctx, name)
	if err == nil && len(addrs) == 0 {
		err = errNoAddress
	}
	if err != nil {
		return false, fmt.Errorf("resolving the registry %s: %w", name, err)
	}
	var outside []netip.Addr
	for _, a := range addrs {
		if !h.Egress.AllowsTCP(a.Unmap(), port) {
			outside = append(outside, a)
		}
	}
	if len(outside) == 0 {
		m.setRefused(r, nil)
		return true, nil
	}
	ref := &refusal{detail: refusalDetail(h, name, port, addrs, outside)}
	m.setRefused(r, ref)
	m.log.Warn("A profile is not ready: its registry resolved outside the host's allow entries", "image", r.name, "reason", "refused", "detail", ref.detail)
	return false, nil
}

func (m *Manager) setRefused(r *ref, ref *refusal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r.refused, r.resolved = ref, time.Now()
}

func refusalDetail(h *config.Host, name string, port uint16, addrs, outside []netip.Addr) string {
	allow := make([]string, 0, len(h.Egress.Allow))
	for _, a := range h.Egress.Allow {
		ports := make([]string, len(a.Ports))
		for i, p := range a.Ports {
			ports[i] = strconv.Itoa(int(p.First))
			if p.Last != p.First {
				ports[i] += "-" + strconv.Itoa(int(p.Last))
			}
		}
		allow = append(allow, a.To.String()+" "+protocolName(a.Protocol)+" "+strings.Join(ports, ","))
	}
	if len(allow) == 0 {
		allow = []string{"none"}
	}
	return fmt.Sprintf("the registry %s resolved to %s, and %s lie outside the allow entries of host %s (%s) on TCP port %d, "+
		"which egress.internet: false confines its pulls to; add an allow entry covering them, or the registry's answer must move inside the entries, "+
		"which the host checks again at each images.refresh",
		name, join(addrs), join(outside), h.Name, strings.Join(allow, "; "), port)
}

func join(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

func protocolName(p config.Protocol) string {
	switch p {
	case config.ProtocolUDP:
		return "udp"
	case config.ProtocolBoth:
		return "tcp and udp"
	}
	return "tcp"
}
