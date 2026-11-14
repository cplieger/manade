package resolver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// metaBody is a GET /meta body in GitHub's shape, trimmed.
const metaBody = `{"verifiable_password_authentication":false,"actions":["4.148.0.0/16"],
"domains":{"website":["*.github.com"],"actions_inbound":{
"full_domains":["github.com","api.github.com","codeload.github.com","ghcr.io"],
"wildcard_domains":["*.githubusercontent.com","*.github.com","ghcr.io"]}}}`

func TestParseMeta(t *testing.T) {
	d, err := parseMeta([]byte(metaBody))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(d.Full, []string{"github.com", "api.github.com", "codeload.github.com", "ghcr.io"}) ||
		!slices.Equal(d.Wildcard, []string{"*.githubusercontent.com", "*.github.com", "ghcr.io"}) {
		t.Errorf("ParseMeta = %+v", d)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"not JSON", "<html>", "reading GitHub's meta"},
		{"no actions_inbound", `{"domains":{"website":["github.com"]}}`, "no domains.actions_inbound"},
		{"an empty list", `{"domains":{"actions_inbound":{"full_domains":[],"wildcard_domains":[]}}}`, "empty"},
		{"a bad name", `{"domains":{"actions_inbound":{"full_domains":["a b"]}}}`, `"a b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseMeta([]byte(tc.body)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseMeta = %v, want an error naming %s", err, tc.want)
			}
		})
	}
}

// fetcher answers its bodies in turn, an error once they run out.
type fetcher struct {
	bodies []string
	calls  atomic.Int32
}

func (f *fetcher) fetch(context.Context) ([]byte, error) {
	i := int(f.calls.Add(1)) - 1
	if i < len(f.bodies) && f.bodies[i] != "" {
		return []byte(f.bodies[i]), nil
	}
	return nil, errors.New("403 rate limit exceeded")
}

func savedPath(t *testing.T) string {
	return filepath.Join(t.TempDir(), "manade", "github-domains.json")
}

func TestSourceStartReadsAndSaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := savedPath(t)
		s := NewSource((&fetcher{bodies: []string{metaBody}}).fetch, path, slog.New(slog.DiscardHandler))
		if s.Matcher().Match("codeload.github.com") || !s.Matcher().Match("api.github.com") {
			t.Error("before Start the list is not the bootstrap list")
		}
		start := time.Now()
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if l := s.List(); l.Source != SourceRead || !l.At.Equal(start) || !s.Matcher().Match("codeload.github.com") {
			t.Errorf("List = %+v, want the read list", l)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("saved mode %v", fi.Mode().Perm())
		}
		var saved struct {
			At   time.Time `json:"read_at"`
			Full []string  `json:"full_domains"`
		}
		b, _ := os.ReadFile(path)
		if err := json.Unmarshal(b, &saved); err != nil || !saved.At.Equal(start) || !slices.Contains(saved.Full, "codeload.github.com") {
			t.Errorf("saved = %s (%v)", b, err)
		}
	})
}

func TestSourceStartFallsBackToTheSavedCopy(t *testing.T) {
	path := savedPath(t)
	first := NewSource((&fetcher{bodies: []string{metaBody}}).fetch, path, slog.New(slog.DiscardHandler))
	if err := first.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	readAt := first.List().At
	for _, tc := range []struct {
		name string
		body string
	}{
		{"on a failed read", ""},
		{"on a read that does not parse", `{"domains":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log bytes.Buffer
			s := NewSource((&fetcher{bodies: []string{tc.body}}).fetch, path, slog.New(slog.NewTextHandler(&log, nil)))
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			l := s.List()
			if l.Source != SourceSaved || !l.At.Equal(readAt) || !s.Matcher().Match("codeload.github.com") {
				t.Errorf("List = %+v, want the saved copy with its read time %v", l, readAt)
			}
			if !strings.Contains(log.String(), "saved copy") {
				t.Errorf("log = %q, want the fallback logged", log.String())
			}
		})
	}
}

func TestSourceStartRefusesWithNoList(t *testing.T) {
	path := savedPath(t)
	s := NewSource((&fetcher{}).fetch, path, slog.New(slog.DiscardHandler))
	err := s.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "no saved copy") {
		t.Errorf("Start = %v, want a refusal naming the read and the missing copy", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"read_at":"2026-11-01T00:00:00Z","full_domains":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("Start = %v, want an empty saved copy refused", err)
	}
}

func TestSourceStartsOnAnUnsavedRead(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSource((&fetcher{bodies: []string{metaBody}}).fetch, filepath.Join(blocker, "github-domains.json"), slog.New(slog.DiscardHandler))
	if err := s.Start(t.Context()); err != nil || s.List().Source != SourceRead {
		t.Errorf("Start = %v, List %+v, want the read list in use though unsaved", err, s.List())
	}
}

func TestSourceRunRefreshesEverySixHours(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fetcher{bodies: []string{metaBody, "", `{"domains":{"actions_inbound":{"full_domains":["api.github.com","github.com"]}}}`}}
		s := NewSource(f.fetch, savedPath(t), slog.New(slog.DiscardHandler))
		if err := s.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		first := s.List().At
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { s.Run(ctx); close(done) }()
		time.Sleep(domainsRefresh + time.Second)
		if f.calls.Load() != 2 || !s.List().At.Equal(first) || !s.Matcher().Match("codeload.github.com") {
			t.Errorf("after a failed refresh: calls %d, list %+v, want the last good list kept", f.calls.Load(), s.List())
		}
		time.Sleep(domainsRefresh)
		if f.calls.Load() != 3 || s.Matcher().Match("codeload.github.com") || s.List().At.Equal(first) {
			t.Errorf("after a good refresh: calls %d, list %+v, want the new list", f.calls.Load(), s.List())
		}
		cancel()
		<-done
	})
}

func TestFetchMeta(t *testing.T) {
	var accept string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		_, _ = w.Write([]byte(metaBody))
	}))
	defer srv.Close()
	client := srv.Client()
	tr := client.Transport.(*http.Transport).Clone()
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	tr.TLSClientConfig.ServerName = "example.com"
	client.Transport = tr
	b, err := FetchMeta(client)(t.Context())
	if err != nil || string(b) != metaBody || accept != "application/vnd.github+json" {
		t.Errorf("FetchMeta = %q, %v, Accept %q", b, err, accept)
	}
}
