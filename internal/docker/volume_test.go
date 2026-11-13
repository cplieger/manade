package docker

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
)

func TestCreateVolume(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("POST /volumes/create", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]string{"Name": "3f9a"})
	})
	name, err := d.dial(t).CreateVolume(t.Context(), map[string]string{"manade.runner": "r1"})
	if err != nil || name != "3f9a" {
		t.Fatalf("CreateVolume = %q, %v", name, err)
	}
	var body struct {
		Labels map[string]string
		Name   string
	}
	if err := json.Unmarshal(d.seen(http.MethodPost, "/volumes/create")[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != "" || body.Labels[LabelManaged] != "true" || body.Labels["manade.runner"] != "r1" {
		t.Errorf("body = %+v, want an anonymous managed volume", body)
	}
	t.Run("an answer without a name fails", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /volumes/create", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusCreated, map[string]string{}) })
		if _, err := d.dial(t).CreateVolume(t.Context(), nil); err == nil {
			t.Error("CreateVolume succeeded with no name")
		}
	})
}

func TestListVolumes(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /volumes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"Volumes": []map[string]any{{"Name": "v1", "Labels": managedLabelsAt()}}})
	})
	got, err := d.dial(t).ListVolumes(t.Context())
	if err != nil || len(got) != 1 || got[0].Name != "v1" {
		t.Fatalf("ListVolumes = %+v, %v", got, err)
	}
	var filters map[string][]string
	if err := json.Unmarshal([]byte(d.seen(http.MethodGet, "/volumes")[0].query["filters"][0]), &filters); err != nil || !slices.Equal(filters["label"], []string{"manade.managed=true"}) {
		t.Errorf("filters = %v, %v", filters, err)
	}
}

func TestRemoveOwnedVolume(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "mine", "inuse", "vanishing":
			writeJSON(w, http.StatusOK, map[string]any{"Name": r.PathValue("name"), "Labels": managedLabelsAt()})
		case "foreign":
			writeJSON(w, http.StatusOK, map[string]any{"Name": "foreign", "Labels": map[string]string{}})
		default:
			writeError(w, http.StatusNotFound, "no such volume")
		}
	})
	d.handle("DELETE /volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("name") {
		case "inuse":
			writeError(w, http.StatusConflict, "volume is in use")
		case "vanishing":
			writeError(w, http.StatusNotFound, "no such volume")
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	c := d.dial(t)
	if err := c.RemoveOwnedVolume(t.Context(), "mine"); err != nil || len(d.seen(http.MethodDelete, "/volumes/mine")) != 1 {
		t.Errorf("RemoveOwnedVolume(mine) = %v", err)
	}
	if err := c.RemoveOwnedVolume(t.Context(), "foreign"); !errors.Is(err, ErrNotOwned) || len(d.seen(http.MethodDelete, "/volumes/foreign")) != 0 {
		t.Errorf("RemoveOwnedVolume(foreign) = %v", err)
	}
	if err := c.RemoveOwnedVolume(t.Context(), "inuse"); !errors.Is(err, ErrConflict) {
		t.Errorf("RemoveOwnedVolume(inuse) = %v", err)
	}
	for _, name := range []string{"absent", "vanishing"} {
		if err := c.RemoveOwnedVolume(t.Context(), name); err != nil {
			t.Errorf("RemoveOwnedVolume(%s) = %v", name, err)
		}
	}
}
