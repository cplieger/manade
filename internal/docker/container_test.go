package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func created(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{}})
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func inspectAs(status string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, inspectAnswer("c1", status)) }
}

func TestCreate(t *testing.T) {
	req := &CreateRequest{Image: "sha256:abc", Labels: map[string]string{"manade.runner": "r1", LabelManaged: "false"}}
	// mine answers c1, created and carrying req's labels.
	mine := func(w http.ResponseWriter, _ *http.Request) {
		a := inspectAnswer("c1", StateCreated)
		a["Config"] = map[string]any{"Labels": map[string]string{LabelManaged: "true", "manade.runner": "r1"}}
		writeJSON(w, http.StatusOK, a)
	}
	t.Run("stamps the managed labels and names the container", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", created)
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Done || res.ID != "c1" || res.Err != nil {
			t.Fatalf("Create = %+v", res)
		}
		got := d.seen(http.MethodPost, "/containers/create")[0]
		var body CreateRequest
		if err := json.Unmarshal(got.body, &body); err != nil {
			t.Fatal(err)
		}
		want := managedLabelsAt()
		want["manade.runner"] = "r1"
		if got.query["name"][0] != "r1" || len(body.Labels) != len(want) || body.Labels[LabelManaged] != "true" || body.Labels["manade.runner"] != "r1" || body.Labels[LabelCreated] != want[LabelCreated] {
			t.Errorf("name %v labels %v, want %v", got.query["name"], body.Labels, want)
		}
		if req.Labels[LabelManaged] != "false" {
			t.Error("Create changed the caller's labels")
		}
		if obs := d.observed(); !slices.Contains(obs, "create") {
			t.Errorf("observed %v, want create", obs)
		}
	})
	t.Run("a missing image is a failed create matching ErrImageMissing", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "No such image: sha256:abc")
		})
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Failed || !errors.Is(res.Err, ErrImageMissing) {
			t.Errorf("Create = %+v", res)
		}
	})
	t.Run("another refusal is a failed create, not a missing image", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusBadRequest, "invalid mount config")
		})
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Failed || errors.Is(res.Err, ErrImageMissing) || len(d.seen(http.MethodGet, "/containers/r1/json")) != 0 {
			t.Errorf("Create = %+v", res)
		}
	})
	inUse := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusConflict, `Conflict. The container name "/r1" is already in use`)
	}
	holder := func(labels map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			a := inspectAnswer("c0", StateCreated)
			a["Config"] = map[string]any{"Labels": labels}
			writeJSON(w, http.StatusOK, a)
		}
	}
	t.Run("a name held by this create's own container is done", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", inUse)
		d.handle("GET /containers/r1/json", holder(map[string]string{LabelManaged: "true", LabelCreated: "2000-01-01T00:00:00Z", "manade.runner": "r1"}))
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Done || res.ID != "c0" || res.Err != nil {
			t.Errorf("Create = %+v, want done with the holder's ID", res)
		}
	})
	t.Run("a name held by another container fails with ErrConflict", func(t *testing.T) {
		for name, labels := range map[string]map[string]string{
			"other labels": {LabelManaged: "true", "manade.runner": "r2"},
			"not managed":  {"manade.runner": "r1"},
		} {
			t.Run(name, func(t *testing.T) {
				d := newDaemon(t, "1.51")
				d.handle("POST /containers/create", inUse)
				d.handle("GET /containers/r1/json", holder(labels))
				res := d.dial(t).Create(t.Context(), "r1", req)
				if res.Outcome != Failed || !errors.Is(res.Err, ErrConflict) || res.ID != "" {
					t.Errorf("Create = %+v", res)
				}
				if del := d.seen(http.MethodDelete, "/containers/c0"); len(del) != 0 {
					t.Errorf("removed the holder: %+v", del)
				}
			})
		}
	})
	t.Run("warnings remove the container and fail the create", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{"", "Your kernel does not support memory limit capabilities or the cgroup\nis not mounted. Limitation discarded."}})
		})
		d.handle("DELETE /containers/{id}", noContent)
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Failed || !errors.Is(res.Err, errCreateWarnings) || strings.Contains(res.Err.Error(), "\n") || !strings.Contains(res.Err.Error(), "Limitation discarded") {
			t.Errorf("Create = %+v", res)
		}
		if del := d.seen(http.MethodDelete, "/containers/c1"); len(del) != 1 || del[0].query["force"][0] != "1" {
			t.Errorf("deletes = %+v", del)
		}
	})
	t.Run("warnings whose removal fails leave a failed create with its container", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{"Limitation discarded."}})
		})
		d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "driver busy")
		})
		res := d.dial(t).Create(t.Context(), "r1", req)
		if res.Outcome != Failed || res.ID != "c1" || !errors.Is(res.Err, errCreateWarnings) {
			t.Errorf("Create = %+v, want failed with c1 left to remove", res)
		}
	})
	t.Run("a create answered after its timeout, with the container there, is created", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", after(CallTimeout+time.Minute, created))
			d.handle("GET /containers/r1/json", mine)
			res := d.dial(t).Create(t.Context(), "r1", req)
			if res.Outcome != Done || res.ID != "c1" {
				t.Errorf("Create = %+v", res)
			}
		})
	})
	t.Run("a 404 after a timed-out create is inspected again until the container appears", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			start := time.Now()
			d.handle("GET /containers/r1/json", func(w http.ResponseWriter, r *http.Request) {
				if time.Since(start) < CallTimeout+time.Minute {
					writeError(w, http.StatusNotFound, "No such container: r1")
					return
				}
				mine(w, r)
			})
			res := d.dial(t).Create(t.Context(), "r1", req)
			if res.Outcome != Done || res.ID != "c1" || len(d.seen(http.MethodGet, "/containers/r1/json")) < 2 {
				t.Errorf("Create = %+v after %d inspects", res, len(d.seen(http.MethodGet, "/containers/r1/json")))
			}
		})
	})
	t.Run("a container that never appears fails the create after one more timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			d.handle("GET /containers/r1/json", func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusNotFound, "No such container: r1")
			})
			start := time.Now()
			res := d.dial(t).Create(t.Context(), "r1", req)
			if res.Outcome != Failed || !errors.Is(res.Err, context.DeadlineExceeded) || time.Since(start) != 2*CallTimeout {
				t.Errorf("Create = %+v after %s, want failed at %s", res, time.Since(start), 2*CallTimeout)
			}
		})
	})
	t.Run("an inspect cut by the window's end after a 404 is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			var n atomic.Int32
			d.handle("GET /containers/r1/json", func(w http.ResponseWriter, r *http.Request) {
				if n.Add(1) == 1 {
					writeError(w, http.StatusNotFound, "No such container: r1")
					return
				}
				hold(w, r)
			})
			d.handle("DELETE /containers/{id}", noContent)
			res := d.dial(t).Create(t.Context(), "r1", req)
			if res.Outcome != Pending || n.Load() != 2 {
				t.Errorf("Create = %+v after %d inspects, want pending after 2", res, n.Load())
			}
			if del := d.seen(http.MethodDelete, "/containers/r1"); len(del) != 0 {
				t.Errorf("removed: %+v", del)
			}
		})
	})
	t.Run("a create held past every deadline, its inspect too, is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			d.handle("GET /containers/r1/json", hold)
			if res := d.dial(t).Create(t.Context(), "r1", req); res.Outcome != Pending || res.Err == nil {
				t.Errorf("Create = %+v", res)
			}
		})
	})
	t.Run("a create cut by its caller is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			c := d.dial(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if res := c.Create(ctx, "r1", req); res.Outcome != Pending {
				t.Errorf("Create = %+v", res)
			}
			if n := len(d.seen(http.MethodGet, "/containers/r1/json")); n != 0 {
				t.Errorf("%d inspects after the caller left", n)
			}
		})
	})
}

func TestCreateFoundKeepsItsLimits(t *testing.T) {
	pids := int64(4096)
	limited := &CreateRequest{Image: "sha256:abc", Labels: map[string]string{"manade.runner": "r1"}, HostConfig: HostConfig{
		Memory: 1 << 30, MemorySwap: -1, NanoCPUs: 2_000_000_000, PidsLimit: &pids, StorageOpt: map[string]string{"size": "1024"},
	}}
	// built answers c1 holding name r1 with hostConfig as the daemon built it.
	built := func(hostConfig map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			a := inspectAnswer("c1", StateCreated)
			a["Config"] = map[string]any{"Labels": map[string]string{LabelManaged: "true", "manade.runner": "r1"}}
			a["HostConfig"] = hostConfig
			writeJSON(w, http.StatusOK, a)
		}
	}
	all := func() map[string]any {
		return map[string]any{"Memory": 1 << 30, "MemorySwap": -1, "NanoCpus": 2_000_000_000, "PidsLimit": 4096, "StorageOpt": map[string]string{"size": "1024"}}
	}
	inUse := func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusConflict, `Conflict. The container name "/r1" is already in use`)
	}
	t.Run("a held name with every limit is done", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", inUse)
		d.handle("GET /containers/r1/json", built(all()))
		if res := d.dial(t).Create(t.Context(), "r1", limited); res.Outcome != Done || res.ID != "c1" || res.Err != nil {
			t.Errorf("Create = %+v", res)
		}
	})
	for name, tc := range map[string]struct {
		edit  func(map[string]any)
		field string
	}{
		"Memory":          {func(h map[string]any) { h["Memory"] = 0 }, "Memory"},
		"MemorySwap":      {func(h map[string]any) { h["MemorySwap"] = 2 << 30 }, "MemorySwap"},
		"NanoCpus":        {func(h map[string]any) { h["NanoCpus"] = 0 }, "NanoCpus"},
		"PidsLimit unset": {func(h map[string]any) { delete(h, "PidsLimit") }, "PidsLimit"},
		"PidsLimit 0":     {func(h map[string]any) { h["PidsLimit"] = 0 }, "PidsLimit"},
		"StorageOpt size": {func(h map[string]any) { delete(h, "StorageOpt") }, "StorageOpt size"},
	} {
		field, edit := tc.field, tc.edit
		t.Run("a held name without its "+name+" fails with its ID", func(t *testing.T) {
			hc := all()
			edit(hc)
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", inUse)
			d.handle("GET /containers/r1/json", built(hc))
			res := d.dial(t).Create(t.Context(), "r1", limited)
			if res.Outcome != Failed || res.ID != "c1" || !errors.Is(res.Err, errLimitDiscarded) || !strings.Contains(res.Err.Error(), field+" is ") {
				t.Errorf("Create = %+v, want failed with c1 naming %s", res, field)
			}
		})
	}
	t.Run("a held cpuset other than the one asked fails", func(t *testing.T) {
		pinned := &CreateRequest{Image: "sha256:abc", Labels: map[string]string{"manade.runner": "r1"}, HostConfig: HostConfig{CpusetCpus: "0,1"}}
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/create", inUse)
		d.handle("GET /containers/r1/json", built(map[string]any{"CpusetCpus": ""}))
		res := d.dial(t).Create(t.Context(), "r1", pinned)
		if res.Outcome != Failed || res.ID != "c1" || !strings.Contains(res.Err.Error(), "CpusetCpus") {
			t.Errorf("Create = %+v", res)
		}
	})
	t.Run("a warned create whose removal failed, created again, fails", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		var creates atomic.Int32
		d.handle("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
			if creates.Add(1) == 1 {
				writeJSON(w, http.StatusCreated, map[string]any{"Id": "c1", "Warnings": []string{"Your kernel does not support memory limit capabilities or the cgroup is not mounted. Limitation discarded."}})
				return
			}
			inUse(w, r)
		})
		d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "driver busy")
		})
		hc := all()
		hc["Memory"], hc["MemorySwap"] = 0, 0
		d.handle("GET /containers/r1/json", built(hc))
		c := d.dial(t)
		if res := c.Create(t.Context(), "r1", limited); res.Outcome != Failed || res.ID != "c1" || !errors.Is(res.Err, errCreateWarnings) {
			t.Fatalf("first Create = %+v, want failed with c1 left", res)
		}
		res := c.Create(t.Context(), "r1", limited)
		if res.Outcome != Failed || res.ID != "c1" || !errors.Is(res.Err, errLimitDiscarded) {
			t.Errorf("second Create = %+v, want failed with c1 for its discarded memory limit", res)
		}
	})
	t.Run("a timed-out create whose container lacks its memory limit fails with its ID", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			hc := all()
			hc["Memory"] = 0
			d.handle("GET /containers/r1/json", built(hc))
			res := d.dial(t).Create(t.Context(), "r1", limited)
			if res.Outcome != Failed || res.ID != "c1" || !errors.Is(res.Err, errLimitDiscarded) || !strings.Contains(res.Err.Error(), "Memory is 0") {
				t.Errorf("Create = %+v", res)
			}
		})
	})
	for name, labels := range map[string]map[string]string{
		"no manade labels":          {"other": "x"},
		"a differing manade.runner": {LabelManaged: "true", "manade.runner": "r2"},
	} {
		t.Run("a timed-out create finding a container with "+name+" fails with no ID", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDaemon(t, "1.51")
				d.handle("POST /containers/create", hold)
				d.handle("GET /containers/r1/json", func(w http.ResponseWriter, _ *http.Request) {
					a := inspectAnswer("c9", StateCreated)
					a["Config"] = map[string]any{"Labels": labels}
					a["HostConfig"] = all()
					writeJSON(w, http.StatusOK, a)
				})
				res := d.dial(t).Create(t.Context(), "r1", limited)
				if res.Outcome != Failed || res.ID != "" || !errors.Is(res.Err, context.DeadlineExceeded) {
					t.Errorf("Create = %+v, want failed with the timeout and no ID", res)
				}
				if obs := d.observed(); slices.Contains(obs, "remove") {
					t.Errorf("observed %v, want no remove", obs)
				}
			})
		})
	}
	t.Run("a timed-out create whose container has its limits is done", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/create", hold)
			d.handle("GET /containers/r1/json", built(all()))
			if res := d.dial(t).Create(t.Context(), "r1", limited); res.Outcome != Done || res.ID != "c1" {
				t.Errorf("Create = %+v", res)
			}
		})
	})
}

func TestStart(t *testing.T) {
	t.Run("a start that answers is done", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/start", noContent)
		if res := d.dial(t).Start(t.Context(), "c1"); res.Outcome != Done || res.ID != "c1" {
			t.Errorf("Start = %+v", res)
		}
		if obs := d.observed(); !slices.Contains(obs, "start") {
			t.Errorf("observed %v, want start", obs)
		}
	})
	t.Run("304 for a running container is done", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotModified) })
		if res := d.dial(t).Start(t.Context(), "c1"); res.Outcome != Done {
			t.Errorf("Start = %+v", res)
		}
	})
	t.Run("a refused start fails with no inspect", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusInternalServerError, "no space left on device")
		})
		res := d.dial(t).Start(t.Context(), "c1")
		if res.Outcome != Failed || !strings.Contains(res.Err.Error(), "no space left") || len(d.seen(http.MethodGet, "/containers/c1/json")) != 0 {
			t.Errorf("Start = %+v", res)
		}
	})
	for _, state := range []string{StateRunning, StateExited, StateDead} {
		t.Run("a start answered after its timeout, the container "+state+", is started", func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d := newDaemon(t, "1.51")
				d.handle("POST /containers/{id}/start", after(CallTimeout+time.Minute, noContent))
				d.handle("GET /containers/c1/json", inspectAs(state))
				res := d.dial(t).Start(t.Context(), "c1")
				if res.Outcome != Done || len(d.seen(http.MethodPost, "/containers/c1/start")) != 1 {
					t.Errorf("Start = %+v after %d starts", res, len(d.seen(http.MethodPost, "/containers/c1/start")))
				}
			})
		})
	}
	t.Run("a container still created mid-start gets the start again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			var starts atomic.Int32
			d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
				if starts.Add(1) == 1 {
					hold(w, r)
					return
				}
				w.WriteHeader(http.StatusNotModified)
			})
			d.handle("GET /containers/c1/json", inspectAs(StateCreated))
			if res := d.dial(t).Start(t.Context(), "c1"); res.Outcome != Done || starts.Load() != 2 {
				t.Errorf("Start = %+v after %d starts", res, starts.Load())
			}
		})
	})
	t.Run("only the second start's error fails the start", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			var starts atomic.Int32
			d.handle("POST /containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
				if starts.Add(1) == 1 {
					hold(w, r)
					return
				}
				writeError(w, http.StatusInternalServerError, "OCI runtime create failed")
			})
			d.handle("GET /containers/c1/json", inspectAs(StateCreated))
			res := d.dial(t).Start(t.Context(), "c1")
			if res.Outcome != Failed || !strings.Contains(res.Err.Error(), "OCI runtime") {
				t.Errorf("Start = %+v", res)
			}
		})
	})
	t.Run("a second start that also times out is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/start", hold)
			d.handle("GET /containers/c1/json", inspectAs(StateCreated))
			if res := d.dial(t).Start(t.Context(), "c1"); res.Outcome != Pending {
				t.Errorf("Start = %+v", res)
			}
		})
	})
	t.Run("an inspect that also times out is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/start", hold)
			d.handle("GET /containers/c1/json", hold)
			res := d.dial(t).Start(t.Context(), "c1")
			if res.Outcome != Pending || len(d.seen(http.MethodPost, "/containers/c1/start")) != 1 {
				t.Errorf("Start = %+v", res)
			}
		})
	})
	t.Run("a container gone after a timed-out start fails it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/start", hold)
			d.handle("GET /containers/c1/json", func(w http.ResponseWriter, _ *http.Request) {
				writeError(w, http.StatusNotFound, "No such container: c1")
			})
			if res := d.dial(t).Start(t.Context(), "c1"); res.Outcome != Failed {
				t.Errorf("Start = %+v", res)
			}
		})
	})
	t.Run("a start cut by its caller is pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/start", hold)
			c := d.dial(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if res := c.Start(ctx, "c1"); res.Outcome != Pending {
				t.Errorf("Start = %+v", res)
			}
		})
	})
}

func TestListContainers(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []map[string]any{{"Id": "c1", "State": StateRemoving, "Names": []string{"/r1"}, "Labels": managedLabelsAt()}})
	})
	got, err := d.dial(t).ListContainers(t.Context())
	if err != nil || len(got) != 1 || got[0].State != StateRemoving || got[0].Names[0] != "/r1" {
		t.Fatalf("ListContainers = %+v, %v", got, err)
	}
	q := d.seen(http.MethodGet, "/containers/json")[0].query
	var filters map[string][]string
	if err := json.Unmarshal([]byte(q["filters"][0]), &filters); err != nil || q["all"][0] != "1" || !slices.Equal(filters["label"], []string{"manade.managed=true"}) {
		t.Errorf("query = %v", q)
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
			"Id": "c1", "Name": "/r1", "Image": "sha256:img",
			"Config":     map[string]any{"Labels": map[string]string{"manade.runner": "r1"}},
			"HostConfig": map[string]any{"CpusetCpus": "2,3", "NanoCpus": 0, "Memory": 1 << 30, "StorageOpt": map[string]string{"size": "1024"}},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"manade": map[string]string{
				"IPAddress": "172.18.0.2", "Gateway": "172.18.0.1", "GlobalIPv6Address": "fd00::2", "IPv6Gateway": "fd00::1",
			}}},
			"State": map[string]any{"Status": StateExited, "ExitCode": 137, "OOMKilled": true, "FinishedAt": "2026-11-10T12:00:00Z"},
		})
	})
	c := d.dial(t)
	ctr, found, err := c.InspectContainer(t.Context(), "c1")
	if err != nil || !found {
		t.Fatalf("InspectContainer = %v, %v", found, err)
	}
	disk, derr := ctr.DiskBytes()
	ep := ctr.NetworkSettings.Networks["manade"]
	if ctr.State.Status != StateExited || ctr.State.ExitCode != 137 || !ctr.State.OOMKilled || ctr.HostConfig.CpusetCpus != "2,3" ||
		ctr.HostConfig.Memory != 1<<30 || disk != 1024 || derr != nil || ep.IPAddress != "172.18.0.2" || ep.IPv6Gateway != "fd00::1" ||
		!ctr.State.FinishedAt.Equal(time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("container = %+v", ctr)
	}
	if _, found, err := c.InspectContainer(t.Context(), "gone"); found || err != nil {
		t.Errorf("a missing container = %v, %v; want not found, nil", found, err)
	}
}

func TestDiskBytes(t *testing.T) {
	for opt, want := range map[string]int64{"": 0, "1073741824": 1 << 30} {
		var ctr Container
		if opt != "" {
			ctr.HostConfig.StorageOpt = map[string]string{"size": opt}
		}
		if got, err := ctr.DiskBytes(); err != nil || got != want {
			t.Errorf("DiskBytes(%q) = %d, %v; want %d", opt, got, err, want)
		}
	}
	for _, bad := range []string{"10G", "-1", "x"} {
		var ctr Container
		ctr.HostConfig.StorageOpt = map[string]string{"size": bad}
		if _, err := ctr.DiskBytes(); err == nil {
			t.Errorf("DiskBytes(%q) succeeded", bad)
		}
	}
}

func TestStopContainer(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "stopped":
			w.WriteHeader(http.StatusNotModified)
		case "gone":
			writeError(w, http.StatusNotFound, "No such container")
		case "broken":
			writeError(w, http.StatusInternalServerError, "daemon fault")
		default:
			noContent(w, r)
		}
	})
	c := d.dial(t)
	for _, id := range []string{"c1", "stopped", "gone"} {
		if err := c.StopContainer(t.Context(), id, 15*time.Second); err != nil {
			t.Errorf("StopContainer(%s) = %v", id, err)
		}
	}
	if q := d.seen(http.MethodPost, "/containers/c1/stop")[0].query; q["t"][0] != "15" {
		t.Errorf("query = %v, want t=15", q)
	}
	if err := c.StopContainer(t.Context(), "broken", time.Second); err == nil {
		t.Error("a daemon fault stopped nothing and answered nil")
	}
}

func TestRemoveOwned(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("id") {
		case "r1":
			writeJSON(w, http.StatusOK, inspectAnswer("c1", StateRunning))
		case "foreign":
			writeJSON(w, http.StatusOK, map[string]any{"Id": "c2", "Config": map[string]any{"Labels": map[string]string{"other": "x"}}})
		case "removing":
			writeJSON(w, http.StatusOK, inspectAnswer("c3", StateRemoving))
		default:
			writeError(w, http.StatusNotFound, "No such container")
		}
	})
	d.handle("DELETE /containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "c3" {
			writeError(w, http.StatusConflict, "removal of container c3 is already in progress")
			return
		}
		noContent(w, r)
	})
	c := d.dial(t)
	t.Run("removes the inspected ID with force", func(t *testing.T) {
		if err := c.RemoveOwned(t.Context(), "r1"); err != nil {
			t.Fatal(err)
		}
		if del := d.seen(http.MethodDelete, "/containers/c1"); len(del) != 1 || del[0].query["force"][0] != "1" {
			t.Errorf("deletes = %+v", del)
		}
	})
	t.Run("refuses a container the host did not create", func(t *testing.T) {
		if err := c.RemoveOwned(t.Context(), "foreign"); !errors.Is(err, ErrNotOwned) || len(d.seen(http.MethodDelete, "/containers/c2")) != 0 {
			t.Errorf("RemoveOwned = %v", err)
		}
	})
	t.Run("a removal in progress is a conflict", func(t *testing.T) {
		if err := c.RemoveOwned(t.Context(), "removing"); !errors.Is(err, ErrConflict) {
			t.Errorf("RemoveOwned = %v", err)
		}
	})
	t.Run("a container already gone is no error", func(t *testing.T) {
		if err := c.RemoveOwned(t.Context(), "absent"); err != nil {
			t.Errorf("RemoveOwned = %v", err)
		}
	})
}
