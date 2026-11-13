package egress

import (
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
)

// want is one planned check as the test reads it: leg, kind and address.
type want struct {
	leg  Leg
	kind Kind
	addr string
}

func (w want) String() string { return fmt.Sprintf("%d/%d %s", w.leg, w.kind, w.addr) }

func planned(p *Plan) []want {
	out := make([]want, len(p.Checks))
	for i, c := range p.Checks {
		out[i] = want{leg: c.Leg, kind: c.Kind, addr: c.Addr}
	}
	return out
}

// probeInput is a host on the job network 172.18.0.0/24 behind the outward
// gateway 172.19.0.1, whose original resolv.conf named 10.0.0.53.
func probeInput(e config.Egress, dns ...netip.Addr) *ProbeInput {
	if len(dns) == 0 {
		dns = config.DefaultDNS
	}
	in := &ProbeInput{
		Host:           &config.Host{Name: "default", DNS: dns, Egress: e},
		Original:       Resolvers{Addrs: []netip.Addr{addr("10.0.0.53")}},
		JobGateway:     addr("172.18.0.1"),
		Listener:       addr("172.18.0.3"),
		OutwardGateway: addr("172.19.0.1"),
		APIPort:        7464,
		ListenerPort:   7465,
	}
	if e.IPv6 {
		in.JobGateway6 = addr("fdc6:6d61:6e61:6465::1")
		in.Listener6 = addr("fdc6:6d61:6e61:6465::3")
		in.OutwardGateway6 = addr("fd4d:6169:6e65::1")
		in.Own6 = addr("fd4d:6169:6e65::2")
	}
	return in
}

func TestPlanProbe(t *testing.T) {
	t.Parallel()
	const (
		job    = LegJob
		output = LegOutput
		block  = KindBlock
	)
	github := want{leg: job, kind: KindReach, addr: "api.github.com:443"}
	jobLookup := want{leg: job, kind: KindResolve, addr: "api.github.com"}
	outLookup := want{leg: output, kind: KindResolve, addr: "api.github.com"}
	for _, tc := range []struct {
		in   *ProbeInput
		name string
		want []want
	}{
		{
			name: "internet",
			in: probeInput(config.Egress{Internet: true, Allow: []config.Allow{
				{To: pfx("10.0.0.50/32"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{5432, 5432})},
				{To: pfx("192.168.2.0/24"), Protocol: config.ProtocolBoth, Ports: ports([2]uint16{8000, 8100})},
				{To: pfx("192.168.3.0/31"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{22, 22})},
				{To: pfx("192.168.4.4/32"), Protocol: config.ProtocolUDP, Ports: ports([2]uint16{514, 514})},
			}}),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "172.18.0.3:7465"},
				github, jobLookup,
				{job, KindNoIPv6, "[2606:4700:4700::1111]:443"},
				{job, KindTry, "10.0.0.50:5432"},
				{job, KindTry, "192.168.2.1:8000"},
				{job, KindTry, "192.168.3.0:22"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				outLookup,
				{output, KindNoIPv6, "[2606:4700:4700::1111]:443"},
			},
		},
		{
			// A configured resolver at the outward gateway keeps port 53
			// open from the host's stack only; jobs reach it through their
			// embedded resolver alone. Without internet every configured
			// resolver's other ports are dropped.
			name: "internet off, a resolver at the gateway, originals unknown",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{}, addr("172.19.0.1"), addr("1.1.1.1"))
				in.Original = Resolvers{Unknown: true}
				in.APIPort = 0
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "172.18.0.3:7465"},
				github,
				{job, block, "9.9.9.9:443"},
				jobLookup,
				{job, KindNoIPv6, "[2606:4700:4700::1111]:443"},
				{output, block, "172.19.0.1:1"},
				{output, block, "1.1.1.1:1"},
				outLookup,
				{output, KindNoIPv6, "[2606:4700:4700::1111]:443"},
			},
		},
		{
			// Without internet the output chain drops port 53 to a public
			// original resolver too; a loopback one is the machine's own.
			name: "internet off, public and loopback originals",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{}, addr("1.1.1.1"))
				in.Original.Addrs = append(in.Original.Addrs, addr("8.8.4.4"), addr("127.0.0.53"), addr("1.1.1.1"))
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "172.18.0.3:7465"},
				github,
				{job, block, "9.9.9.9:443"},
				jobLookup,
				{job, KindNoIPv6, "[2606:4700:4700::1111]:443"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				{output, block, "8.8.4.4:53"},
				{output, block, "1.1.1.1:1"},
				outLookup,
				{output, KindNoIPv6, "[2606:4700:4700::1111]:443"},
			},
		},
		{
			name: "a private configured resolver and an IPv6 original without ipv6",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{Internet: true}, addr("10.0.0.54"))
				in.Original.Addrs = append(in.Original.Addrs, addr("fd00::53"), addr("127.0.0.53"), addr("8.8.4.4"))
				in.JobGateway6 = addr("fdc6:6d61:6e61:6465::1")
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "10.0.0.54:53"},
				{job, block, "172.18.0.3:7465"},
				github, jobLookup,
				{job, KindNoIPv6, "[2606:4700:4700::1111]:443"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				{output, block, "10.0.0.54:1"},
				outLookup,
				{output, KindNoIPv6, "[2606:4700:4700::1111]:443"},
			},
		},
		{
			name: "ipv6 with the /64 learned",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{Internet: true, IPv6: true})
				in.Self64 = pfx("2a01:e0a:1259:2710::/64")
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:7464"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "172.18.0.3:7465"},
				{job, block, "[fdc6:6d61:6e61:6465::3]:7465"},
				github, jobLookup,
				{job, KindReach, "[2606:4700:4700::1111]:443"},
				{job, block, "[fd4d:6169:6e65::2]:1"},
				{job, block, "[fd4d:6169:6e65::1]:1"},
				{job, block, "[2a01:e0a:1259:2710::1]:1"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				outLookup,
			},
		},
		{
			// Until the /64 is learned the IPv6 internet check fails on
			// its own: job IPv6 stays dropped.
			name: "ipv6 with the /64 unlearned",
			in:   probeInput(config.Egress{Internet: true, IPv6: true}),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:7464"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "172.18.0.3:7465"},
				{job, block, "[fdc6:6d61:6e61:6465::3]:7465"},
				github, jobLookup,
				{job, KindReach, "[2606:4700:4700::1111]:443"},
				{job, block, "[fd4d:6169:6e65::2]:1"},
				{job, block, "[fd4d:6169:6e65::1]:1"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				outLookup,
			},
		},
		{
			name: "ipv6 with internet off",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{IPv6: true})
				in.Self64 = pfx("2a01:e0a:1259:2710::/64")
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:7464"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:1"},
				{job, block, "172.19.0.1:1"},
				{job, block, "172.19.0.1:53"},
				{job, block, "10.0.0.53:53"},
				{job, block, "172.18.0.3:7465"},
				{job, block, "[fdc6:6d61:6e61:6465::3]:7465"},
				github,
				{job, block, "9.9.9.9:443"},
				jobLookup,
				{job, block, "[2606:4700:4700::1111]:443"},
				{job, block, "[fd4d:6169:6e65::2]:1"},
				{job, block, "[fd4d:6169:6e65::1]:1"},
				{output, block, "172.19.0.1:1"},
				{output, block, "172.19.0.1:53"},
				{output, block, "10.0.0.53:53"},
				{output, block, "1.1.1.1:1"},
				{output, block, "8.8.8.8:1"},
				outLookup,
			},
		},
		{
			// Allow accepts precede the drops on the forward and output
			// chains, so an opened target is no block; the job gateway, the
			// host's own IPv6 address and another job are dropped ahead of
			// them and stay.
			name: "allow entries opening blocked targets",
			in: func() *ProbeInput {
				in := probeInput(config.Egress{IPv6: true, Allow: []config.Allow{
					{To: pfx("172.16.0.0/12"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{1, 65535})},
					{To: pfx("10.0.0.53/32"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{53, 53})},
					{To: pfx("10.0.0.54/32"), Protocol: config.ProtocolBoth, Ports: ports([2]uint16{1, 65535})},
					{To: pfx("fd4d:6169:6e65::/48"), Protocol: config.ProtocolTCP, Ports: ports([2]uint16{1, 1})},
				}}, addr("10.0.0.54"), addr("1.1.1.1"))
				in.Self64 = pfx("2a01:e0a:1259:2710::/64")
				return in
			}(),
			want: []want{
				{job, block, "172.18.0.1:7464"},
				{job, block, "172.18.0.1:1"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:7464"},
				{job, block, "[fdc6:6d61:6e61:6465::1]:1"},
				{job, block, "172.18.0.3:7465"},
				{job, block, "[fdc6:6d61:6e61:6465::3]:7465"},
				github,
				{job, block, "9.9.9.9:443"},
				jobLookup,
				{job, block, "[2606:4700:4700::1111]:443"},
				{job, block, "[fd4d:6169:6e65::2]:1"},
				{job, KindTry, "172.16.0.1:1"},
				{job, KindTry, "10.0.0.53:53"},
				{job, KindTry, "10.0.0.54:1"},
				{job, KindTry, "[fd4d:6169:6e65::1]:1"},
				{output, block, "1.1.1.1:1"},
				outLookup,
			},
		},
		{
			name: "open",
			in:   probeInput(config.Egress{Open: true, Internet: true}),
			want: []want{github, jobLookup, outLookup},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := planned(PlanProbe(tc.in)); !slices.Equal(got, tc.want) {
				t.Errorf("checks:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

func TestPlanProbeTargetsAndTimeouts(t *testing.T) {
	t.Parallel()
	p := PlanProbe(probeInput(config.Egress{Internet: true}, addr("1.1.1.1"), addr("8.8.8.8"), addr("9.9.9.9")))
	for _, c := range p.Checks {
		t.Run(fmt.Sprintf("%d %s", c.Leg, c.Addr), func(t *testing.T) {
			t.Parallel()
			wantTimeout := ConnectTimeout
			switch {
			case c.Kind == KindResolve && c.Leg == LegJob:
				// Docker's embedded resolver waits 4 s on each of the
				// three configured resolvers in turn, and relays the last
				// answer within 1 s.
				wantTimeout = 13 * time.Second
			case c.Kind == KindResolve:
				// 3 s for each of the three configured resolvers.
				wantTimeout = 9 * time.Second
			}
			if c.Timeout != wantTimeout {
				t.Errorf("Timeout = %v, want %v", c.Timeout, wantTimeout)
			}
			wantExchange := time.Duration(0)
			if c.Leg == LegJob {
				wantExchange = c.Timeout + 2*time.Second
			}
			if c.Exchange() != wantExchange {
				t.Errorf("Exchange = %v, want %v", c.Exchange(), wantExchange)
			}
			if c.Kind == KindBlock {
				if ap := netip.MustParseAddrPort(c.Addr); ap.Addr() != c.Target {
					t.Errorf("Target = %v, want the dialled address %v", c.Target, ap.Addr())
				}
			} else if c.Target.IsValid() {
				t.Errorf("Target = %v on a check that is not a block", c.Target)
			}
			if c.Name == "" {
				t.Error("the check has no name")
			}
		})
	}
}

// moby's embedded resolver gives up on its upstream after 4 s
// (extIOTimeout), so the forwarder must answer strictly before then.
func TestForwarderBudgetInsideEmbeddedWait(t *testing.T) {
	t.Parallel()
	if ForwarderBudget <= 0 || ForwarderBudget >= 4*time.Second {
		t.Errorf("ForwarderBudget = %v, want above 0 and under the embedded resolver's 4s", ForwarderBudget)
	}
}

func TestPlanBound(t *testing.T) {
	t.Parallel()
	const call = 3 * time.Minute
	t.Run("the internet plan", func(t *testing.T) {
		t.Parallel()
		p := PlanProbe(probeInput(config.Egress{Internet: true}))
		// Twelve calls; the job leg's six blocks, its GitHub connection and
		// its NoIPv6 check at 3 s and its lookup at 4 s for each of the two
		// default resolvers plus 1 s, each with 2 s of slack; the output
		// leg's three blocks and NoIPv6 at 3 s and its lookup at 3 s for
		// each of the two default resolvers.
		want := 12*call + (8*(3+2)+(2*4+1+2))*time.Second + (4*3+2*3)*time.Second
		if got := p.Bound(call); got != want {
			t.Errorf("Bound = %v, want %v", got, want)
		}
		if got := p.Deadline(call); got != 2*want {
			t.Errorf("Deadline = %v, want %v", got, 2*want)
		}
	})
	for _, tc := range []struct {
		name        string
		dns         []netip.Addr
		job, output time.Duration
	}{
		// The embedded resolver asks three upstreams at most, 4 s each,
		// and relays the last answer within 1 s.
		{"two resolvers", []netip.Addr{addr("1.1.1.1"), addr("8.8.8.8")}, 9 * time.Second, 6 * time.Second},
		{"four resolvers", []netip.Addr{addr("1.1.1.1"), addr("8.8.8.8"), addr("9.9.9.9"), addr("8.8.4.4")}, 13 * time.Second, 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := PlanProbe(probeInput(config.Egress{Open: true, Internet: true}, tc.dns...))
			var got []time.Duration
			for _, c := range p.Checks {
				if c.Kind == KindResolve {
					got = append(got, c.Timeout)
				}
			}
			if want := []time.Duration{tc.job, tc.output}; !slices.Equal(got, want) {
				t.Errorf("lookup timeouts = %v, want %v", got, want)
			}
			// An open host's plan: GitHub at 3 s and the job lookup, each
			// with 2 s of slack, then the output lookup.
			want := 12*call + (3+2)*time.Second + tc.job + 2*time.Second + tc.output
			if b := p.Bound(call); b != want {
				t.Errorf("Bound = %v, want %v", b, want)
			}
		})
	}
	t.Run("internet off, two resolvers", func(t *testing.T) {
		t.Parallel()
		p := PlanProbe(probeInput(config.Egress{}, addr("1.1.1.1"), addr("8.8.8.8")))
		var got []time.Duration
		for _, c := range p.Checks {
			if c.Kind == KindResolve {
				got = append(got, c.Timeout)
			}
		}
		// The embedded resolver's one upstream is the supervisor's
		// forwarder, plus 1 s; the output leg asks each resolver for 3 s.
		if want := []time.Duration{5 * time.Second, 6 * time.Second}; !slices.Equal(got, want) {
			t.Errorf("lookup timeouts = %v, want %v", got, want)
		}
		// The job leg's six blocks, GitHub, 9.9.9.9 and NoIPv6 at 3 s and
		// its lookup at 5 s, each with 2 s of slack; the output leg's five
		// blocks and NoIPv6 at 3 s and its lookup at 6 s.
		want := 12*call + (9*(3+2)+(5+2))*time.Second + (6*3+6)*time.Second
		if b := p.Bound(call); b != want {
			t.Errorf("Bound = %v, want %v", b, want)
		}
	})
	t.Run("exchanges summing past the call timeout", func(t *testing.T) {
		t.Parallel()
		e := config.Egress{Internet: true}
		for i := range 40 {
			e.Allow = append(e.Allow, config.Allow{
				To: pfx("10.0.0.1/32"), Protocol: config.ProtocolTCP,
				Ports: ports([2]uint16{uint16(8000 + i), uint16(8000 + i)}),
			})
		}
		p := PlanProbe(probeInput(e))
		var exchanges time.Duration
		for i := range p.Checks {
			exchanges += p.Checks[i].Exchange()
		}
		if exchanges <= call {
			t.Fatalf("the exchanges sum to %v, want more than the %v call timeout", exchanges, call)
		}
		// The call timeout bounds the attach's 101 answer only, so every
		// exchange adds to the bound beside the twelve calls.
		if got, least := p.Bound(call), 12*call+exchanges; got < least {
			t.Errorf("Bound = %v, want at least %v", got, least)
		}
	})
}
