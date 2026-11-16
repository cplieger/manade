package hostapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSectionHash(t *testing.T) {
	t.Parallel()
	hash := func(t *testing.T, settings string, revision uint64) string {
		t.Helper()
		s := Section{Settings: []byte(settings), Revision: revision}
		h, err := s.Hash()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	base := hash(t, `{"hosts": {"nas": {"dns": ["9.9.9.9"]}}, "images": {"refresh": "1h"}}`, 1)
	t.Run("keyOrderAndSpacing", func(t *testing.T) {
		if got := hash(t, `{"images":{"refresh":"1h"},"hosts":{"nas":{"dns":["9.9.9.9"]}}}`, 1); got != base {
			t.Errorf("hash %s, want %s: the canonical form ignores order and spacing", got, base)
		}
	})
	t.Run("revision", func(t *testing.T) {
		if got := hash(t, `{"hosts": {"nas": {"dns": ["9.9.9.9"]}}, "images": {"refresh": "1h"}}`, 9); got != base {
			t.Errorf("hash %s, want %s: a revision is not the settings", got, base)
		}
	})
	t.Run("aValue", func(t *testing.T) {
		if got := hash(t, `{"hosts": {"nas": {"dns": ["9.9.9.10"]}}, "images": {"refresh": "1h"}}`, 1); got == base {
			t.Error("another resolver hashes the same")
		}
	})
	t.Run("sha256Hex", func(t *testing.T) {
		// SHA-256 of the canonical form {"a":1}.
		if got := hash(t, `{ "a" : 1 }`, 0); got != "015abd7f5cc57a2dd94b7590f04ad8084273905ee33ec5cebeae62276a97f862" {
			t.Errorf("hash = %s", got)
		}
	})
	t.Run("notJSON", func(t *testing.T) {
		s := Section{Settings: []byte(`{"a":`)}
		if _, err := s.Hash(); err == nil {
			t.Error("a broken section hashed")
		}
	})
}

func TestPutSection(t *testing.T) {
	t.Run("answersHowItApplies", func(t *testing.T) {
		b := &fakeBackend{section: func(*Section) (SectionAnswer, error) {
			return SectionAnswer{Hash: "h1", Applies: SectionPendingDrain, Waiting: []string{"r1"}}, nil
		}}
		c := linkClient(t, b)
		ans, err := c.PutSection(t.Context(), &Section{Settings: []byte(`{"hosts": {"nas": {}}}`), Revision: 4})
		if err != nil || ans.Applies != SectionPendingDrain || ans.Hash != "h1" || len(ans.Waiting) != 1 {
			t.Errorf("answer = %+v, %v", ans, err)
		}
		if len(b.sections) != 1 || b.sections[0].Revision != 4 || !strings.Contains(string(b.sections[0].Settings), `"nas"`) {
			t.Errorf("the backend got %+v", b.sections)
		}
	})
	t.Run("aRefusalNamesEachField", func(t *testing.T) {
		b := &fakeBackend{err: &SectionRefusedError{Errors: []FieldError{
			{Path: "hosts.nas.egress.allow[0].to", Message: "outside MANADE_ALLOW_PRIVATE"},
			{Path: "profiles.p.memory", Message: "not a\nsize"},
		}}}
		_, err := linkClient(t, b).PutSection(t.Context(), &Section{Settings: []byte(`{}`)})
		sr, ok := errors.AsType[*SectionRefusedError](err)
		if !ok || len(sr.Errors) != 2 || sr.Errors[0].Path != "hosts.nas.egress.allow[0].to" || sr.Errors[1].Path != "profiles.p.memory" {
			t.Fatalf("err = %#v", err)
		}
		if strings.ContainsAny(sr.Errors[1].Message, "\r\n") {
			t.Errorf("message %q spans lines", sr.Errors[1].Message)
		}
	})
	t.Run("aRefusalIs422", func(t *testing.T) {
		srv := serve(t, &fakeBackend{err: &SectionRefusedError{Errors: []FieldError{{Path: "hosts", Message: "x"}}}})
		r, body := do(t, http.MethodPut, srv.URL+"/v1/section", `{"settings": {}, "revision": 1}`)
		if e := errorOf(t, body); r.StatusCode != http.StatusUnprocessableEntity || e.Code != codeSectionRefused || len(e.Errors) != 1 {
			t.Errorf("status %d, body %s", r.StatusCode, body)
		}
	})
	t.Run("waitingForTheFirstSection", func(t *testing.T) {
		b := &fakeBackend{err: ErrWaitingForSection}
		srv := serve(t, b)
		r, body := do(t, http.MethodGet, srv.URL+"/v1/runners", "")
		if e := errorOf(t, body); r.StatusCode != http.StatusServiceUnavailable || e.Code != codeWaiting {
			t.Errorf("status %d, body %s", r.StatusCode, body)
		}
		if _, err := linkClient(t, b).Runners(t.Context()); !errors.Is(err, ErrWaitingForSection) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("startingOnItsSection", func(t *testing.T) {
		// A deadline already passed: the refusal is still the host's, not a
		// probe timeout.
		b := &fakeBackend{err: ErrStarting, deadline: time.Nanosecond}
		r, body := do(t, http.MethodPost, serve(t, b).URL+"/v1/probe", "")
		if e := errorOf(t, body); r.StatusCode != http.StatusServiceUnavailable || e.Code != codeStarting {
			t.Errorf("status %d, body %s", r.StatusCode, body)
		}
		if _, err := linkClient(t, b).PutSection(t.Context(), &Section{Settings: []byte(`{}`)}); !errors.Is(err, ErrStarting) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("applyingTheSectionBefore", func(t *testing.T) {
		b := &fakeBackend{err: ErrApplying}
		r, body := do(t, http.MethodPut, serve(t, b).URL+"/v1/section", `{"settings": {}, "revision": 2}`)
		if e := errorOf(t, body); r.StatusCode != http.StatusServiceUnavailable || e.Code != codeApplying {
			t.Errorf("status %d, body %s", r.StatusCode, body)
		}
		if _, err := linkClient(t, b).PutSection(t.Context(), &Section{Settings: []byte(`{}`)}); !errors.Is(err, ErrApplying) {
			t.Errorf("err = %v", err)
		}
	})
	for name, body := range map[string]string{
		"settingsNotAnObject": `{"settings": [], "revision": 1}`,
		"noSettings":          `{"revision": 1}`,
		"unknownField":        `{"settings": {}, "revision": 1, "hash": "x"}`,
		"overTheLimit":        `{"settings": {"x": "` + strings.Repeat("x", SectionLimit) + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			b := &fakeBackend{}
			r, out := do(t, http.MethodPut, serve(t, b).URL+"/v1/section", body)
			if r.StatusCode != http.StatusBadRequest || errorOf(t, out).Code != codeBadRequest || len(b.sections) != 0 {
				t.Errorf("status %d, body %.200s, the backend got %d", r.StatusCode, out, len(b.sections))
			}
		})
	}
	t.Run("aSectionLargerThanARequest", func(t *testing.T) {
		b := &fakeBackend{}
		big := `{"settings": {"x": "` + strings.Repeat("x", 2*requestLimit) + `"}, "revision": 1}`
		if r, out := do(t, http.MethodPut, serve(t, b).URL+"/v1/section", big); r.StatusCode != http.StatusOK {
			t.Errorf("status %d, body %.200s", r.StatusCode, out)
		}
	})
}

// The restart announcement is the events stream's last line.
func TestEventsAnnounceARestart(t *testing.T) {
	b := &fakeBackend{changes: make(chan Runner), restarting: make(chan struct{})}
	srv := serve(t, b)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/events", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() {
		t.Fatal("no first line")
	}
	close(b.restarting)
	var last Event
	n := 0
	for sc.Scan() {
		if err := json.Unmarshal(sc.Bytes(), &last); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 || last.Type != EventRestarting || last.InstanceID != "inst-1" || last.Time.IsZero() {
		t.Errorf("last line = %+v after %d lines, want the restart", last, n)
	}
}
