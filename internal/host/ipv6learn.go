package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/hostapi"
)

const (
	// echoInterval is how often a host with ipv6 and internet asks its
	// ipv6_echo, so a rotated prefix is followed.
	echoInterval = 10 * time.Minute
	// echoTimeout bounds one ask, its retries included.
	echoTimeout = time.Minute
	// echoLimit caps the echo's body, a few hundred bytes.
	echoLimit = 64 << 10
)

// echoFunc answers the body of a GET of url made over IPv6 only.
type echoFunc func(ctx context.Context, url string) ([]byte, error)

// echoOverIPv6 answers the echoFunc whose connections are IPv6 only and go
// direct: through a proxy the echo would show the proxy's address.
func echoOverIPv6() (echoFunc, error) {
	tr, err := httpclient.CloneDefaultTransport()
	if err != nil {
		return nil, err
	}
	tr.Proxy = nil
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return d.DialContext(ctx, "tcp6", addr)
	}
	client := &http.Client{Transport: tr, Timeout: 2 * echoTimeout, CheckRedirect: httpclient.DefaultRedirectPolicy}
	return func(ctx context.Context, url string) ([]byte, error) {
		return httpclient.GetBytes(ctx, client, url, httpclient.WithMaxBodyBytes(echoLimit))
	}, nil
}

// ask64 asks echo at url for the host's IPv6 egress address, its `ip=`
// line, and answers that address's /64, the machine's.
func ask64(ctx context.Context, echo echoFunc, url string) (netip.Prefix, error) {
	ctx, cancel := context.WithTimeout(ctx, echoTimeout)
	defer cancel()
	body, err := echo(ctx, url)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("asking %s for the host's IPv6 address: %w", url, err)
	}
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "ip=")
		if !ok {
			continue
		}
		a, err := netip.ParseAddr(v)
		switch {
		case err != nil:
			return netip.Prefix{}, fmt.Errorf("%s answered ip=%q, not an address", url, v)
		case a.Is4() || a.Is4In6():
			return netip.Prefix{}, fmt.Errorf("%s answered the IPv4 address %s, so the machine's /64 is unknown; give the host IPv6 egress or set an ipv6_echo that answers over IPv6", url, a.Unmap())
		case !globalUnicast6.Contains(a):
			return netip.Prefix{}, fmt.Errorf("%s answered %s, which is not a global IPv6 address", url, a)
		}
		return netip.PrefixFrom(a, 64).Masked(), nil
	}
	return netip.Prefix{}, fmt.Errorf("%s answered no ip= line", url)
}

var globalUnicast6 = netip.MustParsePrefix("2000::/3")

// learned64 is the machine's /64 as last learned, or why it is unknown.
type learned64 struct {
	err    error
	prefix netip.Prefix
}

// unasked64 is what the host holds before it asks its ipv6_echo.
func unasked64() *learned64 {
	return &learned64{err: errors.New("the host has not asked its ipv6_echo yet")}
}

// learns64 reports whether the host learns the machine's /64.
func (n *hostNet) learns64() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.host.Egress.IPv6 && n.host.Egress.Internet
}

// moved64 follows a reload from section old to h: turning internet off
// forgets the learned /64, so turning it on again starts unasked, and
// turning it on wakes learn64 to ask at once. n.mu is held.
func (n *hostNet) moved64(old, h *config.Host) {
	switch {
	case old.Egress.Internet && !h.Egress.Internet:
		n.self64.Store(unasked64())
	case !old.Egress.Internet && h.Egress.Internet:
		select {
		case n.wake64 <- struct{}{}:
		default:
		}
	}
}

// learn64 asks the host's ipv6_echo while the host has internet: at once,
// every echoInterval and when moved64 wakes it, until ctx ends, answering
// nil then. A changed /64 loads the rules again, and a failed load ends it
// with the failure; a failed ask keeps the last /64.
func (n *hostNet) learn64(ctx context.Context, echo echoFunc) error {
	t := time.NewTicker(echoInterval)
	defer t.Stop()
	for {
		if n.learns64() {
			if err := n.ask64(ctx, echo); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		case <-n.wake64:
		}
	}
}

func (n *hostNet) ask64(ctx context.Context, echo echoFunc) error {
	n.mu.Lock()
	url := n.host.Egress.IPv6Echo
	n.mu.Unlock()
	p, err := ask64(ctx, echo, url)
	if ctx.Err() != nil {
		return nil
	}
	n.mu.Lock()
	// A reload that turned internet off during the ask forgot the /64.
	internet := n.host.Egress.Internet
	old := n.self64.Load()
	switch {
	case !internet:
	case err == nil:
		n.self64.Store(&learned64{prefix: p})
	case !old.prefix.IsValid():
		n.self64.Store(&learned64{err: err})
	}
	n.mu.Unlock()
	switch {
	case !internet:
		return nil
	case err != nil:
		n.log.Warn("The machine's IPv6 /64 was not learned; job IPv6 stays dropped until it is", "err", err, "kept", old.prefix)
		return nil
	case p == old.prefix:
		return nil
	}
	n.log.Info("Learned the machine's IPv6 /64", "prefix", p, "was", old.prefix)
	return n.Guard.refresh(ctx)
}

// iPv6Status answers the learned /64 or why it is unknown, on a host with
// ipv6 and internet; nil on any other.
func (n *hostNet) iPv6Status() *hostapi.IPv6Status {
	if !n.learns64() {
		return nil
	}
	l := n.self64.Load()
	if l.prefix.IsValid() {
		return &hostapi.IPv6Status{Prefix: l.prefix}
	}
	return &hostapi.IPv6Status{Reason: l.err.Error()}
}
