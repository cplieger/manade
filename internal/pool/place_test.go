package pool

import (
	"testing"
	"time"
)

// twoHosts is a slow machine preferred for small and a main one: small
// fills nas first and spills onto default.
func twoHosts() (cfg []hostSpec, profiles []profileSpec) {
	return []hostSpec{{name: "default", cpus: 8, mem: 32}, {name: "nas", cpus: 2, mem: 3}},
		[]profileSpec{{name: "small", hosts: []string{"nas", "default"}, cpus: 1, mem: 1}, {name: "large", hosts: []string{"default"}, cpus: 4, mem: 8}}
}

// hostsOf answers the host name of each of s's live containers, by name.
func hostsOf(w *world, s *mset) map[string]int {
	out := map[string]int{}
	for _, r := range w.runs {
		if r.alive && r.set == s {
			out[w.cfg.Hosts[r.host].Name]++
		}
	}
	return out
}

func TestPlacementTakesTheFirstHostWithRoom(t *testing.T) {
	t.Parallel()
	cfg := testConfig(twoHosts())
	t.Run("fillsThePreferredHostFirst", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "small"})
		w.queue(w.sets[0], 5)
		w.cycle(w.sets[0])
		if got := hostsOf(w, w.sets[0]); got["nas"] != 2 || got["default"] != 3 {
			t.Errorf("runners by host = %v, want 2 on nas and 3 on default", got)
		}
	})
	t.Run("neverOnAHostTheProfileDoesNotList", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "large"})
		w.queue(w.sets[0], 3)
		w.cycle(w.sets[0])
		if got := hostsOf(w, w.sets[0]); got["nas"] != 0 || got["default"] != 2 {
			t.Errorf("runners by host = %v, want 2 on default, which holds two 4-CPU runners", got)
		}
	})
	for _, tc := range []struct {
		fault func(w *world)
		name  string
	}{
		{name: "notServing", fault: func(w *world) { w.hosts[1].serving = false }},
		{name: "unreachable", fault: func(w *world) { w.hosts[1].reachable = false }},
		{name: "profileNotReady", fault: func(w *world) { w.hosts[1].ready[0] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "small"})
			tc.fault(w)
			w.gates()
			w.queue(w.sets[0], 2)
			w.cycle(w.sets[0])
			if got := hostsOf(w, w.sets[0]); got["nas"] != 0 || got["default"] != 2 {
				t.Errorf("runners by host = %v, want both on default", got)
			}
		})
	}
}

func TestSlotsLeaveAHostThatStopsTakingThem(t *testing.T) {
	t.Parallel()
	cfg := testConfig(twoHosts())
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "small"})
	s := w.sets[0]
	w.queue(s, 2)
	w.sweep()
	w.begin(s)
	if got := pendOn(cfg, s.b); got["nas"] != 2 {
		t.Fatalf("slots by host = %v, want 2 on nas", got)
	}
	// nas's image vanished while the poll was in flight: GitHub assigns
	// both jobs, and their runners start on default.
	w.hosts[1].ready[0] = false
	w.gates()
	w.answer(s, 2)
	w.starts()
	if got := hostsOf(w, s); got["nas"] != 0 || got["default"] != 2 {
		t.Errorf("runners by host = %v, want both on default", got)
	}
	if s.b.asg.unbacked != 0 {
		t.Errorf("unbacked = %d, want 0", s.b.asg.unbacked)
	}
}

func TestUnreachableHostKeepsItsReservations(t *testing.T) {
	t.Parallel()
	cfg := testConfig([]hostSpec{{name: "a", cpus: 4, mem: 8}, {name: "b", cpus: 2, mem: 8}},
		[]profileSpec{{name: "p", hosts: []string{"a", "b"}, cpus: 2, mem: 2}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "p"})
	s := w.sets[0]
	w.queue(s, 2)
	w.cycle(s)
	w.deliver(s)
	w.reach(0, false)
	// Both jobs end while a does not answer: its reservations stand, so
	// the next jobs fit only b.
	for _, name := range w.names() {
		w.finish(w.runs[name])
	}
	w.queue(s, 3)
	w.deliver(s)
	w.cycle(s)
	if got := hostsOf(w, s); got["a"] != 0 || got["b"] != 1 {
		t.Errorf("live runners by host = %v, want 1 on b alone", got)
	}
	w.reach(0, true)
	w.advance(time.Second)
	w.cycle(s)
	if got := hostsOf(w, s); got["a"] != 2 || got["b"] != 1 {
		t.Errorf("live runners by host = %v once a answers, want 2 on a and 1 on b", got)
	}
}

func TestProfileCapCountsSlotsOnEveryHost(t *testing.T) {
	t.Parallel()
	hosts, profiles := twoHosts()
	profiles[0].max = 3
	cfg := testConfig(hosts, profiles)
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "small"}, setSpec{scope: "acme/lib", profile: "small"})
	w.queue(w.sets[0], 2)
	w.queue(w.sets[1], 2)
	w.cycle(w.sets[0])
	w.cycle(w.sets[1])
	if n := len(w.runs); n != 3 {
		t.Errorf("runners = %d, want small's max_runners of 3", n)
	}
}
