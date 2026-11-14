package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/jobspec"
)

// runnerDaemon is the host daemon's calls a runner's life makes.
type runnerDaemon interface {
	Create(ctx context.Context, name string, req *docker.CreateRequest) docker.Result
	Start(ctx context.Context, id string) docker.Result
	InspectContainer(ctx context.Context, ref string) (docker.Container, bool, error)
	InspectSettled(ctx context.Context, name string) (docker.Container, bool, error)
	ListContainers(ctx context.Context) ([]docker.Summary, error)
	StopContainer(ctx context.Context, id string, grace time.Duration) error
	RemoveOwned(ctx context.Context, ref string) error
	Logs(ctx context.Context, id string, n int) ([]string, error)
	CopyArchive(ctx context.Context, id, p string, w io.Writer, limit int64) (bool, error)
	CreateVolume(ctx context.Context, labels map[string]string) (string, error)
	RemoveOwnedVolume(ctx context.Context, name string) error
	Events(ctx context.Context, fn func(containerID string)) error
}

var _ runnerDaemon = (*docker.Client)(nil)

// runnerImages is the image manager's side of a runner start.
type runnerImages interface {
	ForStart(profile string) (string, bool)
	Missing(profile, id string)
	Tidy()
	Status() []hostapi.ProfileStatus
}

// served is what the host's config lets it run. A SIGHUP swaps it whole.
type served struct {
	Spec   *jobspec.Host
	Config *config.Config
	Host   string
	// Profiles are the profiles listing Host whose limits it enforces.
	Profiles []*config.Profile
}

// The waits of a runner's life, vars so tests can shorten them.
var (
	// pendingRetry is how long a create or start whose fate is unknown
	// waits before it is made again.
	pendingRetry = 15 * time.Second
	// eventsRetry is how long a failed events stream waits before it is
	// opened again; listEvery is how often the containers are listed to
	// catch a change the stream missed.
	eventsRetry = 5 * time.Second
	listEvery   = 30 * time.Second
	// freeReads and freeReadGap are the tries at a space hold's baseline,
	// so one failed statfs does not leave the hold without one.
	freeReads   = 3
	freeReadGap = time.Second
)

// spaceMargin is the free bytes past a space hold's baseline that lift it
// with no runner running: room for one runner start, so the few KiB an idle
// host's own writes and removals free never fund a start that fails again.
// A served profile's disk above it raises it.
const spaceMargin = 1 << 30

// subscriberBuffer is how many changes a subscriber may lag by before its
// channel is closed.
const subscriberBuffer = 64

// The removed_by values the host writes.
const (
	removedByDelete = "host: the orchestrator's DELETE"
	removedByFailed = "host: cleanup after a failed start"
)

// errSpace is a start the daemon refused for want of space on the data
// volume. Docker answers it in the error text alone.
var errSpace = errors.New("the host's data volume is full")

// runnerSet is the host's record of the runners it accepted and their
// containers. Its methods are safe for concurrent use.
type runnerSet struct {
	daemon runnerDaemon
	images runnerImages
	// base is the host's run, which each runner's work runs under.
	base context.Context
	// dataFree answers the data volume's free bytes.
	dataFree func() (uint64, error)
	ctrs     map[string]*hostapi.Container
	pins     *pins
	records  map[string]*record
	served   *served
	last     map[string]hostapi.Runner
	subs     map[chan hostapi.Runner]struct{}
	// wrote is the gen at which put or drop last wrote each name, and
	// reading counts the reads made with mu dropped by the gen each began
	// at: a read writes a name only when no write landed after it began,
	// so wrote keeps a name any read in flight may still write.
	wrote   map[string]uint64
	reading map[uint64]int
	// space holds starts after a refusal for space, nil when they are not.
	space *spaceHold
	// draining are the runners started while the host was open, which hold
	// starts once a reload has closed it, until each has ended.
	draining map[string]struct{}
	// closedAt is when the reload that began the drain closed the host.
	closedAt time.Time
	log      *slog.Logger
	work     sync.WaitGroup
	gen      uint64
	mu       sync.Mutex
	// refreshMu serialises refreshes, so a swap's view of wrote is its own.
	refreshMu sync.Mutex
}

// spaceHold is starts held after a start the daemon refused for space.
type spaceHold struct {
	at     time.Time
	detail string
	// free is the data volume's free bytes read once the refused start's
	// container was removed, the baseline a lift is measured from.
	free uint64
	base baseline
}

// baseline is how far a space hold's free-bytes reading got.
type baseline int

const (
	// baselinePending is a hold whose refused start's container may still
	// be on the volume, so no read yet stands for it.
	baselinePending baseline = iota
	baselineRead
	// baselineUnread is a hold no read could measure: the end of a tidy
	// pass with no runner running lifts it.
	baselineUnread
)

// record is one accepted runner: its request, how far the host has taken
// it, and the work in flight for it.
type record struct {
	profile *config.Profile
	cancel  context.CancelFunc
	done    chan struct{}
	volume  string
	rec     hostapi.Record
	// mayCommit is a create whose fate is unknown: the daemon may still
	// commit its container after the call ended.
	mayCommit bool
}

// runnersConfig is what a runnerSet is built from.
type runnersConfig struct {
	Daemon runnerDaemon
	Images runnerImages
	Served *served
	Log    *slog.Logger
	// DataFree answers the data volume's free bytes.
	DataFree func() (uint64, error)
	// CPUs is the host's effective cpuset on a pinning host; nil on a host
	// with CFS quota.
	CPUs []int
}

// newRunners answers an empty record whose runners' work runs under ctx,
// the host's run; load reads the daemon's containers into it and run
// serves it.
func newRunners(ctx context.Context, cfg *runnersConfig) *runnerSet {
	r := &runnerSet{
		daemon: cfg.Daemon, images: cfg.Images, served: cfg.Served, log: cfg.Log, dataFree: cfg.DataFree,
		records: map[string]*record{}, ctrs: map[string]*hostapi.Container{},
		last: map[string]hostapi.Runner{}, subs: map[chan hostapi.Runner]struct{}{},
		wrote: map[string]uint64{}, reading: map[uint64]int{}, draining: map[string]struct{}{}, base: ctx,
	}
	if cfg.CPUs != nil {
		r.pins = newPins(cfg.CPUs)
	}
	return r
}

// setServed swaps what the host serves; runners already accepted keep the
// profile they were accepted with. A swap that closes an open host drains
// it: every runner then busy holds its starts until each has ended, since
// it may run with the open host's grants. A swap that opens it ends the
// drain.
func (r *runnerSet) setServed(s *served) {
	r.mu.Lock()
	defer r.mu.Unlock()
	was := r.served.open()
	r.served = s
	switch {
	case s.open():
		clear(r.draining)
	case was:
		r.closedAt = time.Now()
		for _, name := range r.names() {
			if r.busy(name) {
				r.draining[name] = struct{}{}
			}
		}
		if len(r.draining) > 0 {
			r.log.Warn("The host is no longer open; it takes no start until the runners started while it was open have ended", "runners", r.drainNames())
		}
	}
}

func (s *served) open() bool {
	h, ok := s.Config.Host(s.Host)
	return ok && h.Egress.Open
}

// names answers every runner the host records or has a container of;
// r.mu is held.
func (r *runnerSet) names() []string {
	names := slices.Collect(maps.Keys(r.ctrs))
	for n := range r.records {
		if r.ctrs[n] == nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

func (r *runnerSet) drainNames() string {
	return strings.Join(slices.Sorted(maps.Keys(r.draining)), ", ")
}

// drainDetail says what the drain waits for; r.mu is held.
func (r *runnerSet) drainDetail() string {
	return fmt.Sprintf("the host stopped being open at %s; starts go on once the runners started while it was open have exited or been removed: %s",
		r.closedAt.UTC().Format(time.RFC3339), r.drainNames())
}

// load reads the daemon's labelled runner containers, so a supervisor
// restart lists them and keeps the CPUs they were pinned to.
func (r *runnerSet) load(ctx context.Context) error {
	if err := r.refresh(ctx); err != nil {
		return fmt.Errorf("listing the host's runner containers: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pins != nil {
		for name, c := range r.ctrs {
			if cpus, ok := parseCPUSet(c.Limits.CPUSet); ok && holdsCPUs(c.State) {
				r.pins.hold(name, cpus)
			}
		}
	}
	for name := range r.ctrs {
		r.last[name], _ = r.listed(name)
	}
	return nil
}

// run follows the daemon's exits until ctx ends, then waits for every
// runner's work, which ends with the context newRunners was given.
func (r *runnerSet) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { r.follow(ctx) })
	wg.Go(func() { r.poll(ctx) })
	wg.Wait()
	r.work.Wait()
}

func (r *runnerSet) follow(ctx context.Context) {
	for ctx.Err() == nil {
		err := r.daemon.Events(ctx, func(id string) { r.exited(ctx, id) })
		if ctx.Err() != nil {
			return
		}
		r.log.Warn("The host daemon's events stream ended; it is opened again", "error", err)
		if !sleep(ctx, eventsRetry) {
			return
		}
		r.sync(ctx)
	}
}

func (r *runnerSet) poll(ctx context.Context) {
	t := time.NewTicker(listEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sync(ctx)
		}
	}
}

// exited reads the container whose die event arrived, then publishes the
// change and releases what its runner held.
func (r *runnerSet) exited(ctx context.Context, id string) {
	begun := r.beginRead()
	ctr, found, err := r.daemon.InspectContainer(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.endRead(begun)
	if err != nil {
		r.log.Warn("Reading an exited runner's container failed; the next list reads it", "container", id, "error", err)
		return
	}
	c, ok := fromDocker(&ctr)
	if !found || !ok || !r.putNewer(c, begun) {
		return
	}
	r.liftOnExit()
	r.changed(c.Name)
}

// put and drop write runner name's container view, newer than any read
// begun before; r.mu is held.
func (r *runnerSet) put(c *hostapi.Container) {
	r.gen++
	r.wrote[c.Name], r.ctrs[c.Name] = r.gen, c
}

func (r *runnerSet) drop(name string) {
	r.gen++
	r.wrote[name] = r.gen
	delete(r.ctrs, name)
}

// putNewer puts c, read by a read begun at gen begun, unless a write for
// its name landed since; r.mu is held. A later stage of the same container
// wins either way, since a runner's container never runs again once it
// has exited (no restart policy).
func (r *runnerSet) putNewer(c *hostapi.Container, begun uint64) bool {
	cur := r.ctrs[c.Name]
	if later(cur, c) || (!later(c, cur) && r.wrote[c.Name] > begun) {
		return false
	}
	r.put(c)
	return true
}

// later reports a and b views of the same container, a further on in its
// life: created, then running, then ended.
func later(a, b *hostapi.Container) bool {
	return a != nil && b != nil && a.ID == b.ID && stage(a.State) > stage(b.State)
}

func stage(state string) int {
	switch {
	case state == docker.StateCreated:
		return 0
	case holdsCPUs(state):
		return 1
	}
	return 2
}

// beginRead marks a read of the daemon made with r.mu dropped, answering
// the gen it began at for its endRead.
func (r *runnerSet) beginRead() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reading[r.gen]++
	return r.gen
}

// endRead ends the read begun at begun and forgets each name in neither the
// view nor the record whose last write no read in flight began before;
// r.mu is held.
func (r *runnerSet) endRead(begun uint64) {
	r.reading[begun]--
	if r.reading[begun] == 0 {
		delete(r.reading, begun)
	}
	oldest := r.gen
	for g := range r.reading {
		oldest = min(oldest, g)
	}
	for name, at := range r.wrote {
		if at <= oldest && r.ctrs[name] == nil && r.records[name] == nil {
			delete(r.wrote, name)
		}
	}
}

// sync lists the containers again, publishes each change and checks
// whether the space hold's wait is over.
func (r *runnerSet) sync(ctx context.Context) {
	if err := r.refresh(ctx); err != nil {
		r.log.Warn("Listing the host's runner containers failed", "error", err)
		return
	}
	r.publishAll()
	r.checkFreed(false)
}

func (r *runnerSet) publishAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liftOnExit()
	names := map[string]bool{}
	for n := range r.ctrs {
		names[n] = true
	}
	for n := range r.records {
		names[n] = true
	}
	for n := range r.last {
		names[n] = true
	}
	for n := range names {
		r.changed(n)
	}
}

// refresh replaces the container view with the daemon's list, inspecting
// only containers that are new or changed state since the last list. A name
// put or dropped after the list began keeps that newer view.
func (r *runnerSet) refresh(ctx context.Context) error {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	r.mu.Lock()
	old := maps.Clone(r.ctrs)
	r.mu.Unlock()
	begun := r.beginRead()
	now, err := r.read(ctx, old)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.endRead(begun)
	if err != nil {
		return err
	}
	r.swap(now, begun)
	return nil
}

// read lists the labelled runner containers, inspecting each that is not
// in old with the same ID and state.
func (r *runnerSet) read(ctx context.Context, old map[string]*hostapi.Container) (map[string]*hostapi.Container, error) {
	list, err := r.daemon.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	now := map[string]*hostapi.Container{}
	for i := range list {
		s := &list[i]
		name := s.Labels[jobspec.LabelRunner]
		if name == "" {
			continue
		}
		if c := old[name]; c != nil && c.ID == s.ID && c.State == s.State {
			now[name] = c
			continue
		}
		ctr, found, err := r.daemon.InspectContainer(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		if c, ok := fromDocker(&ctr); found && ok {
			now[name] = c
		}
	}
	return now, nil
}

// swap makes now the container view, keeping the current view of each name
// written after gen begun unless now holds a later stage of it; r.mu is
// held.
func (r *runnerSet) swap(now map[string]*hostapi.Container, begun uint64) {
	for name, at := range r.wrote {
		if at <= begun || later(now[name], r.ctrs[name]) {
			continue
		}
		if c := r.ctrs[name]; c != nil {
			now[name] = c
		} else {
			delete(now, name)
		}
	}
	r.ctrs = now
}

// fromDocker is a labelled runner container as a list reads it; false for
// a container without the runner label.
func fromDocker(ctr *docker.Container) (*hostapi.Container, bool) {
	l := ctr.Config.Labels
	name := l[jobspec.LabelRunner]
	if name == "" {
		return nil, false
	}
	id, _ := strconv.ParseInt(l[jobspec.LabelRunnerID], 10, 64)
	disk, _ := ctr.DiskBytes()
	return &hostapi.Container{
		ID: ctr.ID, Name: name, Profile: l[jobspec.LabelProfile], Scope: l[jobspec.LabelScope], RunnerID: id,
		State: ctr.State.Status, StartedAt: ctr.State.StartedAt, FinishedAt: ctr.State.FinishedAt,
		ExitCode: ctr.State.ExitCode, OOMKilled: ctr.State.OOMKilled,
		Limits: hostapi.Limits{
			CPUSet: ctr.HostConfig.CpusetCpus, MilliCPUs: ctr.HostConfig.NanoCPUs / 1_000_000,
			Memory: ctr.HostConfig.Memory, Disk: disk,
		},
	}, true
}

// liftOnExit lifts the space hold once a runner's container has exited
// since the hold began; r.mu is held.
func (r *runnerSet) liftOnExit() {
	if r.space == nil {
		return
	}
	for _, c := range r.ctrs {
		if !holdsCPUs(c.State) && c.FinishedAt.After(r.space.at) {
			r.lift("a runner exited")
			return
		}
	}
}

// lift ends the space hold; r.mu is held.
func (r *runnerSet) lift(why string) {
	r.log.Info("The starts held for space go on", "why", why)
	r.space = nil
}

// holdForSpace holds starts after a refusal for space; measure then gives
// the hold its baseline.
func (r *runnerSet) holdForSpace() *spaceHold {
	at := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	h := &spaceHold{at: at, detail: fmt.Sprintf("the data volume %s was full at %s; starts go on once a runner exits, once a runner's container is removed, "+
		"or once a tidy pass or a list finds %d MiB more free space than after the refused start's container was removed, with no runner running",
		dataRoot, at.UTC().Format(time.RFC3339), r.spaceMargin()>>20)}
	r.space = h
	return h
}

// measure reads hold h's baseline, trying freeReads times, then asks for
// a tidy pass. Run once the refused start's container is gone, so its
// removal never counts as space a pass freed.
func (r *runnerSet) measure(h *spaceHold) {
	base := baselineUnread
	var free uint64
	for i := range freeReads {
		var err error
		if free, err = r.dataFree(); err == nil {
			base = baselineRead
			break
		}
		r.log.Warn("Reading the data volume's free space failed", "error", err)
		if i < freeReads-1 && !sleep(r.base, freeReadGap) {
			break
		}
	}
	if base == baselineUnread {
		r.log.Warn("The space hold has no free-space reading; the next tidy pass that ends with no runner running lifts it")
	}
	r.mu.Lock()
	h.free, h.base = free, base
	r.mu.Unlock()
	r.images.Tidy()
}

// spaceMargin is the lift's margin for what the host serves now; r.mu is
// held.
func (r *runnerSet) spaceMargin() uint64 {
	m := uint64(spaceMargin)
	for _, p := range r.served.Profiles {
		if p.Disk > 0 {
			m = max(m, uint64(p.Disk))
		}
	}
	return m
}

// tidied lists the containers after a tidy pass, then checks whether the
// pass freed what the space hold waits for.
func (r *runnerSet) tidied() {
	r.mu.Lock()
	held := r.space != nil
	r.mu.Unlock()
	if !held {
		return
	}
	if err := r.refresh(r.base); err != nil {
		r.log.Warn("Listing the host's runner containers after the tidy pass failed; the last list decides", "error", err)
	}
	r.checkFreed(true)
}

// checkFreed lifts the space hold once no runner container runs and the
// data volume has spaceMargin more free bytes than its baseline, since no
// exit is then left to lift it: a tidy pass freed space or the volume grew.
// afterPass is a tidy pass's end, which alone lifts a hold no read measured.
func (r *runnerSet) checkFreed(afterPass bool) {
	r.mu.Lock()
	h := r.space
	r.mu.Unlock()
	if h == nil {
		return
	}
	free, err := r.dataFree()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.space != h || r.anyRunning() {
		return
	}
	switch h.base {
	case baselineUnread:
		if afterPass {
			r.lift("a tidy pass ended with no runner running, and the data volume's free space was never read")
		}
	case baselineRead:
		if err != nil {
			r.log.Warn("Reading the data volume's free space failed; the space hold stands", "error", err)
		} else if free >= h.free+r.spaceMargin() {
			r.lift("the data volume has more free space, with no runner running")
		}
	}
}

// anyRunning reports a runner container still running; r.mu is held.
func (r *runnerSet) anyRunning() bool {
	for _, c := range r.ctrs {
		switch c.State {
		case docker.StateRunning, docker.StateRestarting, docker.StatePaused:
			return true
		}
	}
	return false
}

// held answers why the host takes no start now, "" for none: a drain after
// the host was closed, starts held for space, or both; r.mu is held.
func (r *runnerSet) held() (reason hostapi.Reason, detail string) {
	var why []string
	if len(r.draining) > 0 {
		reason = hostapi.ReasonOpenDraining
		why = append(why, r.drainDetail())
	}
	if r.space != nil {
		if reason == "" {
			reason = hostapi.ReasonDataFull
		}
		why = append(why, r.space.detail)
	}
	return reason, strings.Join(why, "; ")
}

// profileStatus is each profile's readiness: the image manager's, with
// every profile not ready while the host takes no start, a profile already
// not ready keeping its own reason after the hold's.
func (r *runnerSet) profileStatus() []hostapi.ProfileStatus {
	st := r.images.Status()
	r.mu.Lock()
	reason, detail := r.held()
	r.mu.Unlock()
	if reason == "" {
		return st
	}
	for i := range st {
		d := detail
		if !st[i].Ready {
			d += "; and " + string(st[i].Reason) + ": " + st[i].Detail
		}
		st[i].Ready, st[i].Reason, st[i].Detail = false, reason, d
	}
	return st
}

// holdsCPUs reports a container state that still holds its pinned CPUs.
func holdsCPUs(state string) bool {
	switch state {
	case docker.StateExited, docker.StateDead, docker.StateRemoving:
		return false
	}
	return true
}

// listed is runner name as a list shows it; r.mu is held.
func (r *runnerSet) listed(name string) (hostapi.Runner, bool) {
	var rec *hostapi.Record
	if rr := r.records[name]; rr != nil {
		rec = &rr.rec
	}
	return hostapi.State(r.ctrs[name], rec)
}

// changed publishes runner name when its listed state moved since the last
// publish, and releases its CPUs and its part in a drain once it is no
// longer busy; r.mu is held.
func (r *runnerSet) changed(name string) {
	run, ok := r.listed(name)
	if !r.busy(name) {
		if r.pins != nil {
			r.pins.release(name)
		}
		r.drained(name)
	}
	if !ok {
		delete(r.last, name)
		return
	}
	if prev, seen := r.last[name]; seen && sameState(&prev, &run) {
		return
	}
	r.last[name] = run
	for ch := range r.subs {
		select {
		case ch <- run:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func sameState(a, b *hostapi.Runner) bool {
	return a.State == b.State && a.Reason == b.Reason && a.OOMKilled == b.OOMKilled &&
		(a.ExitCode == nil) == (b.ExitCode == nil) && (a.ExitCode == nil || *a.ExitCode == *b.ExitCode)
}

// busy reports runner name's work in flight or a container of it that
// still holds its CPUs; r.mu is held.
func (r *runnerSet) busy(name string) bool {
	if rr := r.records[name]; rr != nil && rr.done != nil {
		select {
		case <-rr.done:
		default:
			return true
		}
	}
	c := r.ctrs[name]
	return c != nil && holdsCPUs(c.State)
}

// drained takes runner name, no longer busy, out of the drain, ending it
// with the last; r.mu is held.
func (r *runnerSet) drained(name string) {
	if _, ok := r.draining[name]; !ok {
		return
	}
	delete(r.draining, name)
	if len(r.draining) == 0 {
		r.log.Info("The runners started while the host was open have ended; it takes starts again")
	}
}

// subscribe sends each runner whose listed state changes until ctx ends,
// then closes the channel; a subscriber more than subscriberBuffer changes
// behind has its channel closed early.
func (r *runnerSet) subscribe(ctx context.Context) <-chan hostapi.Runner {
	ch := make(chan hostapi.Runner, subscriberBuffer)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	context.AfterFunc(ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	})
	return ch
}

// list answers every runner the host lists, by name, from a fresh read of
// the daemon's containers.
func (r *runnerSet) list(ctx context.Context) ([]hostapi.Runner, error) {
	if err := r.refresh(ctx); err != nil {
		return nil, fmt.Errorf("listing the host's runner containers: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.liftOnExit()
	names := r.names()
	out := make([]hostapi.Runner, 0, len(names))
	for _, n := range names {
		r.changed(n)
		if run, ok := r.listed(n); ok {
			out = append(out, run)
		}
	}
	return out, nil
}

// holds reports a runner the host still records, whose docker volume the
// tidy pass keeps.
func (r *runnerSet) holds(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.records[name] != nil
}

// accept accepts req: it records the runner and answers at once, then
// creates and starts its container in the background. A runner the host
// already holds answers as listed and starts nothing.
func (r *runnerSet) accept(_ context.Context, req *hostapi.RunnerRequest) (hostapi.Runner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, held := r.records[req.RunnerName]; held || r.ctrs[req.RunnerName] != nil {
		run, _ := r.listed(req.RunnerName)
		return run, nil
	}
	p, err := r.admit(req.Profile)
	if err != nil {
		return hostapi.Runner{}, err
	}
	if reason, detail := r.held(); reason != "" {
		return hostapi.Runner{}, &hostapi.NotReadyError{Profile: p.Name, Reason: reason, Detail: detail}
	}
	imageID, ready := r.images.ForStart(p.Name)
	if !ready {
		return hostapi.Runner{}, r.notReady(p.Name)
	}
	rr := &record{profile: p, rec: hostapi.Record{Request: *req, State: hostapi.RecordCreating}}
	rr.rec.Request.JITConfig = ""
	r.records[req.RunnerName] = rr
	var cpus []int
	if r.pins != nil {
		var ok bool
		if cpus, ok = r.pins.take(req.RunnerName, int(p.MilliCPUs/1000)); !ok {
			r.failRecord(rr, fmt.Sprintf("the host has %d free CPUs of its cpuset, and profile %s needs %d", r.pins.free(), p.Name, p.MilliCPUs/1000))
		}
	}
	rr.rec.Limits = hostapi.Limits{Memory: p.Memory, Disk: p.Disk}
	if cpus != nil {
		rr.rec.Limits.CPUSet = formatCPUSet(cpus)
	} else if r.pins == nil {
		rr.rec.Limits.MilliCPUs = p.MilliCPUs
	}
	if rr.rec.State != hostapi.RecordFailed {
		ctx, cancel := context.WithCancel(r.base)
		rr.cancel, rr.done = cancel, make(chan struct{})
		spec := r.served.Spec
		jit := req.JITConfig
		r.work.Go(func() {
			r.build(ctx, rr, spec, &jobspec.Runner{
				Scope: req.Scope, Name: req.RunnerName, ImageID: imageID, JITConfig: jit, CPUs: cpus, ID: req.RunnerID,
			})
			cancel()
			r.finished(rr)
		})
	}
	r.changed(req.RunnerName)
	run, _ := r.listed(req.RunnerName)
	return run, nil
}

// finished ends rr's work: with done closed, a runner that failed frees
// its CPUs now rather than at the next list.
func (r *runnerSet) finished(rr *record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	close(rr.done)
	if name := rr.rec.Request.RunnerName; r.records[name] == rr {
		r.changed(name)
	}
}

// admit answers the profile a request names, refused when the config does
// not define it, does not list this host for it or asks a limit the host
// cannot enforce; r.mu is held.
func (r *runnerSet) admit(name string) (*config.Profile, error) {
	p, ok := r.served.Config.Profile(name)
	switch {
	case !ok:
		return nil, fmt.Errorf("%w: %s", hostapi.ErrUnknownProfile, name)
	case !slices.Contains(p.Hosts, r.served.Host):
		return nil, fmt.Errorf("%w: profile %s does not list host %s", hostapi.ErrProfileRefused, name, r.served.Host)
	}
	if err := r.served.Spec.Check(p); err != nil {
		return nil, fmt.Errorf("%w: %w", hostapi.ErrProfileRefused, err)
	}
	return p, nil
}

func (r *runnerSet) notReady(profile string) error {
	st := r.images.Status()
	for i := range st {
		if st[i].Name == profile {
			return &hostapi.NotReadyError{Profile: profile, Reason: st[i].Reason, Detail: st[i].Detail}
		}
	}
	return &hostapi.NotReadyError{Profile: profile, Reason: hostapi.ReasonPulling}
}

// failRecord fails rr with reason; r.mu is held.
func (r *runnerSet) failRecord(rr *record, reason string) {
	rr.rec.State, rr.rec.Reason = hostapi.RecordFailed, reason
	r.log.Warn("A runner failed to start", "runner", rr.rec.Request.RunnerName, "profile", rr.profile.Name, "reason", reason)
}

// setState moves rr on and publishes it, unless its DELETE dropped it.
func (r *runnerSet) setState(rr *record, state hostapi.RecordState, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if state == hostapi.RecordFailed {
		r.failRecord(rr, reason)
	} else {
		rr.rec.State = state
	}
	if r.records[rr.rec.Request.RunnerName] == rr {
		r.changed(rr.rec.Request.RunnerName)
	}
}

// build creates and starts rr's container. A call whose fate is unknown is
// made again after pendingRetry and never removes the container meanwhile;
// ctx ending (the host stopping, or the runner's DELETE) leaves it pending.
func (r *runnerSet) build(ctx context.Context, rr *record, spec *jobspec.Host, run *jobspec.Runner) {
	if rr.profile.RunnerImage == config.ShippedDocker {
		vol, err := r.daemon.CreateVolume(ctx, map[string]string{jobspec.LabelRunner: run.Name})
		if err != nil {
			r.setState(rr, hostapi.RecordFailed, "creating its docker volume: "+err.Error())
			return
		}
		r.mu.Lock()
		rr.volume = vol
		r.mu.Unlock()
		run.DockerVolume = vol
	}
	req, err := spec.Runner(rr.profile, run)
	if err != nil {
		r.setState(rr, hostapi.RecordFailed, err.Error())
		return
	}
	id, ok := r.create(ctx, rr, req, run)
	if !ok {
		return
	}
	r.setState(rr, hostapi.RecordStarting, "")
	for res := r.daemon.Start(ctx, id); ; res = r.daemon.Start(ctx, id) {
		switch res.Outcome {
		case docker.Done:
			// The container may have exited already; a failed read is the
			// next list's.
			_, _, _ = r.inspect(ctx, id)
			r.setState(rr, hostapi.RecordStarted, "")
			return
		case docker.Failed:
			r.failed(ctx, rr, res, "start")
			return
		}
		r.log.Warn("A runner's start has not answered; it is issued again if its container has not started", "runner", run.Name, "error", res.Err)
		if !r.awaitStart(ctx, rr, id) {
			return
		}
	}
}

// awaitStart waits pendingRetry after a start whose fate is unknown, then
// reads the container, answering true only for one still created, the one
// state a start may be issued in again: the daemon starts any stopped
// container, so a runner that ran and exited meanwhile would run again on
// its spent JIT config. Any other state records the start done and a
// container gone fails it, both answering false, as does ctx ending.
func (r *runnerSet) awaitStart(ctx context.Context, rr *record, id string) bool {
	for sleep(ctx, pendingRetry) {
		state, found, err := r.inspect(ctx, id)
		switch {
		case err != nil:
			r.log.Warn("Reading a runner's container after a start that has not answered failed; it is read again", "runner", rr.rec.Request.RunnerName, "error", err)
		case !found:
			r.mu.Lock()
			r.drop(rr.rec.Request.RunnerName)
			r.mu.Unlock()
			r.setState(rr, hostapi.RecordFailed, "the start failed: the container is gone")
			return false
		case state != docker.StateCreated:
			r.setState(rr, hostapi.RecordStarted, "")
			return false
		default:
			return true
		}
	}
	return false
}

// create makes rr's container until the daemon answers, answering its ID,
// or false once rr failed or ctx ended.
func (r *runnerSet) create(ctx context.Context, rr *record, req *docker.CreateRequest, run *jobspec.Runner) (string, bool) {
	for {
		res := r.daemon.Create(ctx, run.Name, req)
		r.mu.Lock()
		rr.mayCommit = res.Outcome == docker.Pending
		r.mu.Unlock()
		switch res.Outcome {
		case docker.Done:
			return res.ID, true
		case docker.Failed:
			if errors.Is(res.Err, docker.ErrImageMissing) {
				r.images.Missing(rr.profile.Name, run.ImageID)
			}
			r.failed(ctx, rr, res, "create")
			return "", false
		}
		r.log.Warn("A runner's create has not answered; it is made again", "runner", run.Name, "error", res.Err)
		if !sleep(ctx, pendingRetry) {
			return "", false
		}
	}
}

// inspect reads container id into the view unless a newer write landed,
// answering its Docker state.
func (r *runnerSet) inspect(ctx context.Context, id string) (state string, found bool, err error) {
	begun := r.beginRead()
	ctr, found, err := r.daemon.InspectContainer(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.endRead(begun)
	if err != nil || !found {
		return "", false, err
	}
	if c, ok := fromDocker(&ctr); ok {
		r.putNewer(c, begun)
	}
	return ctr.State.Status, true, nil
}

// failed fails rr for a create or start the daemon refused, removing any
// container the call left; a refusal for space holds further starts and
// runs the tidy pass at once. The record fails before the removal, so a
// list in between shows the container as its orphan, never as exited.
func (r *runnerSet) failed(ctx context.Context, rr *record, res docker.Result, call string) {
	err := res.Err
	var hold *spaceHold
	if isNoSpace(err) {
		err = fmt.Errorf("%w (%s): %w", errSpace, dataRoot, err)
		hold = r.holdForSpace()
	}
	r.setState(rr, hostapi.RecordFailed, "the "+call+" failed: "+err.Error())
	if res.ID != "" {
		r.removeFailed(ctx, rr, res.ID)
	}
	if hold != nil {
		r.measure(hold)
	}
}

// removeFailed removes failed runner rr's container id and drops its view,
// so the end of rr's work releases what a list of it held; a container
// left keeps its view.
func (r *runnerSet) removeFailed(ctx context.Context, rr *record, id string) {
	name := rr.rec.Request.RunnerName
	r.mu.Lock()
	rr.rec.RemovedBy = removedByFailed
	r.mu.Unlock()
	if err := r.daemon.RemoveOwned(ctx, id); err != nil {
		r.log.Warn("Removing a failed runner's container failed; it is listed as an orphan", "runner", name, "error", err)
		return
	}
	r.mu.Lock()
	r.drop(name)
	r.mu.Unlock()
}

// isNoSpace reports a daemon refusal for want of space, which Docker
// answers only in its error text.
func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no space left on device") || strings.Contains(s, "disk quota exceeded")
}

// remove stops and removes runner name's container, first copying its
// _diag folder as a tar when diag is set, and drops its record. Work in
// flight for it is cut first, and a create it cut that may still commit is
// waited out (docker.Client.InspectSettled). A runner with no container is
// no error.
func (r *runnerSet) remove(ctx context.Context, name string, diag bool) ([]byte, error) {
	r.mu.Lock()
	rr := r.records[name]
	vol := ""
	if rr != nil {
		rr.rec.RemovedBy, vol = removedByDelete, rr.volume
	}
	r.mu.Unlock()
	settle := false
	if rr != nil && rr.cancel != nil {
		rr.cancel()
		<-rr.done
		r.mu.Lock()
		vol, settle = rr.volume, rr.mayCommit
		r.mu.Unlock()
	}
	tar, removed, err := r.removeContainer(ctx, name, rr, diag, settle)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	delete(r.records, name)
	r.drop(name)
	if removed && r.space != nil {
		r.lift("a runner's container was removed")
	}
	r.changed(name)
	r.mu.Unlock()
	if vol != "" {
		if err := r.daemon.RemoveOwnedVolume(ctx, vol); err != nil {
			r.log.Warn("Removing a deleted runner's docker volume failed; the tidy pass removes it", "runner", name, "error", err)
		}
	}
	return tar, nil
}

// removeContainer stops and removes runner name's labelled container, if
// any, answering its _diag tar first when diag is set and whether it
// removed one; settle inspects as after a create that may still commit.
func (r *runnerSet) removeContainer(ctx context.Context, name string, rr *record, diag, settle bool) (tar []byte, removed bool, err error) {
	inspect := r.daemon.InspectContainer
	if settle {
		inspect = r.daemon.InspectSettled
	}
	ctr, found, err := inspect(ctx, name)
	if err != nil || !found || ctr.Config.Labels[jobspec.LabelRunner] != name {
		return nil, false, err
	}
	if diag {
		tar = r.copyDiag(ctx, &ctr, rr)
	}
	if err := r.daemon.StopContainer(ctx, ctr.ID, hostapi.DeleteStopGrace); err != nil {
		return nil, false, err
	}
	if err := r.daemon.RemoveOwned(ctx, ctr.ID); err != nil {
		return nil, false, err
	}
	return tar, true, nil
}

// copyDiag answers the runner's _diag tar, nil when it has none or it
// passes hostapi.DiagLimit.
func (r *runnerSet) copyDiag(ctx context.Context, ctr *docker.Container, rr *record) []byte {
	var buf bytes.Buffer
	found, err := r.daemon.CopyArchive(ctx, ctr.ID, r.diagDir(ctr, rr), &buf, hostapi.DiagLimit)
	if err != nil {
		r.log.Warn("Copying a deleted runner's _diag folder failed", "runner", ctr.Config.Labels[jobspec.LabelRunner], "error", err)
		return nil
	}
	if !found {
		return nil
	}
	return buf.Bytes()
}

// diagDir is where the runner keeps its own logs: _diag beside the runner's
// start script, which the docker image's wrapper runs from /home/runner.
func (r *runnerSet) diagDir(ctr *docker.Container, rr *record) string {
	var p *config.Profile
	if rr != nil {
		p = rr.profile
	} else {
		r.mu.Lock()
		p, _ = r.served.Config.Profile(ctr.Config.Labels[jobspec.LabelProfile])
		r.mu.Unlock()
	}
	if p == nil || len(p.Command) == 0 || p.Command[0] == config.DockerWrapper {
		return defaultDiagDir
	}
	return path.Join(path.Dir(p.Command[0]), "_diag")
}

// defaultDiagDir is GitHub's runner image's own log directory.
const defaultDiagDir = "/home/runner/_diag"

// logs answers the last tail lines of runner name's log.
func (r *runnerSet) logs(ctx context.Context, name string, tail int) ([]byte, error) {
	ctr, err := r.container(ctx, name)
	if err != nil {
		return nil, err
	}
	lines, err := r.daemon.Logs(ctx, ctr.ID, tail)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return []byte{}, nil
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

// diag answers runner name's _diag tar, empty when it has none.
func (r *runnerSet) diag(ctx context.Context, name string) ([]byte, error) {
	ctr, err := r.container(ctx, name)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	rr := r.records[name]
	r.mu.Unlock()
	var buf bytes.Buffer
	if _, err := r.daemon.CopyArchive(ctx, ctr.ID, r.diagDir(&ctr, rr), &buf, hostapi.DiagLimit); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// container is runner name's labelled container, or ErrUnknownRunner.
func (r *runnerSet) container(ctx context.Context, name string) (docker.Container, error) {
	ctr, found, err := r.daemon.InspectContainer(ctx, name)
	switch {
	case err != nil:
		return docker.Container{}, err
	case !found || ctr.Config.Labels[jobspec.LabelRunner] != name:
		return docker.Container{}, fmt.Errorf("%w: %s", hostapi.ErrUnknownRunner, name)
	}
	return ctr, nil
}

// sleep waits d, answering false when ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
