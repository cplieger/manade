package host

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cplieger/manade/internal/docker"
)

// engine is a fake Engine API on a Unix socket holding one job network,
// present or not, whose starts answer as start says.
type engine struct {
	// start answers a start with the network present or not; nil answers
	// 404 naming the network while it is gone and 204 once it exists.
	start   func(w http.ResponseWriter, present bool)
	created [][]byte
	mu      sync.Mutex
	starts  int
	present bool
	// createStatus is the network create's answer, 201 when zero.
	createStatus int
}

var apiPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (e *engine) client(t *testing.T) *docker.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(e.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	c, err := docker.Dial(t.Context(), sock, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func engineError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (e *engine) serve(w http.ResponseWriter, r *http.Request) {
	path := apiPrefix.ReplaceAllString(r.URL.Path, "")
	e.mu.Lock()
	present := e.present
	e.mu.Unlock()
	switch {
	case path == "/version":
		_ = json.NewEncoder(w).Encode(map[string]string{"ApiVersion": "1.52", "MinAPIVersion": "1.24"})
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		e.mu.Lock()
		e.starts++
		e.mu.Unlock()
		if e.start != nil {
			e.start(w, present)
			return
		}
		if !present {
			engineError(w, http.StatusNotFound, "failed to set up container networking: network manade not found")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/networks/manade":
		if !present {
			engineError(w, http.StatusNotFound, "network manade not found")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Name": "manade", "Driver": "bridge", "EnableIPv6": false,
			"IPAM":    map[string]any{"Config": []map[string]string{{"Subnet": "172.30.0.0/24"}}},
			"Options": map[string]string{"com.docker.network.bridge.enable_icc": "false", "com.docker.network.bridge.name": "manade0"},
			"Labels":  map[string]string{docker.LabelManaged: "true"},
		})
	case r.Method == http.MethodPost && path == "/networks/create":
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.created = append(e.created, body)
		if e.createStatus == 0 {
			e.present = true
		}
		e.mu.Unlock()
		if e.createStatus != 0 {
			engineError(w, e.createStatus, "the daemon failed")
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"n1"}`))
	default:
		engineError(w, http.StatusNotImplemented, "no route "+r.Method+" "+path)
	}
}

func (e *engine) counts() (starts, creates int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts, len(e.created)
}

func newJobDaemon(t *testing.T, e *engine) *jobDaemon {
	t.Helper()
	return &jobDaemon{Client: e.client(t), network: jobNetworkSpec(netip.MustParsePrefix("172.30.0.0/24"), netip.Prefix{}), log: slog.New(slog.DiscardHandler)}
}

// A start the daemon refuses because the job network is gone recreates the
// network with the job network's properties and is issued once more; any
// other refusal, a second refusal and a failed recreation fail the start.
func TestJobDaemonRecreatesAMissingNetwork(t *testing.T) {
	t.Parallel()
	t.Run("recreatedAndStarted", func(t *testing.T) {
		t.Parallel()
		e := &engine{}
		res := newJobDaemon(t, e).Start(t.Context(), "r1")
		starts, creates := e.counts()
		if res.Outcome != docker.Done || starts != 2 || creates != 1 {
			t.Fatalf("Start = %+v after %d starts and %d creates", res, starts, creates)
		}
		var body struct {
			Options    map[string]string
			Name       string
			Driver     string
			IPAM       struct{ Config []struct{ Subnet string } }
			EnableIPv6 bool
		}
		if err := json.Unmarshal(e.created[0], &body); err != nil {
			t.Fatal(err)
		}
		if body.Name != "manade" || body.Driver != "bridge" || body.Options["com.docker.network.bridge.name"] != "manade0" ||
			body.Options["com.docker.network.bridge.enable_icc"] != "false" || len(body.IPAM.Config) != 1 || body.IPAM.Config[0].Subnet != "172.30.0.0/24" || body.EnableIPv6 {
			t.Errorf("network create = %s", e.created[0])
		}
	})
	t.Run("refusedAgainAfterTheRecreation", func(t *testing.T) {
		t.Parallel()
		e := &engine{start: func(w http.ResponseWriter, _ bool) {
			engineError(w, http.StatusNotFound, "failed to set up container networking: network manade not found")
		}}
		res := newJobDaemon(t, e).Start(t.Context(), "r1")
		starts, creates := e.counts()
		if res.Outcome != docker.Failed || !errors.Is(res.Err, docker.ErrNetworkMissing) || starts != 2 || creates != 1 {
			t.Errorf("Start = %+v after %d starts and %d creates", res, starts, creates)
		}
	})
	t.Run("theRecreationFails", func(t *testing.T) {
		t.Parallel()
		e := &engine{createStatus: http.StatusInternalServerError}
		res := newJobDaemon(t, e).Start(t.Context(), "r1")
		starts, creates := e.counts()
		if res.Outcome != docker.Failed || !errors.Is(res.Err, docker.ErrNetworkMissing) || !strings.Contains(res.Err.Error(), "creating the job network again") ||
			!strings.Contains(res.Err.Error(), "the daemon failed") || starts != 1 || creates != 1 {
			t.Errorf("Start = %+v after %d starts and %d creates", res, starts, creates)
		}
	})
	t.Run("anotherRefusal", func(t *testing.T) {
		t.Parallel()
		e := &engine{start: func(w http.ResponseWriter, _ bool) {
			engineError(w, http.StatusInternalServerError, "no space left on device")
		}}
		res := newJobDaemon(t, e).Start(t.Context(), "r1")
		starts, creates := e.counts()
		if res.Outcome != docker.Failed || errors.Is(res.Err, docker.ErrNetworkMissing) || starts != 1 || creates != 0 {
			t.Errorf("Start = %+v after %d starts and %d creates", res, starts, creates)
		}
	})
}

// Two starts that find the network gone at once create it once: the second
// recreation finds the first's network and keeps it.
func TestJobDaemonRecreatesOnceForConcurrentStarts(t *testing.T) {
	t.Parallel()
	refused := make(chan struct{})
	var arrived sync.WaitGroup
	arrived.Add(2)
	go func() { arrived.Wait(); close(refused) }()
	var once sync.Map
	e := &engine{}
	e.start = func(w http.ResponseWriter, present bool) {
		if present {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if _, again := once.LoadOrStore(w, true); !again {
			arrived.Done()
		}
		<-refused
		engineError(w, http.StatusNotFound, "failed to set up container networking: network manade not found")
	}
	d := newJobDaemon(t, e)
	var wg sync.WaitGroup
	results := make([]docker.Result, 2)
	for i := range results {
		wg.Go(func() { results[i] = d.Start(t.Context(), "r"+string(rune('1'+i))) })
	}
	wg.Wait()
	_, creates := e.counts()
	for i, res := range results {
		if res.Outcome != docker.Done {
			t.Errorf("start %d = %+v", i, res)
		}
	}
	if creates != 1 {
		t.Errorf("the network was created %d times, want 1", creates)
	}
}
