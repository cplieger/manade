package egress

import (
	"errors"
	"fmt"
)

// Outcome is what a check's connection or lookup did, as the probe saw it.
type Outcome int

// The outcomes; the zero Outcome is none of them.
const (
	OutcomeConnected Outcome = iota + 1
	OutcomeRefused
	OutcomeTimeout
	// OutcomeNoRoute is a dial the kernel failed at once: no address or
	// route of the target's family, or an unreachable answer.
	OutcomeNoRoute
	OutcomeReset
	OutcomeResolved
	OutcomeLookupFailed
	// OutcomeError is any other failure.
	OutcomeError
)

var outcomeNames = map[Outcome]string{
	OutcomeConnected: "connected", OutcomeRefused: "was refused", OutcomeTimeout: "timed out",
	OutcomeNoRoute: "had no route", OutcomeReset: "was reset", OutcomeResolved: "resolved",
	OutcomeLookupFailed: "did not resolve", OutcomeError: "failed",
}

func (o Outcome) String() string {
	if s, ok := outcomeNames[o]; ok {
		return s
	}
	return "had no outcome"
}

// ProbeCounts are the packets the probe's counting rules hold: Dropped by
// the rules just before a drop, Passed by the rules after a chain's drops.
type ProbeCounts struct {
	Dropped, Passed uint64
}

// Since answers the packets counted after before was read. A count below
// before means the rules were loaded again in between, which zeroed them,
// so the new count is all that is known.
func (pc ProbeCounts) Since(before ProbeCounts) ProbeCounts {
	since := func(now, then uint64) uint64 {
		if now < then {
			return now
		}
		return now - then
	}
	return ProbeCounts{Dropped: since(pc.Dropped, before.Dropped), Passed: since(pc.Passed, before.Passed)}
}

// ProbeCounts sums the probe's counting rules in rb, which must hold rs as
// Compare checks it; an open host has none.
func (rs *Ruleset) ProbeCounts(rb *ReadBack) (ProbeCounts, error) {
	if rs.open {
		return ProbeCounts{}, errors.New("egress: an open host has no probe counters")
	}
	if err := rs.Compare(rb); err != nil {
		return ProbeCounts{}, err
	}
	if rs.Backend == NFTables {
		return rs.nftProbeCounts(rb.NFT)
	}
	return rs.iptProbeCounts(rb), nil
}

func (rs *Ruleset) nftProbeCounts(listing []byte) (ProbeCounts, error) {
	tables, err := parseNFT(listing)
	if err != nil {
		return ProbeCounts{}, err
	}
	var pc ProbeCounts
	t := tables["inet "+nftTable]
	for _, c := range rs.chains {
		packets := t.packets[nftChainNames[c.hook]]
		for i := range c.rules {
			pc.add(&c.rules[i], packets[i])
		}
	}
	return pc, nil
}

// iptProbeCounts reads each rule's lines: one rule is a line per
// destination, protocol and port, in iptLines' order.
func (rs *Ruleset) iptProbeCounts(rb *ReadBack) ProbeCounts {
	var pc ProbeCounts
	for f, listing := range map[family][]byte{ipv4: rb.IPv4, ipv6: rb.IPv6} {
		table := parseSave(listing)[tableFilter]
		for _, c := range rs.chains {
			packets := table.packets[iptChainNames[c.hook]]
			n := 0
			for i := range c.rules {
				for range c.rules[i].ipt(f) {
					pc.add(&c.rules[i], packets[n])
					n++
				}
			}
		}
	}
	return pc
}

func (pc *ProbeCounts) add(r *rule, packets uint64) {
	switch {
	case r.action != count:
	case r.after:
		pc.Passed += packets
	default:
		pc.Dropped += packets
	}
}

// Verdict is one check's result.
type Verdict struct {
	// Reason says why the check failed, naming its target; "" when it
	// passed.
	Reason string
	Pass   bool
	// Required is false for an allow entry's try, whose failure fails no
	// proof.
	Required bool
	// Unproven is a timeout no drop counted: the target may drop by
	// itself, so the rule is not shown at work.
	Unproven bool
}

// Judge rules on check c from its outcome and, for a block check, the
// probe counters' change across it.
func Judge(c *Check, o Outcome, counted ProbeCounts) Verdict {
	v := Verdict{Required: c.Kind != KindTry}
	fail := func(format string, args ...any) Verdict {
		v.Reason = fmt.Sprintf("%s (%s): ", c.Name, c.Addr) + fmt.Sprintf(format, args...)
		return v
	}
	switch c.Kind {
	case KindBlock:
		switch {
		case o != OutcomeTimeout:
			return fail("%s, where only a dropped connection times out", o)
		case counted.Passed > 0:
			return fail("a packet passed every drop")
		case counted.Dropped == 0:
			v.Unproven = true
			return fail("unproven: it timed out, but no drop counted it")
		}
	case KindReach, KindTry:
		if o != OutcomeConnected {
			return fail("%s, want connected", o)
		}
	case KindResolve:
		if o != OutcomeResolved {
			return fail("%s", o)
		}
	case KindNoIPv6:
		if o != OutcomeNoRoute && o != OutcomeTimeout {
			return fail("%s, but a host without ipv6 gives its jobs no IPv6", o)
		}
	default:
		return fail("no such check")
	}
	v.Pass = true
	return v
}
