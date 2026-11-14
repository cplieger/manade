package hostapi

import (
	"strconv"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/manade/internal/docker"
)

// RecordState is how far the host has taken a runner it accepted.
type RecordState int

// The record states.
const (
	// RecordCreating is a runner accepted and not yet created.
	RecordCreating RecordState = iota + 1
	// RecordStarting is a runner whose create returned and whose start has
	// not.
	RecordStarting
	// RecordStarted is a runner whose start returned.
	RecordStarted
	// RecordFailed is a runner whose create or start failed.
	RecordFailed
)

// Container is one of the daemon's labelled runner containers.
type Container struct {
	StartedAt  time.Time
	FinishedAt time.Time
	ID         string
	// Name is the runner name the container is labelled with.
	Name    string
	Profile string
	Scope   string
	// State is the container's Docker state.
	State     string
	Limits    Limits
	RunnerID  int64
	ExitCode  int
	OOMKilled bool
}

// Record is the host's own record of a runner, held from its POST's answer
// to its DELETE.
type Record struct {
	Request RunnerRequest
	// Reason says why a failed runner failed.
	Reason string
	// RemovedBy names who removed the runner's container and why.
	RemovedBy string
	// Limits are what the runner is built with, fixed at acceptance.
	Limits Limits
	State  RecordState
}

// The reasons a list gives a runner whose record and container disagree.
const (
	reasonRemovedOutside = "container removed outside the host"
	reasonResetOutside   = "container reset outside the host"
	ReasonHostRestarted  = "host restarted before its start"
	// reasonUnknownState is followed by the Docker state word, quoted.
	reasonUnknownState = "container in an unknown Docker state"
)

// stateWordLimit caps the Docker state word a reason repeats.
const stateWordLimit = 64

// State maps a runner's labelled container and the host's record of it to
// the runner a list shows; either may be nil. It answers false for a runner
// with neither, which is not listed. A failed record with a container lists
// the container as an orphan under it, and any other record whose container
// the host removed itself (RemovedBy set) lists exited once it is gone.
func State(c *Container, r *Record) (Runner, bool) {
	if c == nil && r == nil {
		return Runner{}, false
	}
	run := describe(c, r)
	switch {
	case r != nil && r.State == RecordFailed:
		run.State, run.Reason = StateFailed, r.Reason
		if c != nil {
			run.Orphans = []string{c.ID}
		}
	case c == nil && r.RemovedBy != "":
		run.State = StateExited
	case c == nil:
		run.State, run.Reason = recorded(r, reasonRemovedOutside)
	case c.State == docker.StateCreated && r == nil:
		run.State, run.Reason = StateFailed, ReasonHostRestarted
	case c.State == docker.StateCreated:
		run.State, run.Reason = recorded(r, reasonResetOutside)
	default:
		fromContainer(c, &run)
	}
	return run, true
}

// fromContainer sets run's state from a created-and-started container's
// Docker state, whatever the record says.
func fromContainer(c *Container, run *Runner) {
	switch c.State {
	case docker.StateRunning, docker.StatePaused, docker.StateRestarting:
		run.State = StateRunning
	case docker.StateExited, docker.StateDead, docker.StateRemoving:
		code := c.ExitCode
		run.State, run.ExitCode, run.ExitedAt, run.OOMKilled = StateExited, &code, c.FinishedAt, c.OOMKilled
	default:
		word, _ := textsafe.SanitizeSingleLineCapped(c.State, stateWordLimit, "…")
		run.State, run.Reason = StateFailed, reasonUnknownState+" "+strconv.Quote(word)
	}
}

// recorded is a creating or starting record's own state, or failed with
// reason for a started one, whose container is gone or was reset.
func recorded(r *Record, reason string) (state RunnerState, why string) {
	switch r.State {
	case RecordCreating:
		return StateCreating, ""
	case RecordStarting:
		return StateStarting, ""
	default:
		return StateFailed, reason
	}
}

// describe fills what a runner shows whatever its state: the record's
// request and limits, then the container's labels and built limits, which
// win once a container exists and its create has returned.
func describe(c *Container, r *Record) Runner {
	var run Runner
	if r != nil {
		run = Runner{
			Name: r.Request.RunnerName, Profile: r.Request.Profile, Scope: r.Request.Scope,
			RunnerID: r.Request.RunnerID, Limits: r.Limits, RemovedBy: r.RemovedBy,
		}
	}
	if c == nil {
		return run
	}
	run.Name, run.Profile, run.Scope, run.RunnerID = c.Name, c.Profile, c.Scope, c.RunnerID
	if c.State != docker.StateCreated {
		run.StartedAt = c.StartedAt
	}
	if r == nil || r.State != RecordCreating {
		run.Limits = c.Limits
	}
	return run
}
