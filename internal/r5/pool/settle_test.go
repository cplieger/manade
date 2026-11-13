package pool

import (
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cplieger/manade/internal/r5/docker"
	"github.com/cplieger/manade/internal/r5/github"
)

// refusal is an answered daemon error, as a 500 from the Engine API is.
func refusal(op string) error {
	return &docker.APIError{Op: op, Code: http.StatusInternalServerError, Message: "refused"}
}

// A start the daemon leaves unanswered may still be running there, so the
// pool reads the container before it cleans anything up: a loaded daemon
// took over 60 s to start a container, and removing it then killed a
// runner that was starting.
func TestAnUnansweredStartIsReadBeforeCleanup(t *testing.T) {
	for _, tc := range []struct {
		setup        func(fx *fixture)
		name         string
		started      bool
		removed      bool
		unregistered bool
		inspected    bool
	}{
		{
			name:    "startedLate",
			setup:   func(fx *fixture) { fx.dk.startLate = errFake },
			started: true, inspected: true,
		},
		{
			name: "startedDuringTheWait",
			setup: func(fx *fixture) {
				fx.dk.startErr = errFake
				reads := 0
				fx.dk.inspectHook = func(b *fakeBox) {
					if reads++; reads == 3 {
						b.run(fx.at)
					}
				}
			},
			started: true, inspected: true,
		},
		{
			name:    "neverStarted",
			setup:   func(fx *fixture) { fx.dk.startErr = errFake },
			removed: true, unregistered: true, inspected: true,
		},
		{
			name: "gone",
			setup: func(fx *fixture) {
				fx.dk.startErr = errFake
				fx.dk.startHook = func(id string) {
					fx.dk.mu.Lock()
					delete(fx.dk.boxes, id)
					fx.dk.mu.Unlock()
				}
			},
			unregistered: true,
		},
		{
			// Neither is touched: the pass adopts or removes the container.
			name:  "unread",
			setup: func(fx *fixture) { fx.dk.startErr, fx.dk.inspectErr = errFake, errFake },
		},
		{
			name:    "refused",
			setup:   func(fx *fixture) { fx.dk.startErr = refusal("start") },
			removed: true, unregistered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFixture(t)
			tc.setup(fx)
			inspects := 0
			hook := fx.dk.inspectHook
			fx.dk.inspectHook = func(b *fakeBox) {
				inspects++
				if hook != nil {
					hook(b)
				}
			}
			fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
			fx.startAll(t)
			page := scrape(t, fx.m)
			if tc.started {
				if got := fx.runners(appDefault); got[idle] != 1 {
					t.Errorf("runners = %v, want one idle", got)
				}
				if !strings.Contains(page, `manade_runner_starts_total{profile="default",result="ok",scope="acme/app"} 1`) {
					t.Error("the start is not counted as ok")
				}
				if strings.Contains(page, `manade_runner_starts_total{profile="default",result="error",scope="acme/app"} 1`) {
					t.Error("a start that went through is counted as an error")
				}
				if n := fx.rec.CountLevel(slog.LevelWarn, "start is retried"); n != 0 {
					t.Errorf("retry warnings = %d, want none", n)
				}
			} else {
				if got := fx.runners(appDefault); len(got) != 0 {
					t.Errorf("runners = %v, want the record dropped", got)
				}
				if !strings.Contains(page, `manade_runner_starts_total{profile="default",result="error",scope="acme/app"} 1`) {
					t.Error("the start is not counted as an error")
				}
			}
			if got := len(fx.dk.removed) == 1; got != tc.removed || len(fx.dk.removed) > 1 {
				t.Errorf("containers removed = %v, want removed %v", fx.dk.removed, tc.removed)
			}
			if got := slices.Equal(fx.gh.removed, []int64{1}); got != tc.unregistered || (!got && len(fx.gh.removed) > 0) {
				t.Errorf("registrations removed = %v, want removed %v", fx.gh.removed, tc.unregistered)
			}
			if got := inspects > 0; got != tc.inspected {
				t.Errorf("container reads = %d, want read %v", inspects, tc.inspected)
			}
		})
	}
}

// A create the daemon leaves unanswered may have made the container, so
// the pool looks it up by name and starts it.
func TestAnUnansweredCreateIsReadByName(t *testing.T) {
	t.Run("createdLate", func(t *testing.T) {
		fx := newFixture(t)
		fx.dk.createLate = errFake
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		fx.startAll(t)
		if got := fx.runners(appDefault); got[idle] != 1 {
			t.Errorf("runners = %v, want one idle", got)
		}
		if len(fx.dk.started) != 1 || len(fx.dk.removed) != 0 || len(fx.gh.removed) != 0 {
			t.Errorf("started %v, removed %v, unregistered %v; want the late container started and nothing removed",
				fx.dk.started, fx.dk.removed, fx.gh.removed)
		}
	})
	t.Run("anotherContainerUnderTheName", func(t *testing.T) {
		fx := newFixture(t)
		fx.dk.createLate = errFake
		fx.dk.inspectHook = func(b *fakeBox) { b.ctr.Config.Labels[docker.LabelRunnerID] = "2" }
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		fx.startAll(t)
		if len(fx.dk.started) != 0 || !slices.Equal(fx.gh.removed, []int64{1}) {
			t.Errorf("started %v, unregistered %v; want a container of another registration left unstarted and this one removed",
				fx.dk.started, fx.gh.removed)
		}
	})
	t.Run("neverCreated", func(t *testing.T) {
		fx := newFixture(t)
		fx.dk.createErr = errFake
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		fx.startAll(t)
		if got := fx.runners(appDefault); len(got) != 0 {
			t.Errorf("runners = %v, want the record dropped", got)
		}
		if !slices.Equal(fx.gh.removed, []int64{1}) || len(fx.dk.started) != 0 {
			t.Errorf("unregistered %v, started %v; want the registration removed and nothing started", fx.gh.removed, fx.dk.started)
		}
	})
	t.Run("refusedIsNotLookedUp", func(t *testing.T) {
		fx := newFixture(t)
		fx.dk.createErr = refusal("create")
		reads := 0
		fx.dk.inspectHook = func(*fakeBox) { reads++ }
		fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
		fx.startAll(t)
		if reads != 0 || len(fx.dk.started) != 0 || !slices.Equal(fx.gh.removed, []int64{1}) {
			t.Errorf("reads %d, started %v, unregistered %v; want an answered refusal failed at once", reads, fx.dk.started, fx.gh.removed)
		}
	})
}

// A pass leaves a container whose start is in flight to the start worker,
// exited or not: the worker removes it after a failed start, and the pass
// must not report that removal as the runner's exit.
func TestAPassLeavesAnInFlightStartsContainer(t *testing.T) {
	fx := newFixture(t)
	fx.dk.startErr = refusal("start")
	fx.dk.startHook = func(id string) {
		fx.dk.mu.Lock()
		fx.dk.boxes[id].state = "exited"
		fx.dk.mu.Unlock()
		if listed, _ := fx.p.reconcile(t.Context()); !listed {
			t.Error("the pass did not list")
		}
	}
	fx.polled(appDefault, fx.at, &github.Stats{Assigned: 1})
	fx.startAll(t)
	if regexp.MustCompile(`manade_runner_exits_total\{[^}]*\} [1-9]`).MatchString(scrape(t, fx.m)) {
		t.Error("the pass recorded the in-flight start's container as an exit")
	}
	if n := fx.rec.CountLevel(slog.LevelInfo, "Container exited"); n != 0 {
		t.Errorf("exit lines = %d, want none", n)
	}
	if len(fx.dk.removed) != 1 {
		t.Errorf("containers removed = %v, want the start worker's removal alone", fx.dk.removed)
	}
}
