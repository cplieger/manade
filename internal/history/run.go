package history

import (
	"cmp"
	"slices"
	"strings"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/github"
	"github.com/cplieger/manade/internal/hostapi"
	"github.com/cplieger/manade/internal/pool"
	"github.com/cplieger/manade/internal/store"
)

// The results a run row records (§5.3.1); GitHub's text beside other.
const (
	resultSucceeded = "succeeded"
	resultFailed    = "failed"
	resultCancelled = "cancelled"
	resultOther     = "other"
)

// result answers GitHub's JobCompleted result as a run's result and, for
// one outside the set, GitHub's text.
func result(raw string) (res, text string) {
	switch strings.ToLower(raw) {
	case "succeeded":
		return resultSucceeded, ""
	case "failed":
		return resultFailed, ""
	case "canceled", "cancelled":
		return resultCancelled, ""
	}
	return resultOther, textsafe.SanitizeSingleLineBounded(raw, 32)
}

// reassignment reports GitHub's reassignment cancel, which ends no run:
// canceled with no runner and no finish time.
func reassignment(e *github.Event) bool {
	return e.Kind == github.Completed && e.Runner == "" && e.Finished.IsZero() && strings.EqualFold(e.Result, "canceled")
}

// job writes job message e of scale set ss, seen at, to its run row; host
// holds its runner.
func job(h *store.HistoryTx, ss github.ScaleSet, host string, e *github.Event, at time.Time) error {
	id := uint64(e.RequestID) //nolint:gosec // a request ID is positive; job drops any other
	r, found, err := h.Run(id)
	if err != nil {
		return err
	}
	if reassignment(e) {
		if found && r.Runner == "" && r.EndedAt.IsZero() {
			return h.DeleteRun(id)
		}
		return nil
	}
	if !found {
		r = store.Run{ID: id, SeenAt: at, Scope: ss.Scope, Profile: ss.Profile}
	}
	describe(&r, e)
	if e.Runner != "" {
		if err := bind(h, &r, e.Runner, host); err != nil {
			return err
		}
	}
	times(&r, e, at)
	return h.PutRun(&r)
}

// times takes e's start, once r has a runner, and a completion's result
// and finish, with the wait and duration they give.
func times(r *store.Run, e *github.Event, at time.Time) {
	if r.StartedAt.IsZero() && r.Runner != "" && !e.RunnerAssigned.IsZero() {
		r.StartedAt = e.RunnerAssigned
		if !r.QueuedAt.IsZero() {
			r.WaitMS = max(0, r.StartedAt.Sub(r.QueuedAt).Milliseconds())
		}
	}
	if e.Kind == github.Completed && r.Result == "" {
		r.Result, r.ResultText = result(e.Result)
		r.FinishedAt = cmp.Or(e.Finished, at)
		end(r, r.FinishedAt)
	}
	if !r.StartedAt.IsZero() && !r.FinishedAt.IsZero() {
		r.DurationMS = max(0, r.FinishedAt.Sub(r.StartedAt).Milliseconds())
	}
}

// describe fills r's job from e's base wherever r has none: every job
// message carries it.
func describe(r *store.Run, e *github.Event) {
	fill := func(dst *string, src string, n int) {
		if *dst == "" {
			*dst = textsafe.SanitizeSingleLineBounded(src, n)
		}
	}
	fill(&r.Repository, e.Repository, 201)
	fill(&r.WorkflowRef, e.WorkflowRef, 512)
	fill(&r.Job, e.Name, 256)
	fill(&r.Event, e.Trigger, 64)
	if r.RunID == 0 {
		r.RunID = e.RunID
	}
	if r.QueuedAt.IsZero() {
		r.QueuedAt = e.Queued
	}
	if r.AssignedAt.IsZero() {
		r.AssignedAt = e.Assigned
	}
}

// bind names runner as the one that took r, on host when known, and
// takes the exit kept for it. A runner another run holds stays there.
func bind(h *store.HistoryTx, r *store.Run, runner, host string) error {
	if r.Host == "" {
		r.Host = host
	}
	if r.Runner != "" {
		return nil
	}
	runner = textsafe.SanitizeSingleLineBounded(runner, 64)
	if other, held, err := h.RunOfRunner(runner); err != nil || (held && other.ID != r.ID) {
		return err
	}
	r.Runner = runner
	x, kept, err := h.Exit(runner)
	if err != nil || !kept {
		return err
	}
	if err := h.DeleteExit(runner); err != nil {
		return err
	}
	return exit(h, r, &x)
}

// exit records x as r's exit, with its evidence; a run keeps its first.
func exit(h *store.HistoryTx, r *store.Run, x *store.Exit) error {
	if r.Exit != "" {
		return nil
	}
	r.Exit, r.ExitCode, r.ExitedAt, r.Usage = x.Reason, x.Code, x.At, x.Usage
	if r.Host == "" {
		r.Host = x.Host
	}
	end(r, x.At)
	if x.Log == "" && x.Diag == nil {
		return nil
	}
	return h.PutBlob(r.ID, &store.Blob{StoredAt: x.At, Log: x.Log, Diag: x.Diag})
}

// end ends r at at, unless it has ended.
func end(r *store.Run, at time.Time) {
	if r.EndedAt.IsZero() {
		r.EndedAt = at
	}
}

// exited writes x to the run of its runner, or keeps it until a job
// message names the runner.
func exited(h *store.HistoryTx, x *pool.RunnerExit) error {
	kept := store.Exit{At: x.At, Code: x.Code, Usage: usage(x.Usage), Host: x.Host, Runner: x.Runner, Reason: string(x.Reason), Diag: x.Diag}
	if x.Log != nil {
		kept.Log = logTail(x.Log)
	}
	r, found, err := h.RunOfRunner(x.Runner)
	if err != nil {
		return err
	}
	if !found {
		return h.PutExit(&kept)
	}
	if r.Exit != "" {
		return nil
	}
	if err := exit(h, &r, &kept); err != nil {
		return err
	}
	return h.PutRun(&r)
}

// usage is a host's usage report as a run keeps it, nil for none.
func usage(u *hostapi.Usage) *store.Usage {
	if u == nil {
		return nil
	}
	return &store.Usage{CPUMS: u.CPU.Milliseconds(), RanMS: u.Ran.Milliseconds(), PeakMemory: u.PeakMemory}
}

// listed closes as lost every open run of host whose runner runners does
// not hold, at at.
func listed(h *store.HistoryTx, host string, runners []string, at time.Time) error {
	var lost []store.Run
	err := h.OpenRuns(host, func(r *store.Run) (bool, error) {
		if !slices.Contains(runners, r.Runner) {
			lost = append(lost, *r)
		}
		return true, nil
	})
	for i := range lost {
		if err != nil {
			break
		}
		r := &lost[i]
		if err = exit(h, r, &store.Exit{At: at, Host: host, Reason: string(pool.ExitLost)}); err == nil {
			err = h.PutRun(r)
		}
	}
	return err
}
