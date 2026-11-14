package host

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/egress"
	"github.com/cplieger/manade/internal/host/platform"
)

const (
	resolvRendered = "nameserver 1.1.1.1\nnameserver 8.8.8.8\n"
	disableAll     = "/proc/sys/net/ipv6/conf/all/disable_ipv6"
	disableDefault = "/proc/sys/net/ipv6/conf/default/disable_ipv6"
)

func iptKernel() *platform.Kernel { return &platform.Kernel{Backend: egress.IPTables, RawOff: true} }

func generateFor(t *testing.T, h *config.Host) func() (*egress.Ruleset, error) {
	t.Helper()
	return func() (*egress.Ruleset, error) { return egress.Generate(&egress.Input{Host: h}, egress.IPTables) }
}

func mustGenerate(t *testing.T, h *config.Host) *egress.Ruleset {
	t.Helper()
	rs, err := generateFor(t, h)()
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestRulesLoadIPTables(t *testing.T) {
	t.Run("loaded, jumped first and read back", func(t *testing.T) {
		k := newFakeIPT(false)
		l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, nil)}
		if err := l.load(t.Context(), mustGenerate(t, section("10.0.0.50/32"))); err != nil {
			t.Fatalf("Load: %v", err)
		}
		for _, j := range []struct {
			chain, target string
			v6            bool
		}{
			{"FORWARD", "DOCKER-USER", false},
			{"OUTPUT", "MANADE-OUT", false},
			{"INPUT", "MANADE-IN", false},
			{"DOCKER-USER", "MANADE-FWD", false},
			{"FORWARD", "MANADE-FWD", true},
			{"OUTPUT", "MANADE-OUT", true},
			{"INPUT", "MANADE-IN", true},
		} {
			if r := k.chain(j.v6, "filter", j.chain); len(r) == 0 || r[0] != "-j "+j.target {
				t.Errorf("ipv6 %t %s = %q, want -j %s first", j.v6, j.chain, r, j.target)
			}
		}
		for _, c := range k.commands() {
			if strings.HasPrefix(c, "iptables") || strings.HasPrefix(c, "ip6tables") {
				t.Errorf("%q ran the nf_tables variant, not the legacy binary", c)
			}
		}
	})
	t.Run("a reload keeps one jump per chain", func(t *testing.T) {
		k := newFakeIPT(false)
		l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, nil)}
		rs := mustGenerate(t, section())
		for range 2 {
			if err := l.load(t.Context(), rs); err != nil {
				t.Fatal(err)
			}
		}
		if got := k.chain(false, "filter", "OUTPUT"); !slices.Equal(got, []string{"-j MANADE-OUT"}) {
			t.Errorf("OUTPUT = %q, want one jump", got)
		}
	})
	t.Run("a jump the daemon moved down is put back first, once", func(t *testing.T) {
		k := newFakeIPT(false)
		l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, nil)}
		rs := mustGenerate(t, section())
		if err := l.load(t.Context(), rs); err != nil {
			t.Fatal(err)
		}
		k.edit(func(tb map[bool]map[string]*xtable) {
			f := tb[true]["filter"]
			f.chains["FORWARD"] = slices.Insert(f.chains["FORWARD"], 0, "-j DOCKER-FORWARD")
		})
		if err := l.check(t.Context(), rs); err == nil {
			t.Fatal("Check passed with MANADE-FWD second in ip6tables FORWARD")
		}
		if err := l.load(t.Context(), rs); err != nil {
			t.Fatal(err)
		}
		if got := k.chain(true, "filter", "FORWARD"); !slices.Equal(got, []string{"-j MANADE-FWD", "-j DOCKER-FORWARD"}) {
			t.Errorf("ip6tables FORWARD = %q, want MANADE-FWD first and no second copy", got)
		}
	})
	t.Run("no ip_set", func(t *testing.T) {
		k := newFakeIPT(false)
		k.fail["ipset list -n"] = errors.New("Kernel error received: ipset protocol error")
		l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, nil)}
		if err := l.load(t.Context(), mustGenerate(t, section())); err == nil || !strings.Contains(err.Error(), "ip_set") {
			t.Errorf("Load = %v, want a refusal naming ip_set", err)
		}
	})
	t.Run("no xt_set", func(t *testing.T) {
		k := newFakeIPT(false)
		k.noSetMatch = true
		l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, map[string]string{ipTablesMatches: "conntrack\ntcp\nudp\n"})}
		if err := l.load(t.Context(), mustGenerate(t, section())); err == nil || !strings.Contains(err.Error(), "xt_set") {
			t.Errorf("Load = %v, want a refusal naming xt_set", err)
		}
	})
}

func TestRulesLoadNFTables(t *testing.T) {
	k := newFakeIPT(false)
	k.nft = true
	l := &ruleLoader{Run: k.run, Kernel: &platform.Kernel{Backend: egress.NFTables}, Root: tree(t, nil)}
	rs, err := egress.Generate(&egress.Input{Host: section()}, egress.NFTables)
	if err != nil {
		t.Fatal(err)
	}
	err = l.load(t.Context(), rs)
	if len(k.nftFiles) != 1 || k.nftFiles[0] != rs.NFT {
		t.Errorf("nft -f - read %d payloads, want the rule set's once", len(k.nftFiles))
	}
	if err == nil || !strings.Contains(err.Error(), "differ") {
		t.Errorf("Load = %v, want a refusal of a kernel that holds none of the table", err)
	}
}

// guardFixture is a loaded host the guard holds: IPv4 only, IPv6 off,
// resolv.conf written.
func guardFixture(t *testing.T) (*fakeIPT, platform.Root, *guard, *[]string) {
	t.Helper()
	k := newFakeIPT(false)
	r := tree(t, map[string]string{disableAll: "1\n", disableDefault: "1\n", "/etc/resolv.conf": resolvRendered})
	l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: r}
	want := wanted{Generate: generateFor(t, section()), Resolv: []byte(resolvRendered), IPv6Off: true}
	rs, _ := want.Generate()
	if err := l.load(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
	var restored []string
	g := newGuard(l, want, slog.New(slog.DiscardHandler), guardHooks{Restored: func(w string) { restored = append(restored, w) }})
	return k, r, g, &restored
}

func TestGuardCheck(t *testing.T) {
	t.Run("nothing moved restores nothing", func(t *testing.T) {
		_, _, g, restored := guardFixture(t)
		checked := 0
		g.hooks.Checked = func() { checked++ }
		if err := g.check(t.Context()); err != nil || len(*restored) != 0 || checked != 1 {
			t.Errorf("Check = %v, restored %q, checked %d", err, *restored, checked)
		}
	})
	t.Run("the checked hook runs with the guard free", func(t *testing.T) {
		_, _, g, _ := guardFixture(t)
		free := false
		g.hooks.Checked = func() {
			if free = g.mu.TryLock(); free {
				g.mu.Unlock()
			}
		}
		if err := g.check(t.Context()); err != nil || !free {
			t.Errorf("Check = %v, guard free in the hook %v", err, free)
		}
	})
	t.Run("a failed restore calls no checked hook", func(t *testing.T) {
		k, _, g, _ := guardFixture(t)
		checked := 0
		g.hooks.Checked = func() { checked++ }
		k.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-OUT"] = []string{} })
		k.fail[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
		if err := g.check(t.Context()); err == nil || checked != 0 {
			t.Errorf("Check = %v, checked %d, want the failure and no hook", err, checked)
		}
	})
	t.Run("flushed rules are loaded again and counted", func(t *testing.T) {
		k, _, g, restored := guardFixture(t)
		want := k.chain(false, "filter", "MANADE-FWD")
		k.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-FWD"] = []string{} })
		if err := g.check(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(*restored, []string{restoredRules}) || !slices.Equal(k.chain(false, "filter", "MANADE-FWD"), want) {
			t.Errorf("restored %q, MANADE-FWD back %t", *restored, slices.Equal(k.chain(false, "filter", "MANADE-FWD"), want))
		}
	})
	t.Run("IPv6 back on is turned off again", func(t *testing.T) {
		_, r, g, restored := guardFixture(t)
		writeAt(t, r, disableAll, "0\n")
		if err := g.check(t.Context()); err != nil || !slices.Equal(*restored, []string{restoredIPv6}) || readAt(t, r, disableAll) != "1\n" {
			t.Errorf("Check = %v, restored %q, disable_ipv6 %q", err, *restored, readAt(t, r, disableAll))
		}
	})
	t.Run("a replaced resolv.conf is written again", func(t *testing.T) {
		_, r, g, restored := guardFixture(t)
		writeAt(t, r, "/etc/resolv.conf", "nameserver 127.0.0.11\n")
		if err := g.check(t.Context()); err != nil || !slices.Equal(*restored, []string{restoredResolvConf}) || readAt(t, r, "/etc/resolv.conf") != resolvRendered {
			t.Errorf("Check = %v, restored %q, resolv.conf %q", err, *restored, readAt(t, r, "/etc/resolv.conf"))
		}
	})
	t.Run("bridge-nf-call-ip6tables cleared is set again", func(t *testing.T) {
		_, r, g, restored := guardFixture(t)
		writeAt(t, r, platform.BridgeNFCall6, "0\n")
		g.want.BridgeNF6, g.want.IPv6Off = true, false
		if err := g.check(t.Context()); err != nil || !slices.Equal(*restored, []string{restoredBridgeNF6}) || readAt(t, r, platform.BridgeNFCall6) != "1\n" {
			t.Errorf("Check = %v, restored %q, sysctl %q", err, *restored, readAt(t, r, platform.BridgeNFCall6))
		}
	})
	t.Run("a failed restore is answered", func(t *testing.T) {
		k, _, g, restored := guardFixture(t)
		k.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-OUT"] = []string{} })
		k.fail[platform.LegacyDir+"/iptables-restore"] = errors.New("iptables-restore: line 3 failed")
		if err := g.check(t.Context()); err == nil || !strings.Contains(err.Error(), "restoring the egress rules") || len(*restored) != 0 {
			t.Errorf("Check = %v, restored %q, want the failure and no restore counted", err, *restored)
		}
	})
}

func TestGuardRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		k, _, g, _ := guardFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		k.edit(func(tb map[bool]map[string]*xtable) { tb[false]["filter"].chains["MANADE-OUT"] = []string{} })
		k.fail[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
		done := make(chan error, 1)
		go func() { done <- g.run(ctx) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Run ended before its first check: %v", err)
		default:
		}
		err := <-done
		if err == nil {
			t.Error("Run = nil after a failed restore, want the failure")
		}
	})
	synctest.Test(t, func(t *testing.T) {
		_, _, g, _ := guardFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- g.run(ctx) }()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v on a cancelled context, want nil", err)
		}
	})
}

func TestGuardSwap(t *testing.T) {
	t.Run("a new section is loaded and held", func(t *testing.T) {
		k, r, g, _ := guardFixture(t)
		h := section("10.0.0.50/32")
		h.DNS = h.DNS[:1]
		kept, fatal := g.swap(t.Context(), wanted{Generate: generateFor(t, h), Resolv: []byte("nameserver 1.1.1.1\n"), IPv6Off: true})
		if kept != nil || fatal != nil {
			t.Fatalf("Swap = %v, %v", kept, fatal)
		}
		if !slices.ContainsFunc(k.chain(false, "filter", "MANADE-FWD"), func(r string) bool { return strings.Contains(r, "10.0.0.50/32") }) {
			t.Error("the new allow entry is not loaded")
		}
		if readAt(t, r, "/etc/resolv.conf") != "nameserver 1.1.1.1\n" {
			t.Errorf("resolv.conf = %q", readAt(t, r, "/etc/resolv.conf"))
		}
		writeAt(t, r, "/etc/resolv.conf", resolvRendered)
		if err := g.check(t.Context()); err != nil || readAt(t, r, "/etc/resolv.conf") != "nameserver 1.1.1.1\n" {
			t.Errorf("the guard holds the old resolv.conf: %v", err)
		}
	})
	t.Run("a section the kernel took but did not read back is unloaded", func(t *testing.T) {
		k, _, g, _ := guardFixture(t)
		old := k.chain(false, "filter", "MANADE-FWD")
		k.failOnce[platform.LegacyDir+"/iptables-save"] = errors.New("iptables-save: resource busy")
		kept, fatal := g.swap(t.Context(), wanted{Generate: generateFor(t, section("10.0.0.50/32")), Resolv: []byte(resolvRendered), IPv6Off: true})
		if kept == nil || fatal != nil {
			t.Fatalf("Swap = %v, %v, want kept", kept, fatal)
		}
		if !slices.Equal(k.chain(false, "filter", "MANADE-FWD"), old) {
			t.Error("the half-loaded section's rules stayed in the kernel")
		}
	})
	t.Run("a section that fails to load keeps the old rules", func(t *testing.T) {
		k, r, g, _ := guardFixture(t)
		old := k.chain(false, "filter", "MANADE-FWD")
		bad := wanted{Generate: func() (*egress.Ruleset, error) { return nil, errors.New("no backend") }, Resolv: []byte("nameserver 9.9.9.9\n")}
		kept, fatal := g.swap(t.Context(), bad)
		if kept == nil || fatal != nil {
			t.Fatalf("Swap = %v, %v, want kept", kept, fatal)
		}
		if !slices.Equal(k.chain(false, "filter", "MANADE-FWD"), old) || readAt(t, r, "/etc/resolv.conf") != resolvRendered {
			t.Error("a kept swap changed the rules or resolv.conf")
		}
	})
}

// heldLocker records whether it is held.
type heldLocker struct{ held bool }

func (l *heldLocker) Lock()   { l.held = true }
func (l *heldLocker) Unlock() { l.held = false }

func TestGuardLoadsUnderLoading(t *testing.T) {
	k := newFakeIPT(false)
	l := &ruleLoader{Run: k.run, Kernel: iptKernel(), Root: tree(t, map[string]string{"/etc/resolv.conf": resolvRendered, disableAll: "1\n", disableDefault: "1\n"})}
	lock := &heldLocker{}
	var held []string
	want := func(name string) wanted {
		return wanted{Loading: lock, Resolv: []byte(resolvRendered), IPv6Off: true, Generate: func() (*egress.Ruleset, error) {
			if lock.held {
				held = append(held, name)
			}
			return egress.Generate(&egress.Input{Host: section()}, egress.IPTables)
		}}
	}
	g := newGuard(l, want("restore"), slog.New(slog.DiscardHandler), guardHooks{})
	if err := g.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := g.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if kept, fatal := g.swap(t.Context(), want("swap")); kept != nil || fatal != nil {
		t.Fatal(kept, fatal)
	}
	// The check's own generation only compares, so it needs no lock; each
	// of the three loads must generate under it.
	if !slices.Equal(held, []string{"restore", "restore", "swap"}) || lock.held {
		t.Errorf("generations under Loading = %q, want one per load; held after: %v", held, lock.held)
	}
}
