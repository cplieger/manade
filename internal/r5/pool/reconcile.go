package pool

import (
	"context"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/r5/config"
	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
	"github.com/cplieger/manade/internal/r5/obs"
)

const (
	reconcileEvery = 30 * time.Second
	// tailLines is how much of an unclean exit's log is kept.
	tailLines = 50
	// diagPath is the runner's own log directory, and diagLimit the most of
	// it one runner's archive keeps.
	diagPath  = "/home/runner/_diag"
	diagLimit = 8 << 20
	// eventsRetry spaces reconnects to a failed event stream; the pass
	// every reconcileEvery catches the exits between.
	eventsRetry = 5 * time.Second
)

// runReconciler runs a pass every reconcileEvery and after each die event,
// one at a time. It returns once ctx ends and the pass in flight is done.
func (p *Pool) runReconciler(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { p.watchDies(ctx) })
	wg.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.died:
				// A die and the end of ctx can both be ready, and select picks either.
				if ctx.Err() != nil {
					return
				}
				p.pass(ctx)
			}
		}
	})
	scheduler.RunLoop(ctx, func(ctx context.Context) { p.pass(ctx) }, scheduler.LoopOptions{Interval: reconcileEvery})
}

func (p *Pool) watchDies(ctx context.Context) {
	for {
		err := p.dk.Events(ctx, func(string) { wake(p.died) })
		if ctx.Err() != nil {
			return
		}
		p.m.DockerError(obs.DockerEvents)
		p.log.Warn("Docker's event stream failed, so exits wait for the next pass", "error", err)
		t := time.NewTimer(eventsRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// pass is one reconciler pass and its record. A good pass refreshes the
// reconciler's health lease. It reports a good pass that also settled
// every orphan, which boot needs before any session opens.
func (p *Pool) pass(ctx context.Context) bool {
	p.passMu.Lock()
	defer p.passMu.Unlock()
	ok, settled := p.reconcile(ctx)
	if ok {
		p.mu.Lock()
		p.passed = p.now()
		p.mu.Unlock()
	}
	p.m.Pass(ctx, ok)
	return ok && settled
}

// work is what one pass does to the containers it listed. strays are the
// running containers of no configured scale set that no record holds yet,
// aged the held ones past their lifetime, and vanished the held ones whose
// container the list lacks.
type work struct {
	exited   []docker.Summary
	expired  []*runner
	idle     []*runner
	adopt    []docker.Summary
	strays   []docker.Summary
	aged     []docker.Summary
	probes   []docker.Summary
	lost     []*runner
	vanished []*stray
}

// dockerFailed counts a failed Docker call of op and reports true, or
// reports false once ctx has ended: a call shutdown cut short is no
// failure, so it is neither counted nor logged.
func (p *Pool) dockerFailed(ctx context.Context, op obs.DockerOp) bool {
	if ctx.Err() != nil {
		return false
	}
	p.m.DockerError(op)
	return true
}

// reconcile lists this pool's containers and acts on each: exits, the
// profile's max_lifetime and idle_timeout, orphans to adopt, runners whose
// scale set the config dropped, and records whose container vanished. It
// reports whether Docker answered the list, and whether every orphan was
// adopted or removed.
func (p *Pool) reconcile(ctx context.Context) (listed, settled bool) {
	known := p.created()
	list, err := p.dk.ListContainers(ctx)
	if err != nil {
		if p.dockerFailed(ctx, obs.DockerList) {
			p.log.Warn("Listing the pool's containers failed, so the pass is retried", "error", err)
		}
		return false, false
	}
	w := p.plan(list, known)
	for _, r := range w.lost {
		p.exited(ctx, r.set, r.name, r.id, r.completed, obs.ExitLost)
		p.log.Warn("A runner's container vanished, so its record is closed", "scope", r.set.ss.Scope, "profile", r.set.ss.Profile, "runner", r.name)
	}
	for _, st := range w.vanished {
		p.unregisterStray(ctx, st.scope, st.r.id, st.r.name)
		p.log.Warn("A runner's container vanished, so its record is closed", "scope", st.scope, "runner", st.r.name)
	}
	for _, c := range w.exited {
		p.exit(ctx, c)
	}
	for _, r := range w.expired {
		p.expire(ctx, r)
	}
	for _, r := range w.idle {
		p.retire(ctx, r)
	}
	for _, c := range w.aged {
		p.expireStray(ctx, c)
	}
	settled = true
	for _, c := range w.adopt {
		settled = p.adopt(ctx, c) && settled
	}
	for _, c := range w.strays {
		settled = p.takeStray(ctx, c) && settled
	}
	p.removeProbes(ctx, w.probes)
	p.tidyImages(ctx)
	p.pruneDiag()
	return true, settled
}

// removeProbes removes the leftover probes a pass found.
func (p *Pool) removeProbes(ctx context.Context, probes []docker.Summary) {
	for _, c := range probes {
		if err := p.dk.RemoveOwned(ctx, c.ID); err != nil && p.dockerFailed(ctx, obs.DockerRemove) {
			p.log.Warn("Removing a leftover probe container failed, so the pass retries it", "container", c.ID, "error", err)
		}
	}
}

// created is the runners whose container existed before the list.
func (p *Pool) created() map[*runner]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	known := map[*runner]bool{}
	for _, s := range p.sets {
		for _, r := range s.runners {
			if r.container != "" {
				known[r] = true
			}
		}
	}
	return known
}

// plan sorts the listed containers by what the pass does to them, and
// closes here, under the lock, each record of known whose container the
// list lacks. A start can create a container after the daemon builds its
// answer, so a record known does not hold is never lost. A probe goes only
// once no call records it and it is old.
func (p *Pool) plan(list []docker.Summary, known map[*runner]bool) work {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var w work
	listed := map[string]bool{}
	for _, c := range list {
		listed[c.ID] = true
		switch {
		case c.Labels[docker.LabelProbe] == "":
			p.sortOne(&w, c, now)
		case !p.probes[c.ID] && leftover(c.Labels[docker.LabelCreated], now):
			w.probes = append(w.probes, c)
		}
	}
	for _, s := range p.sets {
		for _, r := range s.runners {
			if known[r] && !listed[r.container] {
				s.end(r, now)
				w.lost = append(w.lost, r)
			}
		}
	}
	// Only a pass takes in a stray, so every one held predates the list.
	for id, st := range p.strays {
		if !listed[id] {
			delete(p.strays, id)
			w.vanished = append(w.vanished, st)
		}
	}
	return w
}

// sortOne files c in w. A runner whose start is still in flight has no
// container yet and is left to the start worker.
func (p *Pool) sortOne(w *work, c docker.Summary, now time.Time) {
	s := p.setOf(c.Labels)
	var r *runner
	if s != nil {
		r = s.runners[c.Labels[docker.LabelRunner]]
	}
	switch {
	case c.State == "exited" || c.State == "dead":
		w.exited = append(w.exited, c)
	case s == nil:
		if st := p.strays[c.ID]; st == nil {
			w.strays = append(w.strays, c)
		} else if now.Sub(st.r.created) > p.lifetime(st.profile) {
			w.aged = append(w.aged, c)
		}
	case r == nil:
		w.adopt = append(w.adopt, c)
	case r.container == "":
	case now.Sub(r.created) > p.cfg.Profiles[s.profile].MaxLifetime:
		w.expired = append(w.expired, r)
	case r.state == idle && (p.stopping || s.scope.public || now.Sub(r.created) > p.cfg.Profiles[s.profile].IdleTimeout):
		w.idle = append(w.idle, r)
	}
}

// leftover reports a probe whose label shows it created more than
// probeAge before now.
func leftover(created string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, created)
	return err == nil && now.Sub(at) > probeAge
}

// lifetime is profile's max_lifetime, the default one for -1, a profile
// the config no longer has.
func (p *Pool) lifetime(profile int) time.Duration {
	if profile < 0 {
		return config.DefaultMaxLifetime
	}
	return p.cfg.Profiles[profile].MaxLifetime
}

// setOf finds the scale set a container's labels name, nil for one the
// config no longer has.
func (p *Pool) setOf(labels map[string]string) *set {
	for _, s := range p.sets {
		if s.ss.Scope == labels[docker.LabelScope] && s.ss.Profile == labels[docker.LabelProfile] {
			return s
		}
	}
	return nil
}

// exit records an exited container's outcome, keeps its log tail when the
// exit was not clean and its diagnostics, then removes it and its record.
func (p *Pool) exit(ctx context.Context, c docker.Summary) {
	ctr, found, err := p.dk.InspectContainer(ctx, c.ID)
	if err != nil || !found {
		if err != nil && p.dockerFailed(ctx, obs.DockerInspect) {
			p.log.Warn("Inspecting an exited runner failed, so the pass retries it", "container", c.ID, "error", err)
		}
		return
	}
	name := c.Labels[docker.LabelRunner]
	reason := obs.ExitCompleted
	switch {
	case ctr.State.OOMKilled:
		reason = obs.ExitOOM
	case ctr.State.ExitCode != 0:
		reason = obs.ExitFailed
	}
	if reason != obs.ExitCompleted {
		p.tail(ctx, c, &ctr)
	}
	if !p.remove(ctx, box{c.ID, name}) {
		return
	}
	p.mu.Lock()
	s := p.setOf(c.Labels)
	completed := false
	if s != nil {
		if r := s.runners[name]; r != nil {
			completed = r.completed
			s.end(r, p.now())
		}
	}
	p.mu.Unlock()
	if s == nil {
		p.dropStray(ctx, c)
	}
	id, _ := strconv.ParseInt(c.Labels[docker.LabelRunnerID], 10, 64)
	p.exited(ctx, s, name, id, completed, reason)
	p.log.Info("Container exited", "scope", c.Labels[docker.LabelScope], "profile", c.Labels[docker.LabelProfile], "runner", name,
		"image", ctr.Image, "exit_code", ctr.State.ExitCode, "oom", ctr.State.OOMKilled,
		"duration", ctr.State.FinishedAt.Sub(ctr.State.StartedAt).Round(time.Second).String())
}

// tail logs an unclean exit's last log lines.
func (p *Pool) tail(ctx context.Context, c docker.Summary, ctr *docker.Container) {
	lines, err := p.dk.Logs(ctx, c.ID, tailLines)
	if err != nil && ctx.Err() == nil {
		p.log.Warn("Reading a failed runner's log failed", "runner", c.Labels[docker.LabelRunner], "error", err)
	}
	p.log.Warn("A runner exited with an error", "scope", c.Labels[docker.LabelScope], "profile", c.Labels[docker.LabelProfile],
		"runner", c.Labels[docker.LabelRunner], "exit_code", ctr.State.ExitCode, "oom", ctr.State.OOMKilled,
		"tail", textsafe.Sanitize(strings.Join(lines, "\n")))
}

// exited counts a runner's end once its record is gone, and removes its
// registration when no JobCompleted named it: the runner never took a job,
// or died holding one.
func (p *Pool) exited(ctx context.Context, s *set, name string, id int64, completed bool, reason obs.ExitReason) {
	if s == nil {
		return
	}
	p.m.RunnerExited(s.ss.ScaleSet, reason)
	if !completed && id > 0 {
		p.unregister(ctx, s.ss.Scope, id, name)
	}
	p.poke()
}

// expire removes a runner past its profile's max_lifetime.
func (p *Pool) expire(ctx context.Context, r *runner) {
	if !p.remove(ctx, box{r.container, r.name}) {
		return
	}
	if held, completed := p.closeRecord(r, true); held {
		p.exited(ctx, r.set, r.name, r.id, completed, obs.ExitLifetime)
		p.log.Warn("A runner passed its profile's max_lifetime, so it was removed", "scope", r.set.ss.Scope, "profile", r.set.ss.Profile, "runner", r.name)
	}
}

// retire removes an idle runner: its registration first, so GitHub hands
// no job to a runner about to vanish, then its container. A runner GitHub
// says holds a job is busy, and any other failure keeps both for the next
// pass.
func (p *Pool) retire(ctx context.Context, r *runner) {
	switch err := p.gh.RemoveRunner(ctx, r.set.ss.Scope, r.id); {
	case errors.Is(err, github.ErrRunnerBusy):
		p.mu.Lock()
		r.state = busy
		p.mu.Unlock()
		return
	case err != nil:
		if ctx.Err() == nil {
			p.log.Warn("Removing an idle runner's registration failed, so the pass retries it", "scope", r.set.ss.Scope, "runner", r.name, "error", err)
		}
		return
	}
	if !p.remove(ctx, box{r.container, r.name}) {
		return
	}
	if held, _ := p.closeRecord(r, false); held {
		p.exited(ctx, r.set, r.name, 0, true, obs.ExitIdle)
		p.log.Info("Removed an idle runner", "scope", r.set.ss.Scope, "profile", r.set.ss.Profile, "runner", r.name)
	}
}

// closeRecord deletes r's record, which frees its reservation, and
// reports whether it still held one and whether a JobCompleted named it.
// spend is false only for an idle runner GitHub let go, which held no job
// GitHub could still count (set.end).
func (p *Pool) closeRecord(r *runner, spend bool) (held, completed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.set.runners[r.name] != r {
		return false, false
	}
	if spend {
		r.set.end(r, p.now())
	} else {
		delete(r.set.runners, r.name)
	}
	return true, r.completed
}

// adopt takes a running container no record holds, a previous process's
// runner: readOrphan settles an idle one, and a busy one gets a
// record and its reservation back from its inspected limits. It reports
// false while a runner that may hold a job is left with no record, so no
// reservation.
func (p *Pool) adopt(ctx context.Context, c docker.Summary) bool {
	p.mu.Lock()
	s := p.setOf(c.Labels)
	p.mu.Unlock()
	id, err := strconv.ParseInt(c.Labels[docker.LabelRunnerID], 10, 64)
	if err != nil {
		p.log.Warn("A labelled container carries no runner ID, so it is left in place", "container", c.ID)
		return true
	}
	if busy, ok := p.readOrphan(ctx, s.scope.cfg, c, id); !busy {
		return ok
	}
	r, ok := p.inspectBusy(ctx, c, id)
	if r == nil {
		return ok
	}
	r.set = s
	p.mu.Lock()
	if _, taken := s.runners[r.name]; !taken {
		s.runners[r.name] = r
	}
	p.mu.Unlock()
	p.log.Info("Adopted a busy runner", "scope", s.ss.Scope, "profile", s.ss.Profile, "runner", r.name, "image", r.image)
	return true
}

// readOrphan reads a runner no record holds through its scope: one whose
// registration is gone loses its container, and an idle or offline one is
// deregistered and removed unless GitHub refuses that as busy. It reports
// whether the runner is busy, so its reservation must be held, and whether
// its reads and removals got an answer.
func (p *Pool) readOrphan(ctx context.Context, sc *config.Scope, c docker.Summary, id int64) (busy, ok bool) {
	name := c.Labels[docker.LabelRunner]
	gr, found, err := p.rest.Runner(ctx, sc, id)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			p.log.Warn("Reading an orphaned runner failed, so the pass retries it", "scope", sc.Key, "runner", name, "error", err)
		}
		return false, false
	case !found:
		p.remove(ctx, box{c.ID, name})
		return false, true
	case gr.Busy:
		return true, true
	}
	switch rerr := p.gh.RemoveRunner(ctx, sc.Key, id); {
	case rerr == nil:
		p.remove(ctx, box{c.ID, name})
		return false, true
	case errors.Is(rerr, github.ErrRunnerBusy):
		// GitHub handed it a job after the read, so it is held busy.
		return true, true
	default:
		if ctx.Err() == nil {
			p.log.Warn("Removing an idle orphaned runner's registration failed, so the pass retries it", "scope", sc.Key, "runner", name, "error", rerr)
		}
		return false, false
	}
}

// inspectBusy builds a busy runner's record, with no set, from its
// container: the reservation its inspected limits hold and its creation
// time. It answers nil with ok true for a container already gone, and nil
// with ok false when the inspect failed.
func (p *Pool) inspectBusy(ctx context.Context, c docker.Summary, id int64) (r *runner, ok bool) {
	ctr, found, err := p.dk.InspectContainer(ctx, c.ID)
	if err != nil || !found {
		if err != nil {
			p.dockerFailed(ctx, obs.DockerInspect)
		}
		return nil, !found && err == nil
	}
	r = &runner{
		name: c.Labels[docker.LabelRunner], image: ctr.Image, container: c.ID, id: id, state: busy, created: p.now(),
		res: res{cpu: ctr.HostConfig.NanoCPUs / 1_000_000, mem: ctr.HostConfig.Memory, runners: 1},
	}
	if at, err := time.Parse(time.RFC3339, ctr.Config.Labels[docker.LabelCreated]); err == nil {
		r.created = at
	}
	if p.cfg.Capacity.Disk > 0 {
		r.res.disk, _ = ctr.DiskBytes()
	}
	return r, true
}

// takeStray takes a running runner whose scale set the config no longer
// has: its profile left the config or its scope, or its scope left. With
// its scope still configured it is read like an orphan; one that is busy,
// or whose scope's credentials are gone, is held as a stray with its
// inspected reservation until it ends (Pool.strays).
func (p *Pool) takeStray(ctx context.Context, c docker.Summary) bool {
	scope, profile := c.Labels[docker.LabelScope], c.Labels[docker.LabelProfile]
	id, err := strconv.ParseInt(c.Labels[docker.LabelRunnerID], 10, 64)
	sc := p.scopes[scope]
	if sc != nil && err == nil {
		if busy, ok := p.readOrphan(ctx, sc.cfg, c, id); !busy {
			return ok
		}
	}
	r, ok := p.inspectBusy(ctx, c, id)
	if r == nil {
		return ok
	}
	pr, configured := p.byName[profile]
	if !configured {
		pr = -1
	}
	p.mu.Lock()
	p.strays[c.ID] = &stray{r: r, scope: scope, profile: pr}
	p.mu.Unlock()
	if sc == nil {
		p.log.Warn("A runner's scope is no longer configured, so it keeps its reservation until it exits",
			"container", c.ID, "scope", scope, "profile", profile, "runner", r.name)
		return true
	}
	p.log.Info("Holding a busy runner the config no longer serves until it exits", "scope", scope, "profile", profile, "runner", r.name)
	return true
}

// expireStray removes a stray past its lifetime.
func (p *Pool) expireStray(ctx context.Context, c docker.Summary) {
	if !p.remove(ctx, box{c.ID, c.Labels[docker.LabelRunner]}) {
		return
	}
	p.dropStray(ctx, c)
	p.log.Warn("A runner passed its max_lifetime, so it was removed", "scope", c.Labels[docker.LabelScope],
		"profile", c.Labels[docker.LabelProfile], "runner", c.Labels[docker.LabelRunner])
}

// dropStray ends a removed runner of no configured scale set, held as a
// stray or exited before a pass took it: it frees a held reservation and
// removes the registration its labels name (unregisterStray).
func (p *Pool) dropStray(ctx context.Context, c docker.Summary) {
	p.mu.Lock()
	_, held := p.strays[c.ID]
	delete(p.strays, c.ID)
	p.mu.Unlock()
	id, _ := strconv.ParseInt(c.Labels[docker.LabelRunnerID], 10, 64)
	p.unregisterStray(ctx, c.Labels[docker.LabelScope], id, c.Labels[docker.LabelRunner])
	if held {
		p.poke()
	}
}

// unregisterStray removes an ended stray's registration where its scope is
// still configured, since no JobCompleted can name a runner of no scale
// set. A scope that left has no client to remove it with.
func (p *Pool) unregisterStray(ctx context.Context, scope string, id int64, name string) {
	if p.scopes[scope] != nil && id > 0 {
		p.unregister(ctx, scope, id, name)
	}
}

// box is a runner's container: its ID and the runner's name.
type box struct{ id, runner string }

// remove saves a runner's diagnostics, then removes its container, and
// reports whether it is gone.
func (p *Pool) remove(ctx context.Context, b box) bool {
	p.saveDiag(ctx, b)
	if err := p.dk.RemoveOwned(ctx, b.id); err != nil {
		if p.dockerFailed(ctx, obs.DockerRemove) {
			p.log.Warn("Removing a runner's container failed, so the pass retries it", "runner", b.runner, "error", err)
		}
		return false
	}
	return true
}

// saveDiag copies a runner's _diag into DIAG_DIR as <runner>.tar, cut at
// diagLimit. A runner that left none leaves no file.
func (p *Pool) saveDiag(ctx context.Context, b box) {
	if p.diag == nil {
		return
	}
	file := b.runner + ".tar"
	f, err := p.diag.Create(file)
	if err != nil {
		p.log.Warn("Creating a runner's diagnostics file failed", "runner", b.runner, "error", err)
		return
	}
	found, err := p.dk.CopyArchive(ctx, b.id, diagPath, f, diagLimit)
	err = errors.Join(err, f.Close())
	if !found {
		_ = p.diag.Remove(file)
	}
	if err != nil && p.dockerFailed(ctx, obs.DockerArchive) {
		p.log.Warn("Saving a runner's diagnostics failed or passed the cap", "runner", b.runner, "error", err)
	}
}

// pruneDiag removes the diagnostics older than DIAG_RETENTION.
func (p *Pool) pruneDiag() {
	if p.diag == nil {
		return
	}
	entries, err := fs.ReadDir(p.diag.FS(), ".")
	if err != nil {
		p.log.Warn("Listing the diagnostics directory failed", "error", err)
		return
	}
	cutoff := p.now().Add(-p.cfg.DiagRetention)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			if err := p.diag.Remove(e.Name()); err != nil {
				p.log.Warn("Removing old diagnostics failed", "file", e.Name(), "error", err)
			}
		}
	}
}
