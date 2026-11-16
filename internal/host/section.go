package host

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/hostapi"
)

// sectionPath is the host's state file, hostState.
const sectionPath = stateDir + "/section.json"

// hostState is what a start boots and what this host's boots made of it,
// in one file written whole, so each change of it is one rename and a start
// decides from the file alone.
type hostState struct {
	// Section is the section the next start boots; nil waits for one.
	Section *hostapi.Section `json:"section,omitzero"`
	// Fallback is the section the host ran before an Unbooted Section, if
	// any.
	Fallback *hostapi.Section `json:"fallback,omitzero"`
	// Refused is a boot's refusal, kept for the next start only.
	Refused *hostapi.SectionRefusal `json:"refused,omitzero"`
	// Unbooted marks a Section that has not booted on this host, so a boot
	// step's failure is its refusal rather than the host's. Section is the
	// section each boot of a run runs, so the mark is that section's.
	Unbooted bool `json:"unbooted,omitzero"`
}

// accepted is a section that passed the host's checks.
type accepted struct {
	cfg  *config.Config
	host *config.Host
	hash string
	raw  hostapi.Section
}

// row is how a section applies over the one the host runs: the ways a
// delivered section applies, mildest first.
type row int

const (
	// rowAtOnce is a change of the profiles, images.refresh or the dial
	// address, none of which the rules are built from.
	rowAtOnce row = iota
	// rowSwap is a change of allow or block6, which the rule swap loads.
	rowSwap
	// rowPosture is a change of open or internet: no start until every
	// runner has ended, then the posture rebuilt with a boot's functions.
	rowPosture
	// rowRestart is a change of dns, egress.ipv6 or network.subnet, which
	// fix the job network and every running job's resolvers: the drain,
	// then a restart.
	rowRestart
)

// rowOf answers the strictest row a change from run to next touches.
func rowOf(run, next *config.Host) row {
	switch {
	case !slices.Equal(run.DNS, next.DNS) || run.Egress.IPv6 != next.Egress.IPv6 || run.Subnet != next.Subnet:
		return rowRestart
	case run.Egress.Open != next.Egress.Open || run.Egress.Internet != next.Egress.Internet:
		return rowPosture
	case !reflect.DeepEqual(run.Egress.Allow, next.Egress.Allow) || !slices.Equal(run.Egress.Block6, next.Egress.Block6):
		return rowSwap
	}
	return rowAtOnce
}

// check answers sec as the host accepts it, or its refusal: a
// *hostapi.SectionRefusedError for settings the one parser refuses.
func check(self *config.Self, sec *hostapi.Section) (*accepted, error) {
	hash, err := sec.Hash()
	if err != nil {
		return nil, &hostapi.SectionRefusedError{Errors: []hostapi.FieldError{{Message: "want one JSON object: " + err.Error()}}}
	}
	cfg, err := config.ParseSection(self, sec.Settings)
	if fe, ok := errors.AsType[config.FieldErrors](err); ok {
		return nil, refusedError(fe)
	}
	if err != nil {
		return nil, err
	}
	h, _ := cfg.Host(self.Name)
	return &accepted{cfg: cfg, host: h, raw: *sec, hash: hash}, nil
}

func refusedError(fe []config.FieldError) *hostapi.SectionRefusedError {
	out := make([]hostapi.FieldError, len(fe))
	for i, e := range fe {
		out[i] = hostapi.FieldError{Path: e.Path, Message: e.Message}
	}
	return &hostapi.SectionRefusedError{Errors: out}
}

// stateLimit bounds the state file: two sections and a refusal's errors,
// which name fields of a section.
const stateLimit = 3 * hostapi.SectionLimit

// readState answers the host's state at path, false when there is none. A
// file holding anything but one hostState object is an error, so a file of
// another shape is never read as an empty state.
func readState(ctx context.Context, path string) (hostState, bool, error) {
	var st hostState
	b, err := filesafe.ReadBounded(ctx, path, stateLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return st, false, err
	}
	// Unmarshal reads null into a struct as nothing at all.
	if jsontext.Value(b).Kind() != '{' {
		return hostState{}, false, fmt.Errorf("%s: the state is not a JSON object", path)
	}
	if err := json.Unmarshal(b, &st, json.RejectUnknownMembers(true)); err != nil {
		return hostState{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return st, true, nil
}

// errNotDurable is a state written to its path whose directory's fsync
// failed, so a crash may lose it.
var errNotDurable = errors.New("the state's directory was not synced")

// writeFile is filesafe.WriteFile, a var so a test can fail a save.
var writeFile = filesafe.WriteFile

// writeState replaces the state at path whole: a temporary file, fsync,
// rename and the directory's fsync, so a crash leaves the old state or the
// new one; errNotDurable when only the last step failed. A state readState
// would refuse as too large is refused before anything is written.
func writeState(ctx context.Context, path string, st *hostState) error {
	// v2 escapes neither HTML nor U+2028 and U+2029, so a section takes no
	// more bytes here than it was delivered in.
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	res, err := writeFile(ctx, path, b, filesafe.WithMode(0o600), filesafe.WithMkdirAll(true), filesafe.WithMaxBytes(stateLimit))
	if err != nil {
		return fmt.Errorf("writing %s: %w", filepath.Base(path), err)
	}
	if !res.Durable {
		return errNotDurable
	}
	return nil
}

// save writes the state the host holds. A failure is counted and logged,
// and keepSaving tries again, so the host runs on what it holds and its
// next start finds the last state saved whole; the error is nil once the
// state is at its path. sectionMu is held.
func (s *supervisor) save(ctx context.Context) error {
	err := writeState(ctx, s.sectionPath, &s.state)
	s.unsaved = err != nil
	if err == nil {
		return nil
	}
	s.saveFailures.Add(1)
	s.log.Error("The host's state was not saved", "error", err)
	if errors.Is(err, errNotDurable) {
		return nil
	}
	return err
}

// commit makes st the host's state once it is saved, as a delivery is
// accepted only then; a failed save leaves the state the host held.
// sectionMu is held.
func (s *supervisor) commit(ctx context.Context, st hostState) error {
	before, unsaved := s.state, s.unsaved
	s.state = st
	if err := s.save(ctx); err != nil {
		s.state, s.unsaved = before, unsaved
		return err
	}
	return nil
}

// saveRetry is how often a state the host failed to save is tried again,
// a var so a test can shorten it.
var saveRetry = time.Minute

// keepSaving saves the host's state again while its last save failed,
// until ctx ends. A save that puts the state at its path with a restart
// owed ends the run with ErrRestart, as bootRefused restarts on one.
func (s *supervisor) keepSaving(ctx context.Context) {
	t := time.NewTicker(saveRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.sectionMu.Lock()
		if s.unsaved {
			err := s.save(ctx)
			if !s.unsaved {
				s.log.Info("The host's state was saved")
			}
			if err == nil && s.restartOwed {
				s.log.Warn("The host saved the state its failed boot left, so it restarts into a fresh namespace")
				s.cancelRun(ErrRestart)
			}
		}
		s.sectionMu.Unlock()
	}
}

// The retry of a section whose apply failed: retryBase doubling to
// retryCap.
const (
	retryBase = time.Minute
	retryCap  = 15 * time.Minute
)

// retryAfter answers the wait before the next try of a section that has
// failed fails times.
func retryAfter(fails int) time.Duration {
	if fails > 4 {
		return retryCap
	}
	return retryBase << max(fails-1, 0)
}

// pendingSection is an accepted section the host has not applied yet: one
// waiting for its runners to end, or one whose apply failed.
type pendingSection struct {
	next *accepted
	// failedAt, failure and fails are the last failed apply; retry fires
	// the next.
	failedAt time.Time
	retryAt  time.Time
	retry    *time.Timer
	failure  string
	row      row
	fails    int
}

// hostRefusals answers what of section h the host's own facts refuse: on a
// host that applies it without a restart, a resolver or allow entry the job
// subnet in use takes; on one that restarts, what its boot would refuse,
// read in the namespace as it is. The job bridge's own route goes with the
// restart, and a namespace whose IPv6 the host turned off has no IPv6
// address to read until it boots.
func (n *hostNet) hostRefusals(h *config.Host, r row) ([]config.FieldError, error) {
	if r != rowRestart {
		return netsetup.Conflicts(n.Subnet, h), nil
	}
	off, err := netsetup.IPv6Off(n.seams.Root)
	if err != nil {
		return nil, err
	}
	f, err := readNetFacts(n.seams.Root, n.Kernel, h, !off)
	if err != nil {
		return nil, err
	}
	f.routes.OnLink = slices.DeleteFunc(f.routes.OnLink, func(p netip.Prefix) bool { return p.Masked() == n.Subnet })
	_, out := f.plan(h)
	return out, nil
}

// deliver checks a section the control plane delivered and, once the host
// accepts it, saves it before it answers how it applies. While the host
// boots it answers hostapi.ErrStarting, and during a rebuild
// hostapi.ErrApplying, and judges nothing. Before the host's first section
// it hands the section to the waiting boot. The settings of a section a
// boot of this run refused are refused with that boot's errors.
func (s *supervisor) deliver(ctx context.Context, sec *hostapi.Section) (hostapi.SectionAnswer, error) {
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	switch {
	case s.phase.Load() == phaseBooting:
		return hostapi.SectionAnswer{}, hostapi.ErrStarting
	case s.rebuilding:
		return hostapi.SectionAnswer{}, hostapi.ErrApplying
	}
	next, err := check(s.self, sec)
	if sr, ok := errors.AsType[*hostapi.SectionRefusedError](err); ok {
		s.refuse(sec, sr)
		return hostapi.SectionAnswer{}, sr
	}
	if err != nil {
		return hostapi.SectionAnswer{}, err
	}
	if u := s.unbootable; u != nil && u.Hash == next.hash {
		sr := &hostapi.SectionRefusedError{Errors: u.Errors}
		s.refuse(sec, sr)
		return hostapi.SectionAnswer{}, sr
	}
	if s.phase.Load() == phaseWaiting {
		return s.deliverFirst(ctx, next)
	}
	return s.deliverUp(ctx, next)
}

// deliverFirst saves next, the host's first section, marked not booted,
// and hands it to the waiting boot once the host accepts it as
// firstRefusals judges it. A restart a boot refusal owed is dropped, since
// next's boot decides again. sectionMu is held.
func (s *supervisor) deliverFirst(ctx context.Context, next *accepted) (hostapi.SectionAnswer, error) {
	refusals, err := s.firstRefusals(ctx, next.host)
	if err != nil {
		return hostapi.SectionAnswer{}, err
	}
	if len(refusals) > 0 {
		sr := refusedError(refusals)
		s.refuse(&next.raw, sr)
		return hostapi.SectionAnswer{}, sr
	}
	if err := s.commit(ctx, hostState{Section: &next.raw, Unbooted: true}); err != nil {
		return hostapi.SectionAnswer{}, err
	}
	s.running, s.refused, s.restartOwed = next, nil, false
	s.phase.Store(phaseBooting)
	s.publish()
	s.arrived <- next
	s.log.Info("The host received its first section and boots on it", "hash", next.hash, "revision", slog.Uint64Value(next.raw.Revision))
	return s.answer(), nil
}

// firstRefusals answers what of a first section h the host refuses before
// it saves it: its boot facts and, those accepted, its resolvers asked
// directly, which no rule of the waiting host's empty namespace blocks.
// Facts it fails to read refuse nothing: the boot reads them again and
// ends the run with the cause.
func (s *supervisor) firstRefusals(ctx context.Context, h *config.Host) ([]config.FieldError, error) {
	out, err := bootRefusals(ctx, s.seams, h)
	if err != nil {
		s.log.Warn("The host could not read its network's facts, so its boot judges the section", "error", err)
		return nil, nil
	}
	if len(out) > 0 {
		return out, nil
	}
	if err := netsetup.CheckUpstreams(ctx, h, s.seams.Lookup); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []config.FieldError{{Path: "hosts." + h.Name + ".dns", Message: err.Error()}}, nil
	}
	return nil, nil
}

// deliverUp applies next on a host that is up, by the strictest row its
// change from the running section touches; sectionMu is held.
func (s *supervisor) deliverUp(ctx context.Context, next *accepted) (hostapi.SectionAnswer, error) {
	if s.holds(next) {
		st := s.state
		st.Section = &next.raw
		if next.hash == s.running.hash {
			st.Fallback, st.Unbooted = nil, false
		}
		if err := s.commit(ctx, st); err != nil {
			return hostapi.SectionAnswer{}, err
		}
		s.keepHeld(next)
		s.publish()
		return s.answer(), nil
	}
	r := rowOf(s.running.host, next.host)
	refusals, err := s.net.hostRefusals(next.host, r)
	if err != nil {
		return hostapi.SectionAnswer{}, err
	}
	if len(refusals) > 0 {
		sr := refusedError(refusals)
		s.refuse(&next.raw, sr)
		return hostapi.SectionAnswer{}, sr
	}
	st := hostState{Section: &next.raw}
	if r == rowRestart {
		st.Fallback, st.Unbooted = &s.running.raw, true
	}
	if err := s.commit(ctx, st); err != nil {
		return hostapi.SectionAnswer{}, err
	}
	s.refused = nil
	if err := s.applyRow(ctx, next, r); err != nil {
		return hostapi.SectionAnswer{}, err
	}
	s.publish()
	return s.answer(), nil
}

// holds reports whether next has the settings of the section the host runs
// or of the one it holds pending; sectionMu is held.
func (s *supervisor) holds(next *accepted) bool {
	return next.hash == s.running.hash || (s.pending != nil && s.pending.next.hash == next.hash)
}

// keepHeld takes next, a new revision of a section the host holds. Back on
// the running settings, it drops a section left pending; on the pending
// one whose apply failed, it tries that again at once. sectionMu is held.
func (s *supervisor) keepHeld(next *accepted) {
	s.refused = nil
	if p := s.pending; p != nil && p.next.hash == next.hash {
		p.next = next
		if p.failure != "" {
			s.log.Info("The section whose apply failed was delivered again, so the host tries it now", "hash", next.hash)
			p.retryAt = time.Now()
			s.wake()
		}
		return
	}
	s.running = next
	if s.pending != nil {
		s.log.Info("The section delivered is the one the host runs, so the section left pending is dropped", "hash", next.hash)
		s.clearPending()
	}
}

// applyRow applies next by row r: at once, by the rule swap, or left
// pending a drain, which holds every start. Only a failure the host cannot
// survive is returned, after it ends the run. sectionMu is held.
func (s *supervisor) applyRow(ctx context.Context, next *accepted, r row) error {
	switch r {
	case rowAtOnce:
		s.clearPending()
		s.applyNow(next)
	case rowSwap:
		s.clearPending()
		kept, fatal := s.net.swap(ctx, next.host)
		switch {
		case fatal != nil:
			s.cancelRun(fatal)
			return fatal
		case kept != nil:
			s.failed(&pendingSection{next: next, row: r}, kept)
		default:
			s.applyNow(next)
		}
	default:
		s.stopRetry()
		s.pending = &pendingSection{next: next, row: r}
		reason, why := hostapi.ReasonEgressChangePending, fmt.Sprintf("the host applies a change of egress (open %t, internet %t)", next.host.Egress.Open, next.host.Egress.Internet)
		if r == rowRestart {
			reason, why = hostapi.ReasonRestartPending, "the host restarts to apply a change of dns, egress.ipv6 or network.subnet"
		}
		s.runners.holdEgress(reason, why+" once every runner on it has exited or been removed")
		s.log.Warn("The host takes no start until its runners have ended, then applies the new section", "hash", next.hash, "reason", reason)
		s.wake()
	}
	return nil
}

// applyNow makes next the section the host runs; sectionMu is held.
func (s *supervisor) applyNow(next *accepted) {
	s.running = next
	s.apply(next.cfg, next.host)
	s.log.Info("The host applied its section", "hash", next.hash, "revision", slog.Uint64Value(next.raw.Revision))
}

// wake wakes changes.
func (s *supervisor) wake() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// changes applies each section left pending once the host's runners have
// ended, or once its failed apply is due again, until ctx ends; a failure
// the host cannot survive, or its restart, cancels ctx with why.
func (s *supervisor) changes(ctx context.Context, cancel context.CancelCauseFunc) {
	defer s.stopRetryLocked()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.changed:
		case <-s.runners.egressIdle():
		}
		if err := s.applyPending(ctx); err != nil {
			cancel(err)
			return
		}
	}
}

// applyPending applies the pending section once nothing holds it up. Only
// a failure to bring the old posture back, or the restart, is returned.
func (s *supervisor) applyPending(ctx context.Context) error {
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	p := s.pending
	if !s.due(p) {
		return nil
	}
	if p.row == rowRestart {
		s.log.Warn("The host is restarting to apply settings", "hash", p.next.hash, "revision", slog.Uint64Value(p.next.raw.Revision))
		close(s.restarting)
		return ErrRestart
	}
	// The rebuild may wait out a running probe and GitHub's domain list, so
	// it runs unlocked; deliver refuses every section meanwhile, which
	// leaves p the pending section when it ends.
	s.rebuilding = true
	s.sectionMu.Unlock()
	failed, fatal := s.rebuild(ctx, p)
	s.sectionMu.Lock()
	s.rebuilding = false
	switch {
	case fatal != nil:
		return fatal
	case failed != nil:
		if ctx.Err() == nil {
			s.failed(p, failed)
			s.publish()
		}
		return nil
	}
	s.clearPending()
	s.applyNow(p.next)
	s.publish()
	return nil
}

// due reports whether pending section p may be applied now: its runners
// have ended, or its failed apply's retry is due; sectionMu is held.
func (s *supervisor) due(p *pendingSection) bool {
	switch {
	case p == nil:
		return false
	case p.failure != "":
		return !time.Now().Before(p.retryAt)
	}
	return s.runners.egressDrained()
}

// rebuild loads p's rules: by the swap, or for a change of posture by a
// rebuild with no probe running. failed is p's failure, after which the
// old rules stand; fatal one the host cannot survive.
func (s *supervisor) rebuild(ctx context.Context, p *pendingSection) (failed, fatal error) {
	if p.row == rowSwap {
		return s.net.swap(ctx, p.next.host)
	}
	resume, err := s.prober.Pause(ctx)
	if err != nil {
		return err, nil
	}
	defer resume()
	return s.net.change(ctx, p.next.host)
}

// failed keeps p pending after its apply failed with err, every start held,
// and arms its retry; sectionMu is held.
func (s *supervisor) failed(p *pendingSection, err error) {
	s.stopRetry()
	now := time.Now()
	p.fails++
	wait := retryAfter(p.fails)
	p.failure, p.failedAt, p.retryAt = err.Error(), now, now.Add(wait)
	p.retry = time.AfterFunc(wait, s.wake)
	s.pending = p
	s.runners.failEgress("the section's egress failed to apply, so the host kept its old egress and takes no start until it applies: " + p.failure)
	s.log.Error("The host kept its old egress after the section failed to apply, and tries again", "error", err, "retry_in", wait, "fails", p.fails)
}

// clearPending drops the pending section and takes starts again;
// sectionMu is held.
func (s *supervisor) clearPending() {
	s.stopRetry()
	s.pending = nil
	s.runners.releaseEgress()
}

// stopRetry stops the pending section's retry; sectionMu is held.
func (s *supervisor) stopRetry() {
	if p := s.pending; p != nil && p.retry != nil {
		p.retry.Stop()
	}
}

func (s *supervisor) stopRetryLocked() {
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	s.stopRetry()
}

// refuse records sec, which the host refused; sectionMu is held.
func (s *supervisor) refuse(sec *hostapi.Section, sr *hostapi.SectionRefusedError) {
	hash, _ := sec.Hash()
	s.refused = &hostapi.SectionRefusal{At: time.Now(), Hash: hash, Errors: sr.Errors}
	s.publish()
	s.log.Warn("The host refused a section and runs the one it had", "error", sr)
}

// publish stores the section's status for status reads; sectionMu is held.
func (s *supervisor) publish() {
	st := &hostapi.SectionStatus{State: hostapi.SectionRunning, Refused: s.refused}
	if s.running == nil {
		st.State = hostapi.SectionWaiting
	} else {
		st.Hash, st.Revision = s.running.hash, s.running.raw.Revision
		if s.refused != nil {
			st.State = hostapi.SectionRefused
		}
	}
	if p := s.pending; p != nil {
		st.PendingHash, st.PendingRevision = p.next.hash, p.next.raw.Revision
		switch {
		case p.failure != "":
			st.Failure, st.FailedAt, st.RetryAt = p.failure, p.failedAt, p.retryAt
			if s.refused == nil {
				st.State = hostapi.SectionEgressFailed
			}
		case s.refused != nil:
		case p.row == rowRestart:
			st.State = hostapi.SectionRestartPending
		default:
			st.State = hostapi.SectionEgressPending
		}
	}
	s.view.Store(st)
}

// sectionStatus is the section's status now, with the runners a pending
// section waits for.
func (s *supervisor) sectionStatus() hostapi.SectionStatus {
	st := *s.view.Load()
	st.SaveFailures = s.saveFailures.Load()
	if st.PendingHash != "" && st.Failure == "" && s.phase.Load() == phaseUp {
		st.Waiting = s.runners.holdWaiting()
	}
	return st
}

// answer is how the section last accepted applies; sectionMu is held.
func (s *supervisor) answer() hostapi.SectionAnswer {
	p := s.pending
	if p == nil {
		return hostapi.SectionAnswer{Hash: s.running.hash, Applies: hostapi.SectionApplied}
	}
	ans := hostapi.SectionAnswer{Hash: p.next.hash, Applies: hostapi.SectionPendingDrain}
	switch {
	case p.failure != "":
		ans.Applies, ans.Detail = hostapi.SectionFailed, p.failure
	case p.row == rowRestart:
		ans.Applies = hostapi.SectionPendingRestart
	}
	if p.failure == "" {
		ans.Waiting = s.runners.holdWaiting()
	}
	return ans
}
