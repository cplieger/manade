package github

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// A scale set is its repository's, not the credential's: one created under
// a token is found under an App by name, and none is created (check 6).
func TestEnsureUnderASecondCredentialFindsTheScaleSetByName(t *testing.T) {
	f := newFake(t)
	b := newBroker(t, f, "acme/app")
	f.app(42, "/repos/acme/app/installation")
	f.route("/api/v3/app/installations/42/access_tokens", reply(http.StatusCreated,
		`{"token":"ghs_scaleset42","expires_at":"`+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+`"}`))
	cfg, token := load(t, false, sessionLayout)
	byToken, _, _, _ := newTestScaleSets(t, f, token, &clock{t: time.Now()})
	first, err := byToken.Ensure(t.Context(), cfg, scope(t, cfg, "acme/app"))
	if err != nil || len(first) != 1 {
		t.Fatalf("Ensure under the token = %+v, %v", first, err)
	}
	_, app := load(t, true, sessionLayout)
	byApp, _, _, _ := newTestScaleSets(t, f, app, &clock{t: time.Now()})
	second, err := byApp.Ensure(t.Context(), cfg, scope(t, cfg, "acme/app"))
	if err != nil || len(second) != 1 || second[0] != first[0] {
		t.Errorf("Ensure under the App = %+v, %v; want the token's %+v", second, err, first)
	}
	b.do(func(b *broker) {
		if len(b.sets) != 1 {
			t.Errorf("scale sets on GitHub = %d, want the one the token created", len(b.sets))
		}
	})
}

// Only a classic token reaches an enterprise's runner groups, so an
// App refuses an enterprise scope before sending anything.
func TestAppRefusesAnEnterpriseScopeUnsent(t *testing.T) {
	f := newFake(t)
	cfg, gh := load(t, true, patLayout)
	s, r, _, _ := newTestScaleSets(t, f, gh, &clock{t: time.Now()})
	ent := scope(t, cfg, "enterprises/big")
	if _, err := s.Ensure(t.Context(), cfg, ent); !errors.Is(err, ErrEnterpriseNeedsToken) {
		t.Errorf("Ensure = %v, want ErrEnterpriseNeedsToken", err)
	}
	if _, err := r.Private(t.Context(), ent); !errors.Is(err, ErrEnterpriseNeedsToken) {
		t.Errorf("Private = %v, want ErrEnterpriseNeedsToken", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("calls = %+v, want none", f.calls)
	}
}

// The sweep serves each revision's settings: a scope a revision drops is
// read no more and leaves nothing behind.
func TestSweepFollowsTheSettingsItIsGiven(t *testing.T) {
	f := newFake(t)
	s, _, _ := newTestSweep(t, f)
	f.route(listIP, etagged(runs(1, 7)))
	f.route(jobsPath(7), etagged(jobsPage(7, []string{"test", "queued", "small"})))
	if q := s.sweep(t.Context(), s.cfg, false); len(q) != 1 || names(q[0]) != "test:small" {
		t.Fatalf("sweep = %+v, want acme/app's job", q)
	}
	none, _ := load(t, false, "profiles: {small: {}}\n")
	if q := s.sweep(t.Context(), none, false); len(q) != 0 {
		t.Errorf("sweep with no scope = %+v, want no read", q)
	}
	if len(s.memo) != 0 {
		t.Errorf("memo = %v after the scope was dropped, want empty", s.memo)
	}
}
