package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/obs"
)

// broker is the scale set side of the fake GitHub. scaleset treats any host
// but github.com as a GitHub Enterprise Server, so it registers under
// /api/v3 and then talks to the actions service the registration names,
// here /svc, and to each session's message queue, here /queue/<set>.
type broker struct {
	f          *fakeGitHub
	sets       map[int]*fakeSet
	groups     map[string]int
	queue      map[int][]string
	remove     map[int64]int
	patches    []string
	polls      []brokerPoll
	acks       []int
	initial    Stats
	nextSet    int
	nextMsg    int
	creates    int
	refused    int // creates answered 409
	closes     int
	jits       int
	deletes    int
	conflict   int // creates still to answer 409, after createFail
	createFail int // creates still to fail through createWith
	createWith http.HandlerFunc
	closeSt    int              // a close's status; 0 answers 204
	pollFail   int              // polls still to fail
	failWith   http.HandlerFunc // how they fail; nil answers 404
	onPoll     func()           // runs as each poll arrives
	mu         sync.Mutex
}

type fakeSet struct {
	name          string
	labels        []string
	group         int
	disableUpdate bool
}

// brokerPoll is one poll the queue received: its set, maxCapacity and
// lastMessageId.
type brokerPoll struct{ set, capacity, last int }

func newBroker(t *testing.T, f *fakeGitHub, scopes ...string) *broker {
	t.Helper()
	b := &broker{
		f: f, sets: map[int]*fakeSet{}, groups: map[string]int{}, queue: map[int][]string{},
		remove: map[int64]int{}, nextSet: 100,
	}
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, key := range scopes {
		path := "/api/v3/orgs/" + key
		switch {
		case strings.HasPrefix(key, "enterprises/"):
			path = "/api/v3/" + key
		case strings.Contains(key, "/"):
			path = "/api/v3/repos/" + key
		}
		f.route(path+"/actions/runners/registration-token", reply(http.StatusCreated, `{"token":"registration","expires_at":"`+exp+`"}`))
	}
	f.route("/api/v3/actions/runner-registration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"url": f.srv.URL + "/svc", "token": adminToken(time.Hour)})
	})
	f.mu.Lock()
	f.fallback = b.serve
	f.mu.Unlock()
	return b
}

// adminToken is a JWT-shaped actions service token that expires after
// life: scaleset reads only its expiry, unverified.
func adminToken(life time.Duration) string {
	enc := base64.RawURLEncoding.EncodeToString
	claims := fmt.Sprintf(`{"exp":%d}`, time.Now().Add(life).Unix())
	return enc([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc([]byte(claims)) + ".c2lnbmF0dXJl"
}

// set adds scale set id to the broker.
func (b *broker) set(id int, fs fakeSet) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sets[id] = &fs
}

func (b *broker) do(fn func(b *broker)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fn(b)
}

// push queues one message for set with st as its statistics.
func (b *broker) push(set int, st Stats, events ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextMsg++
	body, _ := json.Marshal("[" + strings.Join(events, ",") + "]")
	b.queue[set] = append(b.queue[set], fmt.Sprintf(
		`{"messageId":%d,"messageType":"RunnerScaleSetJobMessages","body":%s,"statistics":{"totalAssignedJobs":%d,"totalRunningJobs":%d}}`,
		b.nextMsg, body, st.Assigned, st.Running,
	))
}

// job is one job message as the broker sends it.
func job(kind string, run int64, name, runner, result string) string {
	return fmt.Sprintf(`{"messageType":%q,"workflowRunId":%d,"jobDisplayName":%q,"runnerName":%q,"result":%q,`+
		`"queueTime":"2026-11-10T08:00:00Z","runnerAssignTime":"2026-11-10T08:00:30Z","finishTime":"2026-11-10T08:05:00Z"}`,
		kind, run, name, runner, result)
}

// exception answers an actions service error the way the service does.
func exception(w http.ResponseWriter, code int, typ string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `{"typeName":"GitHub.Actions.Runtime.WebApi.%s, GitHub.Actions.Runtime.WebApi","message":"refused"}`, typ)
}

func (b *broker) setJSON(id int) map[string]any {
	s := b.sets[id]
	labels := make([]map[string]string, len(s.labels))
	for i, l := range s.labels {
		labels[i] = map[string]string{"name": l, "type": "System"}
	}
	return map[string]any{
		"id": id, "name": s.name, "runnerGroupId": s.group, "labels": labels,
		"RunnerSetting": map[string]bool{"disableUpdate": s.disableUpdate},
	}
}

func (b *broker) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id := func(i int) int {
		n, _ := strconv.Atoi(parts[i])
		return n
	}
	switch {
	case parts[0] == "queue" && len(parts) == 2:
		b.poll(w, r, id(1))
	case parts[0] == "queue" && len(parts) == 3 && r.Method == http.MethodDelete:
		b.acks = append(b.acks, id(2))
		w.WriteHeader(http.StatusNoContent)
	case len(parts) < 4 || parts[0] != "svc":
		http.NotFound(w, r)
	case parts[3] == "runnergroups":
		g, ok := b.groups[r.URL.Query().Get("groupName")]
		if !ok {
			_, _ = io.WriteString(w, `{"count":0,"value":[]}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"count":1,"value":[{"id":%d,"name":%q}]}`, g, r.URL.Query().Get("groupName"))
	case parts[2] == "distributedtask" && r.Method == http.MethodDelete:
		b.deletes++
		b.removeRunner(w, int64(id(len(parts)-1)))
	case len(parts) == 4:
		b.scaleSets(w, r, body)
	case len(parts) == 5 && r.Method == http.MethodPatch:
		b.patches = append(b.patches, string(body))
		b.sets[id(4)] = decodeSet(body)
		_ = json.NewEncoder(w).Encode(b.setJSON(id(4)))
	case len(parts) == 6 && parts[5] == "sessions":
		b.session(w, r, id(4))
	case len(parts) == 7 && parts[5] == "sessions" && r.Method == http.MethodDelete:
		b.closes++
		switch b.closeSt {
		case 0:
			w.WriteHeader(http.StatusNoContent)
		case http.StatusBadRequest:
			exception(w, http.StatusBadRequest, "RunnerScaleSetSessionExpiredException")
		default:
			exception(w, b.closeSt, "VssServiceException")
		}
	case len(parts) == 6 && parts[5] == "generatejitconfig":
		b.jits++
		var in struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(body, &in)
		_, _ = fmt.Fprintf(w, `{"runner":{"id":%d,"name":%q},"encodedJITConfig":"jit-%s"}`, 500+b.jits, in.Name, in.Name)
	default:
		http.NotFound(w, r)
	}
}

func decodeSet(body []byte) *fakeSet {
	var in struct {
		Name    string                  `json:"name"`
		Labels  []struct{ Name string } `json:"labels"`
		Group   int                     `json:"runnerGroupId"`
		Setting struct {
			DisableUpdate bool `json:"disableUpdate"`
		} `json:"RunnerSetting"`
	}
	_ = json.Unmarshal(body, &in)
	s := &fakeSet{name: in.Name, group: in.Group, disableUpdate: in.Setting.DisableUpdate}
	for _, l := range in.Labels {
		s.labels = append(s.labels, l.Name)
	}
	return s
}

func (b *broker) scaleSets(w http.ResponseWriter, r *http.Request, body []byte) {
	if r.Method == http.MethodPost {
		b.nextSet++
		b.sets[b.nextSet] = decodeSet(body)
		_ = json.NewEncoder(w).Encode(b.setJSON(b.nextSet))
		return
	}
	q := r.URL.Query()
	group, _ := strconv.Atoi(q.Get("runnerGroupId"))
	var out []map[string]any
	for id := range b.nextSet + 1 {
		if s, ok := b.sets[id]; ok && s.group == group && (q.Get("name") == "" || q.Get("name") == s.name) {
			out = append(out, b.setJSON(id))
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"count": len(out), "value": out})
}

func (b *broker) session(w http.ResponseWriter, r *http.Request, set int) {
	b.creates++
	if b.createFail > 0 {
		b.createFail--
		b.createWith(w, r)
		return
	}
	if b.conflict > 0 {
		b.conflict--
		b.refused++
		exception(w, http.StatusConflict, "RunnerScaleSetSessionConflictException")
		return
	}
	_, _ = fmt.Fprintf(w, `{"sessionId":"00000000-0000-4000-8000-%012d","ownerName":"manade","messageQueueUrl":"%s/queue/%d",`+
		`"messageQueueAccessToken":"queue-access","statistics":{"totalAssignedJobs":%d,"totalRunningJobs":%d}}`,
		b.creates, b.f.srv.URL, set, b.initial.Assigned, b.initial.Running)
}

func (b *broker) poll(w http.ResponseWriter, r *http.Request, set int) {
	capacity, _ := strconv.Atoi(r.Header.Get("X-ScaleSetMaxCapacity"))
	last, _ := strconv.Atoi(r.URL.Query().Get("lastMessageId"))
	b.polls = append(b.polls, brokerPoll{set: set, capacity: capacity, last: last})
	if b.onPoll != nil {
		b.onPoll()
	}
	switch {
	case b.pollFail > 0 && b.failWith != nil:
		b.pollFail--
		b.failWith(w, r)
	case b.pollFail > 0:
		b.pollFail--
		http.NotFound(w, r)
	case len(b.queue[set]) == 0:
		w.WriteHeader(http.StatusAccepted)
	default:
		msg := b.queue[set][0]
		b.queue[set] = b.queue[set][1:]
		_, _ = io.WriteString(w, msg)
	}
}

func (b *broker) removeRunner(w http.ResponseWriter, id int64) {
	switch b.remove[id] {
	case 0:
		w.WriteHeader(http.StatusNoContent)
	case http.StatusNotFound:
		exception(w, http.StatusNotFound, "AgentNotFoundException")
	case http.StatusBadRequest:
		exception(w, http.StatusBadRequest, "JobStillRunningException")
	default:
		exception(w, b.remove[id], "VssServiceException")
	}
}

// newTestScaleSets builds ScaleSets over the fake, with now on c.
func newTestScaleSets(t *testing.T, f *fakeGitHub, cfg *config.Config, c *clock) (*ScaleSets, *REST, *obs.Metrics, *capture.Recorder) {
	t.Helper()
	r, m, rec := newTestREST(t, f, cfg)
	tr := f.srv.Client().Transport.(*http.Transport).Clone()
	s := newScaleSets(cfg, tr, r, "test", f.srv.URL)
	s.now = c.now
	return s, r, m, rec
}

// clockSleep makes the package's waits advance c instead of blocking, and
// records them.
func clockSleep(t *testing.T, c *clock) *[]time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	prev := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		c.advance(d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = prev })
	return &waits
}

// handler records what Listen hands the pool. after runs after each
// Polled, with the count so far, outside the lock.
type handler struct {
	after    func(n int)
	opened   []Stats
	polls    []Poll
	held     []bool
	log      []string
	capacity int
	beats    int
	closed   int
	mu       sync.Mutex
}

func (h *handler) Capacity(ScaleSet, time.Time) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log = append(h.log, "capacity")
	return h.capacity
}

func (h *handler) Opened(_ ScaleSet, st Stats) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opened = append(h.opened, st)
	h.log = append(h.log, "opened")
}

func (h *handler) Polled(_ ScaleSet, p *Poll) {
	h.mu.Lock()
	h.polls = append(h.polls, *p)
	h.log = append(h.log, "polled")
	n := len(h.polls)
	h.mu.Unlock()
	if h.after != nil {
		h.after(n)
	}
}

func (h *handler) Held(_ ScaleSet, held bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = append(h.held, held)
	h.log = append(h.log, "held="+strconv.FormatBool(held))
}

func (h *handler) Closed(ScaleSet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed++
	h.log = append(h.log, "closed")
}

func (h *handler) Beat(ScaleSet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beats++
}

var _ Handler = (*handler)(nil)
