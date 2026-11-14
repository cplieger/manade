package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
)

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
	// base is run's context, which each runner's work runs under.
	base    context.Context
	ctrs    map[string]*hostapi.Container
	pins    *pins
	records map[string]*record
	served  *served
	last    map[string]hostapi.Runner
	subs    map[chan hostapi.Runner]struct{}
	log     *slog.Logger
	// spaceHeld says why starts are held after a refusal for space, until
	// a runner exits; "" when they are not.
	spaceHeld string
	work      sync.WaitGroup
	mu        sync.Mutex
}

// record is one accepted runner: its request, how far the host has taken
// it, and the work in flight for it.
type record struct {
	profile *config.Profile
	cancel  context.CancelFunc
	done    chan struct{}
	volume  string
	rec     hostapi.Record
}

// runnersConfig is what a runnerSet is built from.
type runnersConfig struct {
	Daemon runnerDaemon
	Images runnerImages
	Served *served
	Log    *slog.Logger
	// CPUs is the host's effective cpuset on a pinning host; nil on a host
	// with CFS quota.
	CPUs []int
}

// newRunners answers an empty record; load reads the daemon's containers
// into it and run serves it.
func newRunners(cfg *runnersConfig) *runnerSet {
	r := &runnerSet{
		daemon: cfg.Daemon, images: cfg.Images, served: cfg.Served, log: cfg.Log,
		records: map[string]*record{}, ctrs: map[string]*hostapi.Container{},
		last: map[string]hostapi.Runner{}, subs: map[chan hostapi.Runner]struct{}{},
		base: context.Background(),
	}
	if cfg.CPUs != nil {
		r.pins = newPins(cfg.CPUs)
	}
	return r
}

// setServed swaps what the host serves; runners already accepted keep the
// profile they were accepted with.
func (r *runnerSet) setServed(s *served) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.served = s
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
// runner's work it started; accept's work runs under ctx.
func (r *runnerSet) run(ctx context.Context) {
	r.mu.Lock()
	r.base = ctx
	r.mu.Unlock()
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
	ctr, found, err := r.daemon.InspectContainer(ctx, id)
	if err != nil {
		r.log.Warn("Reading an exited runner's container failed; the next list reads it", "container", id, "error", err)
		return
	}
	c, ok := fromDocker(&ctr)
	if !found || !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spaceHeld != "" && !holdsCPUs(c.State) {
		r.log.Info("A runner exited, so the starts held for space go on")
		r.spaceHeld = ""
	}
	r.ctrs[c.Name] = c
	r.changed(c.Name)
}

// sync lists the containers again and publishes each change.
func (r *runnerSet) sync(ctx context.Context) {
	if err := r.refresh(ctx); err != nil {
		r.log.Warn("Listing the host's runner containers failed", "error", err)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
// only containers that are new or changed state since the last list.
func (r *runnerSet) refresh(ctx context.Context) error {
	list, err := r.daemon.ListContainers(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	old := r.ctrs
	r.mu.Unlock()
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
			return err
		}
		if c, ok := fromDocker(&ctr); found && ok {
			now[name] = c
		}
	}
	r.mu.Lock()
	r.ctrs = now
	r.mu.Unlock()
	return nil
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
// publish, and releases its CPUs once it no longer holds them; r.mu is held.
func (r *runnerSet) changed(name string) {
	run, ok := r.listed(name)
	r.release(name)
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

// release frees runner name's CPUs once neither pending work nor a live
// container holds them; r.mu is held.
func (r *runnerSet) release(name string) {
	if r.pins == nil {
		return
	}
	if rr := r.records[name]; rr != nil && rr.done != nil {
		select {
		case <-rr.done:
		default:
			return
		}
	}
	if c := r.ctrs[name]; c != nil && holdsCPUs(c.State) {
		return
	}
	r.pins.release(name)
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
	names := make([]string, 0, len(r.ctrs)+len(r.records))
	for n := range r.ctrs {
		names = append(names, n)
	}
	for n := range r.records {
		if r.ctrs[n] == nil {
			names = append(names, n)
		}
	}
	slices.Sort(names)
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
	imageID, ready := r.images.ForStart(p.Name)
	if !ready {
		return hostapi.Runner{}, r.notReady(p.Name)
	}
	rr := &record{profile: p, rec: hostapi.Record{Request: *req, State: hostapi.RecordCreating}}
	rr.rec.Request.JITConfig = ""
	r.records[req.RunnerName] = rr
	var cpus []int
	switch {
	case r.spaceHeld != "":
		r.failRecord(rr, "starts are held: "+r.spaceHeld+"; they go on once a runner exits")
	case r.pins != nil:
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
			defer close(rr.done)
			defer cancel()
			r.build(ctx, rr, spec, &jobspec.Runner{
				Scope: req.Scope, Name: req.RunnerName, ImageID: imageID, JITConfig: jit, CPUs: cpus, ID: req.RunnerID,
			})
		})
	}
	r.changed(req.RunnerName)
	run, _ := r.listed(req.RunnerName)
	return run, nil
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
	for {
		res := r.daemon.Start(ctx, id)
		switch res.Outcome {
		case docker.Done:
			r.started(ctx, rr, id)
			return
		case docker.Failed:
			r.failed(ctx, rr, res, "start")
			return
		}
		r.log.Warn("A runner's start has not answered; it is issued again", "runner", run.Name, "error", res.Err)
		if !sleep(ctx, pendingRetry) {
			return
		}
	}
}

// create makes rr's container until the daemon answers, answering its ID,
// or false once rr failed or ctx ended.
func (r *runnerSet) create(ctx context.Context, rr *record, req *docker.CreateRequest, run *jobspec.Runner) (string, bool) {
	for {
		res := r.daemon.Create(ctx, run.Name, req)
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

// started records rr started and reads its container, which may have
// exited already.
func (r *runnerSet) started(ctx context.Context, rr *record, id string) {
	ctr, found, err := r.daemon.InspectContainer(ctx, id)
	r.mu.Lock()
	if err == nil && found {
		if c, ok := fromDocker(&ctr); ok {
			r.ctrs[c.Name] = c
		}
	}
	r.mu.Unlock()
	r.setState(rr, hostapi.RecordStarted, "")
}

// failed fails rr for a create or start the daemon refused, removing any
// container the call left; a refusal for space holds further starts and
// runs the tidy pass at once.
func (r *runnerSet) failed(ctx context.Context, rr *record, res docker.Result, call string) {
	err := res.Err
	if isNoSpace(err) {
		err = fmt.Errorf("%w (%s): %w", errSpace, dataRoot, err)
		r.mu.Lock()
		r.spaceHeld = fmt.Sprintf("the data volume %s was full at %s", dataRoot, time.Now().UTC().Format(time.RFC3339))
		r.mu.Unlock()
		r.images.Tidy()
	}
	if res.ID != "" {
		r.mu.Lock()
		rr.rec.RemovedBy = removedByFailed
		r.mu.Unlock()
		if rerr := r.daemon.RemoveOwned(ctx, res.ID); rerr != nil {
			r.log.Warn("Removing a failed runner's container failed; it is listed as an orphan", "runner", rr.rec.Request.RunnerName, "error", rerr)
		}
	}
	r.setState(rr, hostapi.RecordFailed, "the "+call+" failed: "+err.Error())
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
// flight for it is cut first. A runner with no container is no error.
func (r *runnerSet) remove(ctx context.Context, name string, diag bool) ([]byte, error) {
	r.mu.Lock()
	rr := r.records[name]
	vol := ""
	if rr != nil {
		rr.rec.RemovedBy, vol = removedByDelete, rr.volume
	}
	r.mu.Unlock()
	if rr != nil && rr.cancel != nil {
		rr.cancel()
		<-rr.done
		r.mu.Lock()
		vol = rr.volume
		r.mu.Unlock()
	}
	tar, err := r.removeContainer(ctx, name, rr, diag)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	delete(r.records, name)
	delete(r.ctrs, name)
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
// any, answering its _diag tar first when diag is set.
func (r *runnerSet) removeContainer(ctx context.Context, name string, rr *record, diag bool) ([]byte, error) {
	ctr, found, err := r.daemon.InspectContainer(ctx, name)
	if err != nil || !found || ctr.Config.Labels[jobspec.LabelRunner] != name {
		return nil, err
	}
	var tar []byte
	if diag {
		tar = r.copyDiag(ctx, &ctr, rr)
	}
	if err := r.daemon.StopContainer(ctx, ctr.ID, hostapi.DeleteStopGrace); err != nil {
		return nil, err
	}
	if err := r.daemon.RemoveOwned(ctx, ctr.ID); err != nil {
		return nil, err
	}
	return tar, nil
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
