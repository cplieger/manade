package docker

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCreateContainer(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("name") {
		case "warned":
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c-warned", "Warnings": []string{"Your kernel does not support pids limit capabilities or the cgroup is not mounted. PIDs limit discarded.\x1b[0m"}})
		case "warnedStuck":
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c-stuck", "Warnings": []string{"swap limit discarded"}})
		case "blank":
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c-blank", "Warnings": []string{"", "  "}})
		case "noimage":
			writeError(w, http.StatusNotFound, "No such image: x")
		default:
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": nil})
		}
	})
	d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "c-stuck" {
			writeError(w, http.StatusInternalServerError, "remove failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := d.dial(t)
	pids := int64(4096)
	initOn := true
	req := &CreateRequest{
		Image:      imageID,
		Entrypoint: []string{"/home/runner/run.sh"},
		Env:        []string{"ACTIONS_RUNNER_INPUT_JITCONFIG=test-config"},
		Labels:     map[string]string{LabelRunner: "default-1", LabelPool: "someone-else"},
		HostConfig: HostConfig{
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			Memory: 4 << 30, MemorySwap: 4 << 30, NanoCPUs: 2e9, PidsLimit: &pids, Init: &initOn,
			NetworkMode: "manade-isolated", DNS: []string{"192.0.2.53"},
			LogConfig: LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
		},
	}
	t.Run("sendsTheBodyWithThePoolLabels", func(t *testing.T) {
		id, err := c.CreateContainer(t.Context(), "default-1", req)
		if err != nil || id != "c1" {
			t.Fatalf("CreateContainer = %q, %v", id, err)
		}
		reqs := d.seen(http.MethodPost, "/containers/create")
		last := reqs[len(reqs)-1]
		if last.query["name"][0] != "default-1" {
			t.Errorf("name = %v", last.query["name"])
		}
		var body map[string]any
		if err := json.Unmarshal(last.body, &body); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{}
		for k, v := range body["Labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		want := ownedLabels()
		want[LabelRunner] = "default-1"
		if !maps.Equal(labels, want) {
			t.Errorf("Labels = %v, want %v", labels, want)
		}
		hc := body["HostConfig"].(map[string]any)
		for _, k := range []string{"CapDrop", "SecurityOpt", "Memory", "MemorySwap", "NanoCpus", "PidsLimit", "Init", "NetworkMode", "Dns", "LogConfig"} {
			if _, ok := hc[k]; !ok {
				t.Errorf("HostConfig lacks the Engine API field %s: %v", k, hc)
			}
		}
		for _, k := range []string{"Privileged", "Binds", "Mounts", "CapAdd", "GroupAdd", "StorageOpt", "Runtime"} {
			if _, ok := hc[k]; ok {
				t.Errorf("HostConfig sends unset field %s", k)
			}
		}
		if _, ok := body["Cmd"]; ok {
			t.Error("an empty Cmd was sent; it must be absent so the image's Cmd is dropped")
		}
	})
	t.Run("leavesTheCallersLabelsAlone", func(t *testing.T) {
		if req.Labels[LabelPool] != "someone-else" || len(req.Labels) != 2 {
			t.Errorf("req.Labels = %v, want unchanged", req.Labels)
		}
	})
	t.Run("warningsRemoveTheContainer", func(t *testing.T) {
		_, err := c.CreateContainer(t.Context(), "warned", req)
		if !errors.Is(err, ErrCreateWarnings) {
			t.Fatalf("CreateContainer = %v, want ErrCreateWarnings", err)
		}
		if !strings.Contains(err.Error(), "PIDs limit discarded") || strings.Contains(err.Error(), "\x1b") {
			t.Errorf("error text = %q, want the warning sanitized", err)
		}
		del := d.seen(http.MethodDelete, "/containers/c-warned")
		if len(del) != 1 || del[0].query["force"][0] != "1" {
			t.Errorf("deletes = %+v, want one forced", del)
		}
	})
	t.Run("aFailedRemovalJoinsBothErrors", func(t *testing.T) {
		_, err := c.CreateContainer(t.Context(), "warnedStuck", req)
		if !errors.Is(err, ErrCreateWarnings) || !strings.Contains(err.Error(), "remove failed") {
			t.Errorf("CreateContainer = %v, want the warnings and the removal failure", err)
		}
	})
	t.Run("blankWarningsAreNone", func(t *testing.T) {
		if id, err := c.CreateContainer(t.Context(), "blank", req); err != nil || id != "c-blank" {
			t.Errorf("CreateContainer = %q, %v", id, err)
		}
	})
	t.Run("aMissingImageIsNotFound", func(t *testing.T) {
		if _, err := c.CreateContainer(t.Context(), "noimage", req); !errors.Is(err, ErrNotFound) {
			t.Errorf("CreateContainer = %v, want ErrNotFound", err)
		}
	})
}

func TestStartContainer(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "gone" {
			writeError(w, http.StatusNotFound, "No such container")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := d.dial(t)
	if err := c.StartContainer(t.Context(), "c1"); err != nil {
		t.Errorf("StartContainer = %v", err)
	}
	if err := c.StartContainer(t.Context(), "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("StartContainer(gone) = %v, want ErrNotFound", err)
	}
}

func TestListContainers(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{
			{"Id": "c1", "State": "running", "Created": 1794312000, "Labels": map[string]string{LabelRunner: "default-1"}},
			{"Id": "c2", "State": "exited", "Created": 1794312060, "Labels": map[string]string{LabelProbe: "user"}},
		})
	})
	got, err := d.dial(t).ListContainers(t.Context())
	if err != nil || len(got) != 2 {
		t.Fatalf("ListContainers = %v, %v", got, err)
	}
	if got[0].ID != "c1" || got[0].State != "running" || got[0].Labels[LabelRunner] != "default-1" {
		t.Errorf("first = %+v", got[0])
	}
	q := d.seen(http.MethodGet, "/containers/json")[0].query
	if q["all"][0] != "1" {
		t.Errorf("all = %v, want 1", q["all"])
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(q["filters"][0]), &filters); err != nil {
		t.Fatal(err)
	}
	if want := []string{"manade.managed=true", "manade.pool=manade"}; !slices.Equal(filters["label"], want) {
		t.Errorf("label filter = %v, want %v", filters["label"], want)
	}
}

func TestInspectContainer(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "c1" {
			writeError(w, http.StatusNotFound, "No such container")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"Id": "c1full", "Name": "/default-1", "Image": imageID,
			"State": map[string]any{
				"Status": "exited", "Running": false, "ExitCode": 137, "OOMKilled": true,
				"StartedAt": "2026-11-10T12:00:01.5Z", "FinishedAt": "2026-11-10T12:10:00Z",
			},
			"Config":     map[string]any{"Labels": ownedLabels()},
			"HostConfig": map[string]any{"NanoCpus": 2e9, "Memory": 4 << 30, "StorageOpt": map[string]string{"size": "10737418240"}},
		})
	})
	c := d.dial(t)
	ctr, found, err := c.InspectContainer(t.Context(), "c1")
	if err != nil || !found {
		t.Fatalf("InspectContainer = %v, %v", found, err)
	}
	if ctr.ID != "c1full" || ctr.Image != imageID {
		t.Errorf("container = %+v", ctr)
	}
	st := ctr.State
	if st.ExitCode != 137 || !st.OOMKilled || st.Running {
		t.Errorf("exit = %d, oom %v, running %v", st.ExitCode, st.OOMKilled, st.Running)
	}
	if hc := ctr.HostConfig; hc.NanoCPUs != 2e9 || hc.Memory != 4<<30 || ctr.Config.Labels[LabelPool] != "manade" {
		t.Errorf("resources, labels = %d, %d, %v", hc.NanoCPUs, hc.Memory, ctr.Config.Labels)
	}
	if st.FinishedAt.Sub(st.StartedAt) != 9*time.Minute+58500*time.Millisecond {
		t.Errorf("StartedAt, FinishedAt = %v, %v", st.StartedAt, st.FinishedAt)
	}
	if n, err := ctr.DiskBytes(); n != 10<<30 || err != nil {
		t.Errorf("DiskBytes = %d, %v", n, err)
	}
	if _, found, err := c.InspectContainer(t.Context(), "gone"); found || err != nil {
		t.Errorf("InspectContainer(gone) = %v, %v; want not found, nil", found, err)
	}
}

func TestDiskBytes(t *testing.T) {
	for _, tc := range []struct {
		opt  map[string]string
		name string
		want int64
		ok   bool
	}{
		{name: "unset", opt: nil, want: 0, ok: true},
		{name: "bytes", opt: map[string]string{"size": "1048576"}, want: 1 << 20, ok: true},
		{name: "unitsAreNotThePoolsForm", opt: map[string]string{"size": "10G"}},
		{name: "negative", opt: map[string]string{"size": "-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctr := Container{ID: "c"}
			ctr.HostConfig.StorageOpt = tc.opt
			n, err := ctr.DiskBytes()
			if (err == nil) != tc.ok || n != tc.want {
				t.Errorf("DiskBytes = %d, %v; want %d, ok %v", n, err, tc.want, tc.ok)
			}
		})
	}
}

func TestRemoveOwned(t *testing.T) {
	d := newDaemon(t, "1.51")
	other := ownedLabels()
	other[LabelPool] = "other-pool"
	containers := map[string]map[string]string{
		"mine":      ownedLabels(),
		"otherPool": other,
		"unlabeled": {"com.example.stack": "web"},
		"notTrue":   {LabelManaged: "yes", LabelPool: "manade"},
		"racing":    ownedLabels(),
	}
	d.handle("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		labels, ok := containers[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "No such container")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"Id": "id-" + r.PathValue("id"), "Config": map[string]any{"Labels": labels}})
	})
	d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "id-racing" {
			writeError(w, http.StatusNotFound, "No such container")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	c := d.dial(t)
	t.Run("removesAnOwnedContainerByItsInspectedID", func(t *testing.T) {
		if err := c.RemoveOwned(t.Context(), "mine"); err != nil {
			t.Fatalf("RemoveOwned: %v", err)
		}
		del := d.seen(http.MethodDelete, "/containers/id-mine")
		if len(del) != 1 || del[0].query["force"][0] != "1" {
			t.Errorf("deletes = %+v, want one forced by ID", del)
		}
	})
	for _, name := range []string{"otherPool", "unlabeled", "notTrue"} {
		t.Run("refuses"+name, func(t *testing.T) {
			if err := c.RemoveOwned(t.Context(), name); !errors.Is(err, ErrNotOwned) {
				t.Errorf("RemoveOwned = %v, want ErrNotOwned", err)
			}
			if del := d.seen(http.MethodDelete, "/containers/id-"+name); len(del) != 0 {
				t.Errorf("a refused container was deleted: %+v", del)
			}
		})
	}
	t.Run("aContainerAlreadyGoneIsNoError", func(t *testing.T) {
		for _, id := range []string{"absent", "racing"} {
			if err := c.RemoveOwned(t.Context(), id); err != nil {
				t.Errorf("RemoveOwned(%s) = %v, want nil", id, err)
			}
		}
	})
}
