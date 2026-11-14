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
func echoOverIPv6() echoFunc {
	var d net.Dialer
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp6", addr)
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     90 * time.Second,
	}
	client := &http.Client{Transport: tr, Timeout: 2 * echoTimeout, CheckRedirect: httpclient.DefaultRedirectPolicy}
	return func(ctx context.Context, url string) ([]byte, error) {
		return httpclient.GetBytes(ctx, client, url, httpclient.WithMaxBodyBytes(echoLimit))
	}
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

// unasked64 is what a posture holds before it asks its ipv6_echo.
func unasked64() *learned64 {
	return &learned64{err: errors.New("the host has not asked its ipv6_echo yet")}
}

// learn64 asks posture p's ipv6_echo at once and every echoInterval until
// ctx ends, answering nil then. A changed /64 loads the rules again, and a
// failed load ends it with the failure; a failed ask keeps the last /64.
func (n *hostNet) learn64(ctx context.Context, p *posture) error {
	t := time.NewTicker(echoInterval)
	defer t.Stop()
	for {
		if err := n.learnOnce(ctx, p); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (n *hostNet) learnOnce(ctx context.Context, p *posture) error {
	got, err := ask64(ctx, n.echo, p.host.Load().Egress.IPv6Echo)
	if ctx.Err() != nil {
		return nil
	}
	old := p.self64.Load()
	switch {
	case err != nil:
		if !old.prefix.IsValid() {
			p.self64.Store(&learned64{err: err})
		}
		n.log.Warn("The machine's IPv6 /64 was not learned; job IPv6 stays dropped until it is", "err", err, "kept", old.prefix)
		return nil
	case got == old.prefix:
		return nil
	}
	n.log.Info("Learned the machine's IPv6 /64", "prefix", got, "was", old.prefix)
	err = p.guard.refresh(ctx, func() { p.self64.Store(&learned64{prefix: got}) })
	if ctx.Err() != nil {
		// A change or a stop ended the posture mid-load: a change loads the next posture's rules, a stop takes the daemon down.
		return nil
	}
	return err
}

// iPv6Status answers the learned /64 or why it is unknown, on a posture with
// ipv6 and internet; nil on any other. It waits on nothing a change does.
func (n *hostNet) iPv6Status() *hostapi.IPv6Status {
	p := n.posture.Load()
	if !p.learns64 {
		return nil
	}
	l := p.self64.Load()
	if l.prefix.IsValid() {
		return &hostapi.IPv6Status{Prefix: l.prefix}
	}
	return &hostapi.IPv6Status{Reason: l.err.Error()}
}
