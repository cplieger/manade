package pool

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// No job is ever stuck (D9): whatever boots, budgets, assignments, exits,
// restarts of either role, unreachable, disconnected and full hosts,
// refused starts, vanished images, failed ensures and settings changes
// came before, once the faults end and the budget frees, no assignment
// waits beside room for its runner, and every job runs. GitHub never
// cancels an assignment but to reassign it.

// randomWorld decodes a config and its scale sets: up to three hosts and
// three profiles, each profile on some of the hosts in some order and
// fitting each of them, and two to four scale sets.
func randomWorld(t testing.TB, in *bytesIn) *world {
	var hosts []hostSpec
	for i := range 1 + in.n(3) {
		hosts = append(hosts, hostSpec{name: fmt.Sprintf("h%d", i), cpus: int64(2 + in.n(8)), mem: int64(4 + in.n(28)), max: []int{0, 0, 0, 2, 4}[in.n(5)]})
	}
	var profiles []profileSpec
	for i := range 1 + in.n(3) {
		p := profileSpec{name: fmt.Sprintf("p%d", i), max: []int{0, 0, 0, 1, 3}[in.n(5)]}
		for _, k := range rand.New(rand.NewPCG(uint64(in.n(256)), 0)).Perm(len(hosts))[:1+in.n(len(hosts))] {
			p.hosts = append(p.hosts, hosts[k].name)
		}
		cpus, mem := int64(1<<20), int64(1<<20)
		for _, name := range p.hosts {
			h := hosts[slices.IndexFunc(hosts, func(h hostSpec) bool { return h.name == name })]
			cpus, mem = min(cpus, h.cpus), min(mem, h.mem)
		}
		p.cpus, p.mem = 1+int64(in.n(int(cpus))), 1+int64(in.n(int(mem)))
		profiles = append(profiles, p)
	}
	cfg := testConfig(hosts, profiles)
	cfg.HeadReserveAfter = time.Duration(in.n(4)) * time.Minute
	var sets []setSpec
	for i := range 2 + in.n(3) {
		sets = append(sets, setSpec{scope: fmt.Sprintf("acme/r%d", i), profile: profiles[in.n(len(profiles))].name, org: in.n(5) == 0})
	}
	return newWorld(t, cfg, sets...)
}

// step runs one choice: GitHub's, a host's, the pool's or the clock's.
func (w *world) step(in *bytesIn) {
	s := w.sets[in.n(len(w.sets))]
	h := in.n(len(w.hosts))
	switch in.n(16) {
	case 0, 1:
		w.queue(s, 1+in.n(3))
		w.sweep()
	case 2, 3:
		if !s.inFlight {
			w.sweep()
			w.begin(s)
		}
	case 4, 5:
		if s.inFlight {
			w.answer(s, in.n(5))
		}
	case 6, 7:
		w.retire()
		w.starts()
		w.takeJobs()
	case 8:
		for _, name := range w.names() {
			if r := w.runs[name]; r.alive && r.job != nil && r.set == s {
				w.finish(r)
				break
			}
		}
	case 9:
		w.cancel(s)
	case 10:
		w.reach(h, !w.hosts[h].reachable)
	case 11:
		// A proof that fails or holds again, an image that vanished or
		// was pulled again, a data volume that filled.
		mh := &w.hosts[h]
		switch in.n(3) {
		case 0:
			mh.serving = !mh.serving
		case 1:
			p := in.n(len(mh.ready))
			mh.ready[p] = !mh.ready[p] && slices.Contains(w.cfg.Profiles[p].Hosts, w.cfg.Hosts[h].Name)
		default:
			clear(mh.ready)
		}
		w.gates()
	case 12:
		w.hosts[h].refuse = !w.hosts[h].refuse
	case 13:
		if in.n(2) == 0 {
			w.crash(h)
		} else {
			w.stop(h)
		}
	case 14:
		if in.n(3) == 0 {
			w.boot()
		}
	case 15:
		w.advance(time.Duration(in.n(120)) * time.Second)
	}
}

// propCases is how many scenarios the plain test run covers.
var propCases = 10000

func FuzzPool_noJobIsStuck(f *testing.F) {
	r := rand.New(rand.NewPCG(5, 6))
	for range propCases {
		b := make([]byte, 200)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		in := bytesIn(data)
		w := randomWorld(t, &in)
		for len(in) > 0 {
			w.step(&in)
		}
		total := 0
		for _, s := range w.sets {
			total += s.jobs()
		}
		w.drain(8 + 2*total)
	})
}

func TestBootFundsEveryAssignmentOnceTheBudgetFrees(t *testing.T) {
	t.Parallel()
	// Revision 5's deployment on 2026-11-13: a restart left assignments
	// past the budget unfunded, GitHub kept them assigned while the scale
	// set kept polling, and one job never got a runner.
	cfg := testConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}}, []profileSpec{{name: "default", hosts: []string{"default"}, cpus: 2, mem: 4}})
	w := newWorld(t, cfg, setSpec{scope: "acme/app", profile: "default"}, setSpec{scope: "acme/lib", profile: "default"})
	app, lib := w.sets[0], w.sets[1]
	w.queue(app, 2)
	w.cycle(app)
	w.queue(lib, 6)
	lib.assigned, lib.queue = lib.queue, nil
	w.boot()
	w.starts()
	w.takeJobs()
	if got := hostsOf(w, lib)["default"]; got != 2 {
		t.Fatalf("lib runs %d at boot, want the 2 the budget covers beside app's 2", got)
	}
	for _, name := range w.names() {
		if r := w.runs[name]; r.alive && r.set == app {
			w.finish(r)
		}
	}
	w.starts()
	w.takeJobs()
	if got := hostsOf(w, lib)["default"]; got != 4 {
		t.Errorf("lib runs %d once app's runners end, want 4", got)
	}
	w.drain(10)
}

// startCases is how many scenarios the start-path property covers: each
// runs the pool's own start path, so it costs more than a ledger case.
var startCases = 2000

// randomRig decodes hosts, profiles and scopes as randomWorld does, over
// a rig whose log is discarded.
func randomRig(t testing.TB, in *bytesIn) *smodel {
	var hosts []hostSpec
	for i := range 1 + in.n(3) {
		hosts = append(hosts, hostSpec{name: fmt.Sprintf("h%d", i), cpus: int64(2 + in.n(8)), mem: int64(4 + in.n(28)), max: []int{0, 0, 0, 2, 4}[in.n(5)]})
	}
	var profiles []profileSpec
	for i := range 1 + in.n(2) {
		p := profileSpec{name: fmt.Sprintf("p%d", i), max: []int{0, 0, 0, 1, 3}[in.n(5)]}
		for _, k := range rand.New(rand.NewPCG(uint64(in.n(256)), 0)).Perm(len(hosts))[:1+in.n(len(hosts))] {
			p.hosts = append(p.hosts, hosts[k].name)
		}
		cpus, mem := int64(1<<20), int64(1<<20)
		for _, name := range p.hosts {
			h := hosts[slices.IndexFunc(hosts, func(h hostSpec) bool { return h.name == name })]
			cpus, mem = min(cpus, h.cpus), min(mem, h.mem)
		}
		p.cpus, p.mem = 1+int64(in.n(int(cpus))), 1+int64(in.n(int(mem)))
		profiles = append(profiles, p)
	}
	var scopes []string
	for i := range 1 + in.n(2) {
		scopes = append(scopes, fmt.Sprintf("acme/r%d", i))
	}
	cfg := poolConfig(hosts, profiles, scopes...)
	cfg.HeadReserveAfter = time.Duration(in.n(4)) * time.Minute
	return newModel(quietRig(t, cfg))
}

func FuzzPool_noJobIsStuckThroughTheStartPath(f *testing.F) {
	r := rand.New(rand.NewPCG(7, 8))
	for range startCases {
		b := make([]byte, 160)
		for i := range b {
			b[i] = byte(r.Uint32())
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		in := bytesIn(data)
		m := randomRig(t, &in)
		for len(in) > 0 {
			m.step(&in)
		}
		total := 0
		for _, s := range m.sets {
			total += s.jobs()
		}
		m.drain(8 + 2*total)
	})
}
