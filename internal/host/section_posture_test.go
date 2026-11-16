package host

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/manade/internal/host/platform"
	"github.com/cplieger/manade/internal/host/resolver"
	"github.com/cplieger/manade/internal/hostapi"
)

// The host parts of host lan's IPv6 sections.
const (
	v6Host       = `{"egress": {"ipv6": true}}`
	closedV6Host = `{"egress": {"ipv6": true, "internet": false}}`
)

// handlerTransport serves each request through h in memory, as the link
// the host opened carries it, with no socket.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		req.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// apiOf is a control plane's client of s's host API.
func apiOf(s *supervisor) *hostapi.Client {
	return hostapi.NewClient(handlerTransport{hostapi.NewServer(&backend{s: s}, s.instance, s.log).Handler()})
}

// put delivers sec to s over PUT /v1/section.
func put(t *testing.T, s *supervisor, sec *hostapi.Section) hostapi.SectionAnswer {
	t.Helper()
	ans, err := apiOf(s).PutSection(t.Context(), sec)
	if err != nil {
		t.Fatalf("PUT /v1/section = %v", err)
	}
	return ans
}

func statusOf(t *testing.T, s *supervisor) hostapi.Status {
	t.Helper()
	st, err := apiOf(s).Status(t.Context())
	if err != nil {
		t.Fatalf("GET /v1/status = %v", err)
	}
	return st
}

// postureView is what s's status says of its section and egress, the time
// its domain list was read left out.
func postureView(t *testing.T, s *supervisor) string {
	t.Helper()
	st := statusOf(t, s)
	if st.GitHub != nil {
		st.GitHub.DomainsAt = time.Time{}
	}
	b, err := json.Marshal(struct {
		IPv6       *hostapi.IPv6Status
		GitHub     *hostapi.GitHubStatus
		PolicyHash string
		Section    hostapi.SectionStatus
		Rules      hostapi.RulesStatus
		DNS        hostapi.DNSStatus
		Profiles   []hostapi.ProfileStatus
	}{st.IPv6, st.GitHub, st.PolicyHash, st.Section, st.Rules, st.DNS, st.Profiles})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// appliedRevision waits for s to run revision rev of its section.
func appliedRevision(t *testing.T, s *supervisor, rev uint64) {
	t.Helper()
	waitFor(t, "the section applied", func() bool {
		st := s.sectionStatus()
		return st.State == hostapi.SectionRunning && st.Revision == rev
	})
}

// A section changing open or internet, delivered with a runner running,
// starts nothing and loads nothing until the runner ends; then the host's
// rules, resolver and status are a fresh boot's of that section. A host
// that becomes open keeps the old posture's disable_ipv6, which only its
// guard's bring-up turns off.
func TestAPostureSectionDrainsThenMatchesAFreshBoot(t *testing.T) {
	postures := map[string]string{"open": openHost, "internet": lanHost, "internet off": closedHost}
	for from, fromHost := range postures {
		for to, toHost := range postures {
			if from == to {
				continue
			}
			t.Run(from+" to "+to, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s, f, run := upSupervisor(t, sectionOf(1, fromHost, smallJSON))
					changing(t, s, run)
					running(s, "r1")
					v6Off := toHost != openHost
					before, policy := kernelState(t, f, v6Off), statusOf(t, s).PolicyHash
					next := sectionOf(2, toHost, smallJSON)
					if ans := put(t, s, next); ans.Applies != hostapi.SectionPendingDrain || !slices.Equal(ans.Waiting, []string{"r1"}) {
						t.Errorf("answer %+v", ans)
					}
					time.Sleep(time.Minute)
					synctest.Wait()
					if _, err := apiOf(s).CreateRunner(t.Context(), request("r2", "small")); !heldFor(err, hostapi.ReasonEgressChangePending) {
						t.Errorf("a start with r1 running = %v", err)
					}
					if got := kernelState(t, f, v6Off); got != before {
						t.Errorf("with r1 running the rules became:\n%s\nwant the old posture's:\n%s", got, before)
					}
					if st := statusOf(t, s); st.PolicyHash != policy || st.Section.Revision != 1 || st.Section.State != hostapi.SectionEgressPending {
						t.Errorf("with r1 running: policy %s, section %+v; want the old policy reported", st.PolicyHash, st.Section)
					}
					exit(s, "r1")
					synctest.Wait()
					fresh, ff, _ := upSupervisor(t, next)
					if got, want := kernelState(t, f, v6Off), kernelState(t, ff, v6Off); got != want {
						t.Errorf("after the change:\n%s\nwant a fresh boot's:\n%s", got, want)
					}
					if got, want := postureView(t, s), postureView(t, fresh); got != want {
						t.Errorf("status after the change:\n%s\nwant a fresh boot's:\n%s", got, want)
					}
					if f.dnsUDP.isClosed() || strings.Count(strings.Join(f.events, " "), "daemon") != 1 {
						t.Errorf("the change stopped the resolver or started a daemon: events %q", f.events)
					}
				})
			})
		}
	}
}

// A section changing only allow keeps the running posture: its learned
// addresses, domain list and resolver stay, and the swap loads the entry.
func TestAnAllowSectionKeepsThePosture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, run := upSupervisor(t, sectionOf(1, closedHost, smallJSON))
		changing(t, s, run)
		p := s.net.posture.Load()
		learn(t.Context(), p.gh.learner, []resolver.Record{{Addr: netip.MustParseAddr("140.82.121.10"), TTL: time.Hour}})
		fetches := strings.Count(strings.Join(f.events, " "), "domains")
		ans := put(t, s, sectionOf(2, `{"egress": {"internet": false, "allow": [{"to": "10.0.0.50", "ports": [5432]}]}}`, smallJSON))
		synctest.Wait()
		if ans.Applies != hostapi.SectionApplied || s.sectionStatus().Revision != 2 {
			t.Errorf("answer %+v, section %+v", ans, s.sectionStatus())
		}
		if s.net.posture.Load() != p || len(s.net.section().Egress.Allow) != 1 {
			t.Error("an allow change built a new posture")
		}
		if st := statusOf(t, s).GitHub; st == nil || st.Addresses != 1 {
			t.Errorf("GitHub = %+v, want the learned address kept", st)
		}
		if got := strings.Count(strings.Join(f.events, " "), "domains"); got != fetches || f.dnsUDP.isClosed() {
			t.Errorf("the swap read the domain list %d more times or closed the resolver", got-fetches)
		}
		if !slices.ContainsFunc(f.kernel.chain(false, "filter", "MANADE-FWD"), func(r string) bool { return strings.Contains(r, "10.0.0.50/32") }) {
			t.Error("the allow entry was not loaded")
		}
	})
}

// A learner's write in flight when a section ends its internet: false
// posture lands before the next posture's rules load, never after them.
func TestALearnInFlightEndsBeforeTheNextPosture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newUpFixture(t)
		p := f.platform()
		var (
			mu    sync.Mutex
			order []string
			armed atomic.Bool
		)
		writing, release := make(chan struct{}), make(chan struct{})
		run := p.Run
		p.Run = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
			step := ""
			switch {
			case strings.Contains(string(stdin), "add manade-gh4 140.82.121.10") && armed.CompareAndSwap(true, false):
				close(writing)
				<-release
				step = "learned"
			case strings.HasSuffix(name, "-restore"):
				step = "rules"
			}
			if step != "" {
				mu.Lock()
				order = append(order, step)
				mu.Unlock()
			}
			return run(ctx, stdin, name, args...)
		}
		s, hostRun := upOn(t, sectionOf(1, closedHost, smallJSON), p)
		changing(t, s, hostRun)
		armed.Store(true)
		go learn(t.Context(), s.net.posture.Load().gh.learner, []resolver.Record{{Addr: netip.MustParseAddr("140.82.121.10"), TTL: time.Hour}})
		<-writing
		mu.Lock()
		mark := len(order)
		mu.Unlock()
		put(t, s, sectionOf(2, lanHost, smallJSON))
		synctest.Wait()
		close(release)
		synctest.Wait()
		if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Revision != 2 {
			t.Fatalf("section = %+v", st)
		}
		mu.Lock()
		after := slices.Clone(order[mark:])
		mu.Unlock()
		if len(after) < 2 || after[0] != "learned" || after[1] != "rules" {
			t.Errorf("writes after the section = %q, want the learn's before the new posture's rules", after)
		}
	})
}

func TestAPostureRebuildThatFails(t *testing.T) {
	t.Run("keeps the old posture's rules and names why", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, f, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
			changing(t, s, run)
			before, policy := kernelState(t, f, true), statusOf(t, s).PolicyHash
			f.fetchErr = errors.New("403 rate limit exceeded")
			put(t, s, sectionOf(2, closedHost, smallJSON))
			synctest.Wait()
			if got := kernelState(t, f, true); got != before {
				t.Errorf("after the failed change:\n%s\nwant the old posture's:\n%s", got, before)
			}
			st := statusOf(t, s)
			if st.Section.State != hostapi.SectionEgressFailed || !strings.Contains(st.Section.Failure, "domain list") || st.PolicyHash != policy || st.GitHub != nil {
				t.Errorf("status: section %+v, policy %s, GitHub %+v", st.Section, st.PolicyHash, st.GitHub)
			}
			if f.dnsUDP.isClosed() {
				t.Error("the failed change closed the resolver")
			}
		})
	})
	t.Run("an old posture that does not come back ends the run", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s, f, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
			held := make(chan error, 1)
			var wg sync.WaitGroup
			wg.Go(func() { held <- s.net.hold(run) })
			wg.Go(func() { s.changes(run, s.cancelRun) })
			s.net.seams.FetchDomains = func(context.Context) ([]byte, error) {
				f.kernel.mu.Lock()
				f.kernel.fail[platform.LegacyDir+"/iptables-restore"] = errors.New("refused")
				f.kernel.mu.Unlock()
				return nil, errors.New("403 rate limit exceeded")
			}
			put(t, s, sectionOf(2, closedHost, smallJSON))
			<-run.Done()
			if cause := context.Cause(run); cause == nil || !strings.Contains(cause.Error(), "old egress posture") || !strings.Contains(cause.Error(), "refused") {
				t.Errorf("the run ended with %v, want the failed return to the old posture", cause)
			}
			// The run's end, not the hold, stops the daemon: hold answers nil
			// when the cancel reaches it first.
			<-held
			wg.Wait()
		})
	})
}

// Status and the probe read the posture as it stands while a rebuild waits
// on its domain list read.
func TestStatusAndTheProbeAnswerDuringARebuild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := v6Fixture(t, &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}})
		s, run := upOn(t, sectionOf(1, v6Host, smallJSON), f.platform())
		changing(t, s, run)
		synctest.Wait()
		policy := statusOf(t, s).PolicyHash
		reading, release := make(chan struct{}), make(chan struct{})
		s.net.seams.FetchDomains = func(context.Context) ([]byte, error) {
			close(reading)
			<-release
			return []byte(metaBody), nil
		}
		put(t, s, sectionOf(2, closedV6Host, smallJSON))
		<-reading
		var (
			st  hostapi.Status
			rep hostapi.ProbeReport
			err error
		)
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			c := apiOf(s)
			if st, err = c.Status(t.Context()); err == nil {
				rep, err = c.Probe(t.Context(), st.ProbeDeadline)
			}
		}()
		synctest.Wait()
		select {
		case <-answered:
		default:
			t.Fatal("status or the probe waited on the rebuild's domain list read")
		}
		if err != nil || st.Section.State != hostapi.SectionEgressPending || st.Section.Revision != 1 || st.PolicyHash != policy || rep.Proven || rep.Unproven == "" {
			t.Errorf("status %+v, policy %s, probe %+v, %v; want the running section's status and policy, and an unproven probe", st.Section, st.PolicyHash, rep, err)
		}
		close(release)
		synctest.Wait()
		if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Revision != 2 {
			t.Errorf("section = %+v after the read was released", st)
		}
	})
}

// A delivery while the host rebuilds its posture is answered at once and
// changes nothing; the control plane delivers it again.
func TestADeliveryDuringARebuild(t *testing.T) {
	s, _, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	changing(t, s, run)
	reading, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	s.net.seams.FetchDomains = func(context.Context) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(reading)
		}
		<-release
		return []byte(metaBody), nil
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	put(t, s, sectionOf(2, closedHost, smallJSON))
	<-reading
	done := make(chan error, 1)
	go func() {
		_, err := apiOf(s).PutSection(t.Context(), sectionOf(3, lanHost, smallJSON, largeJSON))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, hostapi.ErrApplying) {
			t.Errorf("PUT during the rebuild = %v, want ErrApplying", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT during the rebuild waited for it")
	}
	if saved, _, _ := readSection(t.Context(), s.sectionPath); saved == nil || saved.Revision != 2 {
		t.Errorf("saved %+v, want the section being applied", saved)
	}
	close(release)
	appliedRevision(t, s, 2)
	if s.net.section().Egress.Internet || len(s.served.Load().Profiles) != 1 {
		t.Error("the rebuild did not end on the section it applied")
	}
	if ans, err := apiOf(s).PutSection(t.Context(), sectionOf(3, closedHost, smallJSON, largeJSON)); err != nil || ans.Applies != hostapi.SectionApplied {
		t.Errorf("PUT after the rebuild = %+v, %v, want it applied", ans, err)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d domain list reads, want the rebuild's one", n)
	}
}

// A section the ceiling refuses, delivered during a rebuild, is answered
// ErrApplying like any other: nothing is judged or recorded, and the
// section the rebuild applies is then the one the host runs.
func TestARefusedDeliveryDuringARebuild(t *testing.T) {
	s, _, run := upSupervisor(t, sectionOf(1, lanHost, smallJSON))
	changing(t, s, run)
	reading, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	s.net.seams.FetchDomains = func(context.Context) ([]byte, error) {
		if reads.Add(1) == 1 {
			close(reading)
		}
		<-release
		return []byte(metaBody), nil
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	put(t, s, sectionOf(2, closedHost, smallJSON))
	<-reading
	beyond := sectionOf(3, `{"egress": {"allow": [{"to": "192.168.1.5", "ports": [5432]}]}}`, smallJSON)
	if _, err := apiOf(s).PutSection(t.Context(), beyond); !errors.Is(err, hostapi.ErrApplying) {
		t.Errorf("PUT during the rebuild = %v, want ErrApplying", err)
	}
	if saved, _, _ := readSection(t.Context(), s.sectionPath); saved == nil || saved.Revision != 2 {
		t.Errorf("saved %+v, want the section being applied", saved)
	}
	if st := s.sectionStatus(); st.State != hostapi.SectionEgressPending || st.Refused != nil {
		t.Errorf("section status during the rebuild = %+v, want egress_pending with no refusal", st)
	}
	close(release)
	appliedRevision(t, s, 2)
	if st := s.sectionStatus(); st.Hash != hashOf(t, sectionOf(2, closedHost, smallJSON)) || st.Refused != nil {
		t.Errorf("section status after the rebuild = %+v, want the rebuilt section running", st)
	}
}

// A job started before a change of egress.internet keeps the resolver
// resolv.conf named at its start: it resolves after each change, and its
// GitHub lookup is learned once internet is off.
func TestARunningJobResolvesAcrossInternetChanges(t *testing.T) {
	f := newUpFixture(t)
	up := fakeUpstream(t, netip.MustParseAddr("140.82.121.10"))
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	job := jobResolver(udp.LocalAddr().String())
	learned := func() bool {
		_, ipset := f.kernel.payloads()
		return slices.ContainsFunc(ipset, func(p string) bool { return strings.HasPrefix(p, "add manade-gh4 140.82.121.10 ") })
	}
	// The list read resolves api.github.com through the host's resolver, as
	// the supervisor's HTTP client does, and needs that answer learned.
	var learnedForTheList atomic.Bool
	p := f.platform()
	p.ListenDNS = func(context.Context) (net.PacketConn, net.Listener, error) { return udp, tcp, nil }
	p.Upstreams = func([]netip.Addr) []netip.AddrPort { return []netip.AddrPort{up} }
	p.FetchDomains = func(ctx context.Context) ([]byte, error) {
		if _, err := job.LookupNetIP(ctx, "ip4", "api.github.com"); err != nil {
			return nil, err
		}
		learnedForTheList.Store(learned())
		return []byte(metaBody), nil
	}
	s, run := upOn(t, sectionOf(1, lanHost, smallJSON), p)
	changing(t, s, run)
	resolvAtStart := readAt(t, f.root, "/etc/resolv.conf")
	resolves := func(when string) {
		t.Helper()
		addrs, err := job.LookupNetIP(t.Context(), "ip4", "codeload.github.com")
		if err != nil || !slices.Equal(addrs, []netip.Addr{netip.MustParseAddr("140.82.121.10")}) {
			t.Errorf("%s: the job's lookup = %v, %v, want the upstream's answer", when, addrs, err)
		}
		if got := readAt(t, f.root, "/etc/resolv.conf"); got != resolvAtStart {
			t.Errorf("%s: resolv.conf = %q, want %q unchanged", when, got, resolvAtStart)
		}
	}
	resolves("before any change")
	if learned() {
		t.Error("an internet host learned the job's answer")
	}
	put(t, s, sectionOf(2, closedHost, smallJSON))
	appliedRevision(t, s, 2)
	if !learnedForTheList.Load() {
		t.Error("api.github.com's address was not learned before the domain list was read")
	}
	resolves("after internet was turned off")
	if !learned() {
		t.Error("the job's GitHub lookup after internet was turned off was not learned")
	}
	put(t, s, sectionOf(3, lanHost, smallJSON))
	appliedRevision(t, s, 3)
	resolves("after internet was turned on again")
}

// An internet: false posture refreshes its domain list every six hours;
// one that a delivered section replaced with an internet posture reads it
// no more.
func TestTheDomainListRefreshesOnlyOnAnInternetFalsePosture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, f, run := upSupervisor(t, sectionOf(1, closedHost, smallJSON))
		fetches := func() int { return strings.Count(strings.Join(f.events, " "), "domains") }
		changing(t, s, run)
		time.Sleep(6*time.Hour + time.Minute)
		synctest.Wait()
		if got := fetches(); got != 2 {
			t.Errorf("fetches after one refresh period = %d, want the start's and one refresh", got)
		}
		put(t, s, sectionOf(2, lanHost, smallJSON))
		synctest.Wait()
		if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Revision != 2 {
			t.Fatalf("section = %+v", st)
		}
		time.Sleep(6*time.Hour + time.Minute)
		synctest.Wait()
		if got := fetches(); got != 2 {
			t.Errorf("fetches with internet on = %d, want none more", got)
		}
	})
}

// A section that turns internet on or off on an ipv6 host starts or stops
// the /64 learner with the posture it builds.
func TestAnInternetSectionRebuildsThe64(t *testing.T) {
	// deliverAndWait delivers sec and waits for every part of the host to
	// settle on it.
	deliverAndWait := func(t *testing.T, s *supervisor, sec *hostapi.Section) {
		t.Helper()
		put(t, s, sec)
		synctest.Wait()
		if st := s.sectionStatus(); st.State != hostapi.SectionRunning || st.Revision != sec.Revision {
			t.Fatalf("section = %+v, want revision %d running", st, sec.Revision)
		}
	}
	t.Run("turning internet on asks at once and lets job IPv6 out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := &echoes{replies: []func() ([]byte, error){answer("2a01:e0a:1259:2710::7008")}}
			f := v6Fixture(t, e)
			s, run := upOn(t, sectionOf(1, closedV6Host, smallJSON), f.platform())
			changing(t, s, run)
			time.Sleep(2 * echoInterval)
			synctest.Wait()
			if got := e.asked(); got != 0 {
				t.Errorf("an internet: false host asked its echo endpoint %d times", got)
			}
			at := time.Now()
			deliverAndWait(t, s, sectionOf(2, v6Host, smallJSON))
			if got := e.asked(); got != 1 || time.Since(at) != 0 {
				t.Errorf("asked %d times %v after the section, want once at once", got, time.Since(at))
			}
			if st := statusOf(t, s).IPv6; st == nil || st.Prefix != pfx("2a01:e0a:1259:2710::/64") {
				t.Errorf("IPv6 status = %+v, want the learned /64", st)
			}
			if r := fwd6(f); !passes6(r) || !dropsPrefix(r, "2a01:e0a:1259:2710::/64") {
				t.Errorf("MANADE-FWD6 = %q, want IPv6 passing with the learned /64 dropped", r)
			}
		})
	})
	t.Run("turning internet off stops asking, and on again starts unasked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			e := &echoes{replies: []func() ([]byte, error){
				answer("2a01:e0a:1259:2710::7008"),
				func() ([]byte, error) { return nil, errors.New("dial tcp6: network is unreachable") },
			}}
			f := v6Fixture(t, e)
			s, run := upOn(t, sectionOf(1, v6Host, smallJSON), f.platform())
			changing(t, s, run)
			synctest.Wait()
			if st := s.net.iPv6Status(); st == nil || !st.Prefix.IsValid() {
				t.Fatalf("setup: IPv6Status = %+v, want a learned /64", st)
			}
			deliverAndWait(t, s, sectionOf(2, closedV6Host, smallJSON))
			if st := statusOf(t, s).IPv6; st != nil {
				t.Errorf("IPv6 status = %+v on an internet: false host, want none", st)
			}
			if dropsPrefix(fwd6(f), "2a01:e0a:1259:2710::/64") {
				t.Error("the internet: false rules carry the /64 learned before the change")
			}
			loads := restores(f)
			time.Sleep(2*echoInterval + time.Second)
			synctest.Wait()
			if got := e.asked(); got != 1 {
				t.Errorf("asked %d times, want no ask after internet was turned off", got)
			}
			if got := restores(f); got != loads {
				t.Errorf("the closed rules were restored %d times after the change, want them held as loaded", got-loads)
			}
			deliverAndWait(t, s, sectionOf(3, v6Host, smallJSON))
			if st := statusOf(t, s).IPv6; st == nil || st.Prefix.IsValid() || !strings.Contains(st.Reason, "unreachable") {
				t.Errorf("IPv6 status = %+v, want the failed ask, not the /64 learned before internet was off", st)
			}
			if passes6(fwd6(f)) {
				t.Error("job IPv6 passes on a /64 learned before internet was turned off")
			}
		})
	})
	t.Run("an ask in flight at the change is dropped", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			e := &echoes{replies: []func() ([]byte, error){
				func() ([]byte, error) {
					<-release
					return trace("2a01:e0a:1259:2710::7008"), nil
				},
				func() ([]byte, error) { return nil, errors.New("dial tcp6: network is unreachable") },
			}}
			f := v6Fixture(t, e)
			s, run := upOn(t, sectionOf(1, v6Host, smallJSON), f.platform())
			changing(t, s, run)
			synctest.Wait()
			deliverAndWait(t, s, sectionOf(2, closedV6Host, smallJSON))
			close(release)
			deliverAndWait(t, s, sectionOf(3, v6Host, smallJSON))
			if st := statusOf(t, s).IPv6; st == nil || st.Prefix.IsValid() || !strings.Contains(st.Reason, "unreachable") {
				t.Errorf("IPv6 status = %+v, want the failed ask, not the answer that came after the change", st)
			}
		})
	})
}
