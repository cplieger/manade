package pool

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// With no credential the settings boot a generation that serves no scope,
// and naming one serves them at the next revision (D21's first run).
func TestNoCredentialBootsAndACredentialServes(t *testing.T) {
	r := newRig(t, oneHost())
	r.side = nil
	r.restart()
	r.listed(app, "small", 1)
	snap := r.reg.Snapshot()
	if w := waitingRun(t, &snap, 1); w.Reason != JobScopeNotServed || w.Detail != DetailNotConnected {
		t.Errorf("job %+v, want scope_not_served (github_not_connected)", w)
	}
	if h := r.reg.Health(); !h.Started || h.Stopping {
		t.Errorf("health %+v, want a started pool with no credential", h)
	}
	if n := len(r.p.servedSets()); n != 0 {
		t.Errorf("sessions wanted = %d, want none with no credential", n)
	}
	if strings.Contains(r.log.String(), "Ensuring a scope's scale sets failed") {
		t.Error("a missing credential was logged as a failed ensure")
	}
	r.side = sideOf(r.gh)
	r.apply(cloneConfig(r.cfg))
	if r.small() == nil || len(r.p.servedSets()) != 1 {
		t.Fatalf("scale set %v, sessions %d; want the scope served once a credential is set", r.small(), len(r.p.servedSets()))
	}
	r.listed(app, "small", 1)
	snap = r.reg.Snapshot()
	if w := waitingRun(t, &snap, 1); w.Reason == JobScopeNotServed {
		t.Errorf("job %+v, want it served under the credential", w)
	}
}

// enterpriseRefusing is fakeGitHub under a GitHub App, which refuses an
// enterprise scope as the real client does.
type enterpriseRefusing struct{ *fakeGitHub }

func (g enterpriseRefusing) Ensure(ctx context.Context, cfg *config.Config, sc *config.Scope) ([]github.ScaleSet, error) {
	if sc.Kind == config.ScopeEnterprise {
		return nil, github.ErrEnterpriseNeedsToken
	}
	return g.fakeGitHub.Ensure(ctx, cfg, sc)
}

func TestEnterpriseScopeUnderAnAppIsNotServed(t *testing.T) {
	cfg := oneHost()
	cfg.Scopes = append(cfg.Scopes, config.Scope{Key: "enterprises/big", Owner: "big", Kind: config.ScopeEnterprise, RunnerGroup: "Default"})
	r := quietRig(t, cfg)
	r.side = &GitHub{ScaleSets: enterpriseRefusing{r.gh}, REST: r.gh, Sweep: r.gh}
	r.restart()
	r.p.mu.Lock()
	detail := ensureDetail(r.p.scopes["enterprises/big"].ensureErr)
	r.p.mu.Unlock()
	if detail != DetailEnterpriseNeedsToken {
		t.Errorf("enterprise scope detail = %q, want enterprise_needs_token", detail)
	}
	if r.small() == nil {
		t.Error("the repository scope is not served beside the refused enterprise scope")
	}
}

// sessionLog orders the session opens and closes of every credential.
type sessionLog struct {
	events []string
	mu     sync.Mutex
}

func (l *sessionLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *sessionLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// credentialGitHub is one credential's fake: its sessions take closeAfter
// to close, as a real close's round trip does, and it counts its ensures.
type credentialGitHub struct {
	*fakeGitHub
	log      *sessionLog
	name     string
	ensures  int
	ensureMu sync.Mutex
}

const closeAfter = 50 * time.Millisecond

func (g *credentialGitHub) Ensure(ctx context.Context, cfg *config.Config, sc *config.Scope) ([]github.ScaleSet, error) {
	g.ensureMu.Lock()
	g.ensures++
	g.ensureMu.Unlock()
	return g.fakeGitHub.Ensure(ctx, cfg, sc)
}

func (g *credentialGitHub) Listen(ctx context.Context, _ *config.Scope, ss github.ScaleSet, _ string, _ github.Handler) {
	g.log.add("open " + g.name + " " + ss.Scope)
	<-ctx.Done()
	time.Sleep(closeAfter)
	g.log.add("close " + g.name + " " + ss.Scope)
}

func (g *credentialGitHub) ensured() int {
	g.ensureMu.Lock()
	defer g.ensureMu.Unlock()
	return g.ensures
}

// GitHub holds one session per scale set whatever the credential, so a
// Replace that opened a session before the old one closed would get a 409
// (§9.1, §10.6).
func TestCredentialChangeClosesEverySessionBeforeOneOpens(t *testing.T) {
	cfg := poolConfig([]hostSpec{{name: "default", cpus: 8, mem: 32}},
		[]profileSpec{{name: "small", hosts: []string{"default"}, cpus: 2, mem: 4}}, app, "acme/web")
	log := &sessionLog{}
	old := &credentialGitHub{fakeGitHub: newFakeGitHub(cfg), log: log, name: "old"}
	next := &credentialGitHub{fakeGitHub: newFakeGitHub(cfg), log: log, name: "new"}
	side := func(g *credentialGitHub) *GitHub { return &GitHub{ScaleSets: g, REST: g, Sweep: g} }
	sections := map[string]string{"default": sectionHash(1)}
	h := newFakeHost(cfg, "default", sectionHash(1))
	reg := NewRegistry(&Settings{Config: cfg, GitHub: side(old), Revision: 1, Sections: sections}, &Deps{
		Links: &fakeLinks{hosts: []*fakeHost{h}}, Metrics: newFakeMetrics(), Runs: newFakeRuns(), Hosts: newFakeHostLog(), Settle: &fakeSettler{},
		Records: newRecords(), Routes: newRecords(), Log: slog.New(slog.DiscardHandler),
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		reg.Run(ctx)
	}()
	defer func() { cancel(); <-done }()
	count := func(prefix string) int {
		n := 0
		for _, e := range log.all() {
			if strings.HasPrefix(e, prefix) {
				n++
			}
		}
		return n
	}
	waitFor(t, "both sessions under the old credential", func() bool { return count("open old") == 2 })
	reg.Apply(&Settings{Config: cloneConfig(cfg), GitHub: side(next), Revision: 2, Sections: sections})
	waitFor(t, "both sessions under the new credential", func() bool { return count("open new") == 2 })
	events := log.all()
	firstNew := slices.IndexFunc(events, func(e string) bool { return strings.HasPrefix(e, "open new") })
	if closed := count("close old"); closed != 2 || slices.ContainsFunc(events[firstNew:], func(e string) bool { return strings.HasPrefix(e, "close old") }) {
		t.Errorf("events %v, want both old sessions closed before the first new one opens", events)
	}
	if n := next.ensured(); n != 2 {
		t.Errorf("ensures under the new credential = %d, want one per scope, finding its scale sets by name", n)
	}
}

// A proof read under one credential says nothing of what the next one
// reaches, so a credential change reads every scope at once.
func TestCredentialChangeReadsEveryScopeAgain(t *testing.T) {
	r := quietRig(t, oneHost())
	if len(r.p.servedSets()) != 1 {
		t.Fatal("the scope is not served under the first credential")
	}
	next := newFakeGitHub(r.cfg)
	next.public[app] = true
	r.side = sideOf(next)
	r.apply(cloneConfig(r.cfg))
	if n := len(r.p.servedSets()); n != 0 {
		t.Errorf("sessions wanted = %d right after the change, want none: the new credential reads the scope public", n)
	}
}
