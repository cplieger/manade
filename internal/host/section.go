package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/netsetup"
	"github.com/cplieger/manade/internal/hostapi"
)

// sectionPath is the section the host last accepted, which its next boot
// runs.
const sectionPath = stateDir + "/section.json"

// accepted is a section that passed the host's checks.
type accepted struct {
	cfg  *config.Config
	host *config.Host
	hash string
	raw  hostapi.Section
}

// row is how a section applies over the one the host runs: the rows of
// §7.2's table, mildest first.
type row int

const (
	// rowAtOnce is a change of the profiles, images.refresh or the dial
	// address, none of which the rules are built from.
	rowAtOnce row = iota
	// rowSwap is a change of allow or block6, which the rule swap loads.
	rowSwap
	// rowPosture is a change of open or internet: D12's drain, then the
	// posture rebuilt with a boot's functions.
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

// readSection answers the section at path, false when there is none.
func readSection(ctx context.Context, path string) (*hostapi.Section, bool, error) {
	b, err := filesafe.ReadBounded(ctx, path, hostapi.SectionLimit+4096)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var sec hostapi.Section
	if err := json.Unmarshal(b, &sec); err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return &sec, true, nil
}

// writeSection replaces the section at path whole: a temporary file, fsync
// and rename, so a crash leaves the old section or the new one.
func writeSection(ctx context.Context, path string, sec *hostapi.Section) error {
	b, err := json.Marshal(sec)
	if err != nil {
		return err
	}
	if _, err := filesafe.WriteFile(ctx, path, b, filesafe.WithMode(0o600), filesafe.WithMkdirAll(true)); err != nil {
		return fmt.Errorf("writing the section: %w", err)
	}
	return nil
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
// subnet in use takes; on one that restarts, a job subnet its routes would
// shadow, and egress.ipv6 on a kernel with no IPv6 nat table.
func (n *hostNet) hostRefusals(h *config.Host, r row) ([]config.FieldError, error) {
	if r != rowRestart {
		return netsetup.Conflicts(n.Subnet, h), nil
	}
	routes, err := netsetup.ReadRoutes(n.seams.Root)
	if err != nil {
		return nil, err
	}
	at := "hosts." + h.Name + "."
	var out []config.FieldError
	if h.Subnet.IsValid() {
		err = netsetup.CheckSubnet(h, routes.OnLink)
	} else {
		_, err = netsetup.PickSubnet(h, routes.OnLink)
	}
	if err != nil {
		out = append(out, config.FieldError{Path: at + "network.subnet", Message: strings.TrimPrefix(err.Error(), "netsetup: job subnet: ")})
	}
	if h.Egress.IPv6 && !n.Kernel.IPv6NAT {
		out = append(out, config.FieldError{Path: at + "egress.ipv6", Message: "the host's kernel has no IPv6 nat table, which egress.ipv6 needs"})
	}
	return out, nil
}

// deliver checks a section the control plane delivered and, once the host
// accepts it, saves it before it answers how it applies. Before the host's
// first section it hands the section to the waiting boot; during that boot
// it answers hostapi.ErrStarting, and during a rebuild hostapi.ErrApplying.
func (s *supervisor) deliver(ctx context.Context, sec *hostapi.Section) (hostapi.SectionAnswer, error) {
	next, err := check(s.self, sec)
	s.sectionMu.Lock()
	defer s.sectionMu.Unlock()
	if sr, ok := errors.AsType[*hostapi.SectionRefusedError](err); ok {
		s.refuse(sec, sr)
		return hostapi.SectionAnswer{}, sr
	}
	if err != nil {
		return hostapi.SectionAnswer{}, err
	}
	switch s.phase.Load() {
	case phaseWaiting:
		if err := writeSection(ctx, s.sectionPath, &next.raw); err != nil {
			return hostapi.SectionAnswer{}, err
		}
		s.running, s.refused = next, nil
		s.phase.Store(phaseBooting)
		s.publish()
		s.arrived <- next
		s.log.Info("The host received its first section and boots on it", "hash", next.hash, "revision", next.raw.Revision)
		return s.answer(), nil
	case phaseBooting:
		return hostapi.SectionAnswer{}, hostapi.ErrStarting
	}
	if s.rebuilding {
		return hostapi.SectionAnswer{}, hostapi.ErrApplying
	}
	return s.deliverUp(ctx, next)
}

// deliverUp applies next on a host that is up, by the strictest row of
// §7.2's table its change from the running section touches; sectionMu is
// held.
func (s *supervisor) deliverUp(ctx context.Context, next *accepted) (hostapi.SectionAnswer, error) {
	if s.holds(next) {
		if err := writeSection(ctx, s.sectionPath, &next.raw); err != nil {
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
	if err := writeSection(ctx, s.sectionPath, &next.raw); err != nil {
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
	s.log.Info("The host applied its section", "hash", next.hash, "revision", next.raw.Revision)
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
		s.log.Warn("The host is restarting to apply settings", "hash", p.next.hash, "revision", p.next.raw.Revision)
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
