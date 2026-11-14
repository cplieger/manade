package resolver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/go/httpclient"
)

// MetaURL is where GitHub publishes its domain list.
const MetaURL = "https://" + GitHubAPI + "/meta"

// DomainsRefresh is how often a host reads the list again.
const DomainsRefresh = 6 * time.Hour

// metaLimit caps the meta body and the saved list; meta is a few hundred KiB.
const metaLimit = 8 << 20

// Where a List came from.
const (
	SourceRead  = "read"
	SourceSaved = "saved"
)

// List is a domain list, when it was read from GitHub and whether this run
// read it or loaded the saved copy. The zero At is the bootstrap list.
type List struct {
	At     time.Time
	Source string
	Domains
}

type savedList struct {
	At time.Time `json:"read_at"`
	Domains
}

// ParseMeta reads `domains.actions_inbound` out of a `GET /meta` body.
func ParseMeta(body []byte) (Domains, error) {
	var meta struct {
		Domains struct {
			ActionsInbound *Domains `json:"actions_inbound"`
		} `json:"domains"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return Domains{}, fmt.Errorf("reading GitHub's meta: %w", err)
	}
	d := meta.Domains.ActionsInbound
	if d == nil {
		return Domains{}, errors.New("GitHub's meta has no domains.actions_inbound")
	}
	if err := d.check(); err != nil {
		return Domains{}, fmt.Errorf("GitHub's meta domains.actions_inbound: %w", err)
	}
	return *d, nil
}

// Fetch answers a `GET /meta` body.
type Fetch func(ctx context.Context) ([]byte, error)

// FetchMeta answers the Fetch that reads MetaURL through client.
func FetchMeta(client *http.Client) Fetch {
	return func(ctx context.Context) ([]byte, error) {
		return httpclient.GetBytes(ctx, client, MetaURL, httpclient.WithMaxBodyBytes(metaLimit),
			httpclient.WithHeaders(func(r *http.Request) {
				r.Header.Set("Accept", "application/vnd.github+json")
			}))
	}
}

type current struct {
	m    *Matcher
	list List
}

// Source holds a host's domain list: the bootstrap list until Start, then
// the last good read, saved at path after each one. Its methods are safe
// for concurrent use.
type Source struct {
	fetch Fetch
	log   *slog.Logger
	cur   atomic.Pointer[current]
	path  string
}

// NewSource answers a Source holding Bootstrap.
func NewSource(fetch Fetch, path string, log *slog.Logger) *Source {
	s := &Source{fetch: fetch, path: path, log: log}
	s.cur.Store(&current{m: NewMatcher(Bootstrap), list: List{Domains: Bootstrap}})
	return s
}

// Matcher answers the current list's Matcher.
func (s *Source) Matcher() *Matcher { return s.cur.Load().m }

// List answers the current list.
func (s *Source) List() List { return s.cur.Load().list }

// Start reads the list, through the host's resolver, which must be serving
// with this Source's Matcher; when the read fails it loads the saved copy.
// It fails only when neither gives a list.
func (s *Source) Start(ctx context.Context) error {
	err := s.Refresh(ctx)
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*saveError](err); ok {
		s.log.Warn("GitHub's domain list was read but not saved; a restart during a GitHub outage will not start", "err", err)
		return nil
	}
	l, found, lerr := s.load(ctx)
	if lerr != nil || !found {
		if lerr == nil {
			lerr = errors.New("no saved copy at " + s.path)
		}
		return fmt.Errorf("an internet: false host needs GitHub's domain list: %w; %w", err, lerr)
	}
	s.cur.Store(&current{m: NewMatcher(l.Domains), list: l})
	s.log.Warn("GitHub's domain list was not read; using the saved copy", "err", err, "read_at", l.At)
	return nil
}

type saveError struct{ err error }

func (e *saveError) Error() string { return "saving GitHub's domain list: " + e.err.Error() }
func (e *saveError) Unwrap() error { return e.err }

// Refresh reads the list once. A good read replaces the list, then is saved;
// a failed one keeps the last good list.
func (s *Source) Refresh(ctx context.Context) error {
	body, err := s.fetch(ctx)
	if err != nil {
		return fmt.Errorf("reading %s: %w", MetaURL, err)
	}
	d, err := ParseMeta(body)
	if err != nil {
		return err
	}
	l := List{At: time.Now(), Source: SourceRead, Domains: d}
	s.cur.Store(&current{m: NewMatcher(d), list: l})
	b, err := json.Marshal(savedList{At: l.At, Domains: d})
	if err == nil {
		_, err = filesafe.WriteFile(ctx, s.path, b, filesafe.WithMode(0o600), filesafe.WithMkdirAll(true))
	}
	if err != nil {
		return &saveError{err: err}
	}
	return nil
}

func (s *Source) load(ctx context.Context) (List, bool, error) {
	b, err := filesafe.ReadBounded(ctx, s.path, metaLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return List{}, false, nil
	}
	if err != nil {
		return List{}, false, fmt.Errorf("reading the saved copy: %w", err)
	}
	var sl savedList
	if err := json.Unmarshal(b, &sl); err != nil {
		return List{}, false, fmt.Errorf("the saved copy %s: %w", s.path, err)
	}
	if err := sl.check(); err != nil {
		return List{}, false, fmt.Errorf("the saved copy %s: %w", s.path, err)
	}
	return List{At: sl.At, Source: SourceSaved, Domains: sl.Domains}, true, nil
}

// Run reads the list every DomainsRefresh until ctx ends, logging a failed
// read at WARN.
func (s *Source) Run(ctx context.Context) {
	t := time.NewTicker(DomainsRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("GitHub's domain list was not refreshed; keeping the last good one", "err", err, "read_at", s.List().At)
			}
		}
	}
}
