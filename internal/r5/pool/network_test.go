package pool

import (
	"log/slog"
	"strings"
	"testing"
)

// A docker system prune removes a job network no container is attached to,
// as Komodo's weekly cleanup did on 2026-11-15. Every start then fails until
// something creates it again, so each pass ensures it.
func TestAPassRecreatesAGoneJobNetwork(t *testing.T) {
	fx := newFixture(t)
	fx.dk.networkGone = true
	if listed, _ := fx.p.reconcile(t.Context()); !listed {
		t.Fatal("the pass did not list")
	}
	if n := fx.rec.CountLevel(slog.LevelWarn, "job network was gone"); n != 1 {
		t.Errorf("recreation warnings = %d, want 1", n)
	}
	if listed, _ := fx.p.reconcile(t.Context()); !listed {
		t.Fatal("the second pass did not list")
	}
	if n := fx.rec.CountLevel(slog.LevelWarn, "job network was gone"); n != 1 {
		t.Errorf("recreation warnings after a pass that found it = %d, want still 1", n)
	}
}

// A network the daemon cannot check or create fails no pass: the pass goes
// on, counts the Docker error and retries the network next time.
func TestAPassGoesOnWhenAJobNetworkCannotBeEnsured(t *testing.T) {
	fx := newFixture(t)
	fx.dk.networkErr = errFake
	if listed, _ := fx.p.reconcile(t.Context()); !listed {
		t.Fatal("the pass did not list")
	}
	if !strings.Contains(scrape(t, fx.m), `manade_docker_errors_total{op="network"} 1`) {
		t.Error("the failed network check is not counted")
	}
	if n := fx.rec.CountLevel(slog.LevelWarn, "Checking a job network failed"); n != 1 {
		t.Errorf("network warnings = %d, want 1", n)
	}
}
