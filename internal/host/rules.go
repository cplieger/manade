package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/host/platform"
)

// maxJumpDeletes bounds the deletes of a misplaced jump's copies.
const maxJumpDeletes = 64

// ipTablesMatches lists the iptables matches the kernel has loaded.
const ipTablesMatches = "/proc/net/ip_tables_matches"

// ruleLoader loads rule sets through the kernel's backend and reads them back.
type ruleLoader struct {
	Run    platform.Command
	Kernel *platform.Kernel
	Root   platform.Root
}

// load loads rs, then reads the kernel back and compares, so a load the
// kernel took differently fails like one it refused.
func (l *ruleLoader) load(ctx context.Context, rs *egress.Ruleset) error {
	var err error
	if rs.Backend == egress.NFTables {
		_, err = l.Run(ctx, []byte(rs.NFT), "nft", "-f", "-")
	} else {
		err = l.loadIPT(ctx, rs)
	}
	if err != nil {
		return fmt.Errorf("loading the egress rules: %w", err)
	}
	return l.check(ctx, rs)
}

// check reads the kernel's rules back and compares them with rs.
func (l *ruleLoader) check(ctx context.Context, rs *egress.Ruleset) error {
	rb, err := l.readBack(ctx, rs)
	if err != nil {
		return fmt.Errorf("reading the egress rules back: %w", err)
	}
	return rs.Compare(rb)
}

// addLearned adds ls to the loaded rule set's learned sets, renewing the
// timeout of any already there; it is a resolver.SetWriter.
func (l *ruleLoader) addLearned(ctx context.Context, ls []egress.Learned) error {
	payload := []byte(egress.Refresh(l.Kernel.Backend, ls))
	var err error
	if l.Kernel.Backend == egress.NFTables {
		_, err = l.Run(ctx, payload, "nft", "-f", "-")
	} else {
		_, err = l.Run(ctx, payload, "ipset", "restore", "-exist")
	}
	if err != nil {
		return fmt.Errorf("adding learned GitHub addresses: %w", err)
	}
	return nil
}

func (l *ruleLoader) loadIPT(ctx context.Context, rs *egress.Ruleset) error {
	if err := l.loadSets(ctx, rs); err != nil {
		return err
	}
	if err := errors.Join(l.restore(ctx, rs, false, rs.IPv4), l.restore(ctx, rs, true, rs.IPv6)); err != nil {
		return err
	}
	if rs.IPv6NATReset != "" && l.Kernel.IPv6NAT {
		if err := l.restore(ctx, rs, true, rs.IPv6NATReset); err != nil {
			return err
		}
		for _, j := range rs.StaleJumps {
			l.deleteJump(ctx, j)
		}
	}
	for _, j := range rs.Jumps {
		if err := l.ensureFirst(ctx, j); err != nil {
			return err
		}
	}
	return nil
}

func (l *ruleLoader) loadSets(ctx context.Context, rs *egress.Ruleset) error {
	if rs.IPSet == "" {
		return nil
	}
	if _, err := l.Run(ctx, nil, "ipset", "list", "-n"); err != nil {
		return fmt.Errorf("the kernel offers no ip_set, which an iptables host needs loaded on the machine with xt_set: %w", err)
	}
	_, err := l.Run(ctx, []byte(rs.IPSet), "ipset", "restore", "-exist")
	return err
}

func (l *ruleLoader) restore(ctx context.Context, rs *egress.Ruleset, v6 bool, payload string) error {
	if payload == "" {
		return nil
	}
	_, err := l.Run(ctx, []byte(payload), l.Kernel.Binary(saveTool(v6)+"-restore"), "-w", platform.XtablesWait, "--noflush")
	if err != nil && rs.IPSet != "" && !l.hasSetMatch() {
		return fmt.Errorf("%w; the kernel lists no set match, so xt_set may be missing on the machine", err)
	}
	return err
}

func (l *ruleLoader) hasSetMatch() bool {
	b, err := os.ReadFile(l.Root.Path(ipTablesMatches))
	return err == nil && slices.Contains(strings.Fields(string(b)), "set")
}

// ensureFirst makes j its chain's first rule, removing any copy elsewhere.
func (l *ruleLoader) ensureFirst(ctx context.Context, j egress.Jump) error {
	bin := l.Kernel.Binary(saveTool(j.IPv6))
	out, err := l.Run(ctx, nil, bin, "-w", platform.XtablesWait, "-t", j.Table, "-S", j.Chain)
	if err != nil {
		return err
	}
	if firstRule(string(out), j.Chain) == "-j "+j.Target {
		return nil
	}
	l.deleteJump(ctx, j)
	_, err = l.Run(ctx, nil, bin, "-w", platform.XtablesWait, "-t", j.Table, "-I", j.Chain, "1", "-j", j.Target)
	return err
}

// deleteJump deletes every copy of j; the delete of a missing rule fails,
// which ends the loop.
func (l *ruleLoader) deleteJump(ctx context.Context, j egress.Jump) {
	bin := l.Kernel.Binary(saveTool(j.IPv6))
	for range maxJumpDeletes {
		if _, err := l.Run(ctx, nil, bin, "-w", platform.XtablesWait, "-t", j.Table, "-D", j.Chain, "-j", j.Target); err != nil {
			return
		}
	}
}

// firstRule answers the first rule `iptables -S chain` lists, after its
// "-A chain " prefix.
func firstRule(s, chain string) string {
	for line := range strings.Lines(s) {
		if r, ok := strings.CutPrefix(strings.TrimSpace(line), "-A "+chain+" "); ok {
			return r
		}
	}
	return ""
}

func saveTool(v6 bool) string {
	if v6 {
		return "ip6tables"
	}
	return "iptables"
}

func (l *ruleLoader) readBack(ctx context.Context, rs *egress.Ruleset) (*egress.ReadBack, error) {
	rb := &egress.ReadBack{}
	var err error
	if rs.Backend == egress.NFTables {
		rb.NFT, err = l.Run(ctx, nil, "nft", "-j", "list", "ruleset")
		return rb, err
	}
	// -c lists each rule's counters, which the probe's verdicts read.
	if rb.IPv4, err = l.Run(ctx, nil, l.Kernel.Binary("iptables-save"), "-c"); err != nil {
		return nil, err
	}
	if rb.IPv6, err = l.Run(ctx, nil, l.Kernel.Binary("ip6tables-save"), "-c"); err != nil {
		return nil, err
	}
	if rs.IPSet != "" {
		rb.IPSets, err = l.Run(ctx, nil, "ipset", "list", "-t")
	}
	return rb, err
}

// What the 5-second check restores, as guardHooks.Restored names it.
const (
	restoredRules      = "rules"
	restoredIPv6       = "ipv6"
	restoredBridgeNF6  = "bridge-nf-call-ip6tables"
	restoredResolvConf = "resolv.conf"
)

// guardHooks are told what the 5-second check did. Either may be nil.
type guardHooks struct {
	// Restored is called once per restored item, after it was restored.
	Restored func(what string)
	// Checked is called after each check that ended with everything in
	// place.
	Checked func()
}

// wanted is what the 5-second check holds in place.
type wanted struct {
	// Loading, when set, is held from each load's generation to its load.
	Loading sync.Locker
	// Generate builds the rule set from what the host holds at the time, so
	// a restore carries the learned addresses.
	Generate func() (*egress.Ruleset, error)
	Resolv   []byte
	// IPv6Off holds IPv6 off in the namespace; BridgeNF6 holds
	// bridge-nf-call-ip6tables at 1.
	IPv6Off   bool
	BridgeNF6 bool
}

// guard holds the host's rules and namespace settings in place. Its methods
// are safe for concurrent use; a check and a swap run one at a time.
type guard struct {
	rules *ruleLoader
	log   *slog.Logger
	hooks guardHooks
	want  wanted
	mu    sync.Mutex
}

// newGuard answers a guard holding want, which the caller has loaded.
func newGuard(rules *ruleLoader, want wanted, log *slog.Logger, hooks guardHooks) *guard {
	return &guard{rules: rules, want: want, log: log, hooks: hooks}
}

// checkInterval is how often run checks.
const checkInterval = 5 * time.Second

// run checks every checkInterval until ctx ends, answering nil then, or
// until a restore fails, answering why: the caller then stops the daemon,
// since jobs may no longer run under the host's policy.
func (g *guard) run(ctx context.Context) error {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := g.check(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
}

// check restores whatever has left its wanted state, logging each restore
// at ERROR, and fails only when a restore fails. The Checked hook runs after
// the lock is released, so its slow call holds up no swap or probe read.
func (g *guard) check(ctx context.Context) error {
	if err := g.restore(ctx); err != nil {
		return err
	}
	if g.hooks.Checked != nil {
		g.hooks.Checked()
	}
	return nil
}

func (g *guard) restore(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	rs, err := g.want.Generate()
	if err != nil {
		return err
	}
	if cerr := g.rules.check(ctx, rs); cerr != nil {
		if err := g.load(ctx, g.want); err != nil {
			return fmt.Errorf("restoring the egress rules: %w", err)
		}
		g.restored(restoredRules, cerr)
	}
	if err := g.hold(func(s *setting) { g.restored(s.what, errors.New(s.why)) }); err != nil {
		return fmt.Errorf("restoring %w", err)
	}
	return nil
}

// hold sets each wanted setting that does not hold, calling set after each;
// g.mu is held.
func (g *guard) hold(set func(s *setting)) error {
	for _, s := range g.settings() {
		if !s.wanted {
			continue
		}
		if ok, err := s.holds(); err == nil && ok {
			continue
		}
		if err := s.set(); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
		set(&s)
	}
	return nil
}

type setting struct {
	holds     func() (bool, error)
	set       func() error
	what, why string
	wanted    bool
}

func (g *guard) settings() []setting {
	r, w := g.rules.Root, g.want
	return []setting{
		{
			what: restoredIPv6, why: "IPv6 was on in the host's namespace", wanted: w.IPv6Off,
			holds: func() (bool, error) { return netsetup.IPv6Off(r) },
			set:   func() error { return netsetup.TurnIPv6Off(r) },
		},
		{
			what: restoredBridgeNF6, why: "bridge-nf-call-ip6tables was not 1", wanted: w.BridgeNF6,
			holds: func() (bool, error) { return netsetup.BridgeNF6On(r) },
			set:   func() error { return netsetup.SetBridgeNF6(r) },
		},
		{
			what: restoredResolvConf, why: "resolv.conf was replaced", wanted: true,
			holds: func() (bool, error) { return netsetup.ResolvConfHolds(r, w.Resolv) },
			set:   func() error { return netsetup.WriteResolvConf(r, w.Resolv) },
		},
	}
}

func (g *guard) restored(what string, why error) {
	g.log.Error("A host setting changed from outside and was restored", "what", what, "err", why)
	if g.hooks.Restored != nil {
		g.hooks.Restored(what)
	}
}

// swap loads want's rules, resolv.conf and namespace settings and holds
// them from now on. When the rules fail to load it loads the ones it held
// again and keeps holding them, answering the failure as kept. fatal is a
// failure a restore would answer: the old rules failing to load again, or a
// setting that cannot be written.
func (g *guard) swap(ctx context.Context, want wanted) (kept, fatal error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.load(ctx, want); err != nil {
		if gerr := g.load(ctx, g.want); gerr != nil {
			return err, fmt.Errorf("restoring the old egress rules after a failed swap: %w", gerr)
		}
		return err, nil
	}
	g.want = want
	if err := g.hold(func(*setting) {}); err != nil {
		return nil, fmt.Errorf("setting %w", err)
	}
	return nil, nil
}

// refresh loads the rule set generated from what the host holds now, for a
// change the host made itself, such as a learned /64. A failure is one a
// restore would answer, and the caller stops the host.
func (g *guard) refresh(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.load(ctx, g.want); err != nil {
		return fmt.Errorf("loading the egress rules again: %w", err)
	}
	return nil
}

// load generates want's rule set and loads it, holding want.Loading.
func (g *guard) load(ctx context.Context, want wanted) error {
	if want.Loading != nil {
		want.Loading.Lock()
		defer want.Loading.Unlock()
	}
	rs, err := want.Generate()
	if err != nil {
		return err
	}
	return g.rules.load(ctx, rs)
}

// probeCounts reads the probe counters of the rule set the guard holds; a
// rule set that no longer matches fails, as the next check restores it.
func (g *guard) probeCounts(ctx context.Context) (egress.ProbeCounts, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rs, err := g.want.Generate()
	if err != nil {
		return egress.ProbeCounts{}, err
	}
	rb, err := g.rules.readBack(ctx, rs)
	if err != nil {
		return egress.ProbeCounts{}, fmt.Errorf("reading the probe counters: %w", err)
	}
	return rs.ProbeCounts(rb)
}
