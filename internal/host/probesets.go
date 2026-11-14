package host

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/probe"
)

// probeSets fills the loaded rule set's probe sets and reads its probe
// counters, for a probe run.
type probeSets struct {
	rules *ruleLoader
	// guard answers the running posture's guard.
	guard func() *guard
	ipv6  bool
}

var _ probe.Sets = (*probeSets)(nil)

func (p *probeSets) SetSources(ctx context.Context, addrs []netip.Addr) error {
	return p.load(ctx, egress.ProbeSources(p.rules.Kernel.Backend, p.ipv6, addrs))
}

func (p *probeSets) SetTarget(ctx context.Context, a netip.Addr) error {
	return p.load(ctx, egress.ProbeTarget(p.rules.Kernel.Backend, p.ipv6, a))
}

func (p *probeSets) Clear(ctx context.Context) error {
	return p.load(ctx, egress.ProbeSources(p.rules.Kernel.Backend, p.ipv6, nil))
}

func (p *probeSets) load(ctx context.Context, payload string) error {
	l := p.rules
	var err error
	if l.Kernel.Backend == egress.NFTables {
		_, err = l.Run(ctx, []byte(payload), "nft", "-f", "-")
	} else {
		_, err = l.Run(ctx, []byte(payload), "ipset", "restore")
	}
	if err != nil {
		return fmt.Errorf("the probe sets: %w", err)
	}
	return nil
}

// Counts reads the probe counters of the rule set the guard holds.
func (p *probeSets) Counts(ctx context.Context) (egress.ProbeCounts, error) {
	return p.guard().probeCounts(ctx)
}
