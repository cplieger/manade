// Package resolver is an internet: false host's own DNS resolver: it
// forwards every query to the host's configured resolvers and, for a name
// on GitHub's published list of runner domains, adds each address of the
// answer to the rules' learned set before it answers.
package resolver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cplieger/manade/internal/egress"
)

// Domains is GitHub's list of the names self-hosted runners reach, as
// `domains.actions_inbound` in `GET /meta` gives it. A wildcard entry
// "*.example.com" lists every name under example.com, never example.com
// itself; a wildcard entry without "*." lists only that name.
type Domains struct {
	Full     []string `json:"full_domains"`
	Wildcard []string `json:"wildcard_domains"`
}

// bootstrap is the list a host holds before its first good read.
var bootstrap = Domains{Full: []string{egress.GitHubAPI}}

// check refuses an empty list and any entry that is not a DNS name.
func (d Domains) check() error {
	if len(d.Full)+len(d.Wildcard) == 0 {
		return errors.New("the domain list is empty")
	}
	var errs []error
	for _, n := range d.Full {
		if !isName(n) {
			errs = append(errs, fmt.Errorf("full domain %q is not a DNS name", n))
		}
	}
	for _, n := range d.Wildcard {
		if !isName(strings.TrimPrefix(n, "*.")) {
			errs = append(errs, fmt.Errorf("wildcard domain %q is not a DNS name or *. and one", n))
		}
	}
	return errors.Join(errs...)
}

// Matcher answers whether a name is on a domain list.
type Matcher struct {
	full     map[string]struct{}
	suffixes []string
}

// newMatcher answers the Matcher for d, which also lists egress.GitHubAPI,
// the name the domain list is read from: without it no list is ever read.
func newMatcher(d Domains) *Matcher {
	m := &Matcher{full: map[string]struct{}{egress.GitHubAPI: {}}}
	for _, n := range d.Full {
		m.full[canonical(n)] = struct{}{}
	}
	for _, n := range d.Wildcard {
		if s, ok := strings.CutPrefix(canonical(n), "*."); ok {
			m.suffixes = append(m.suffixes, "."+s)
		} else {
			m.full[canonical(n)] = struct{}{}
		}
	}
	return m
}

// Match reports whether name, in any case and with or without its trailing
// dot, is on the list.
func (m *Matcher) Match(name string) bool {
	name = canonical(name)
	if _, ok := m.full[name]; ok {
		return true
	}
	for _, s := range m.suffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

func canonical(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// isName reports whether n is a DNS name of letters, digits, dashes and
// underscores in labels of 1 to 63, at most 253 in all.
func isName(n string) bool {
	if n == "" || len(n) > 253 {
		return false
	}
	for label := range strings.SplitSeq(n, ".") {
		if !isLabel(label) {
			return false
		}
	}
	return true
}

func isLabel(l string) bool {
	if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for _, c := range l {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
