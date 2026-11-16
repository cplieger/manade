package config

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// document is the settings document's shape, JSON with the field names the
// YAML settings file used. A number, a boolean, a size or a duration decodes
// as a scalar, so the section parsers read it with its own path in the error.
type document struct {
	Hosts    map[string]docHost    `json:"hosts"`
	Profiles map[string]docProfile `json:"profiles"`
	Queue    struct {
		HeadReserveAfter scalar `json:"head_reserve_after"`
	} `json:"queue"`
	Images docImages  `json:"images"`
	Scopes []docScope `json:"scopes"`
}

type docScope struct {
	Repo           string `json:"repo"`
	Org            string `json:"org"`
	Enterprise     string `json:"enterprise"`
	RunnerGroup    string `json:"runner_group"`
	DefaultProfile string `json:"default_profile"`
	State          string `json:"state"`
	InstallationID scalar `json:"installation_id"`
}

// ScopeKind is a scope's registration target.
type ScopeKind int

// The scope kinds; the zero ScopeKind is none of them.
const (
	ScopeRepo ScopeKind = iota + 1
	ScopeOrg
	ScopeEnterprise
)

// Scope is one registration target.
type Scope struct {
	// Key names the scope everywhere (labels, metrics, profile scopes):
	// owner/repo, the organization, or enterprises/<name>, which is also
	// the registration URL's path.
	Key string
	// Owner is the repo's owner, the organization or the enterprise.
	Owner string
	// Repo is set for a repo scope only.
	Repo string
	// RunnerGroup is set for an organization or enterprise scope only.
	RunnerGroup string
	// DefaultProfile is the profile the scope's runner variable names.
	DefaultProfile string
	// path is the scope's error path.
	path string
	// InstallationID is 0 when boot resolves it.
	InstallationID int64
	Kind           ScopeKind
	// Stopping is the pool's state for a scope the user turned off while
	// its scale sets still hold jobs; it is served until they reach 0.
	Stopping bool
}

// The scope states.
const (
	scopeServed   = "served"
	scopeStopping = "stopping"
)

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func (d *document) apply(cfg *Config, p *problems) {
	cfg.HeadReserveAfter = field(p, "queue.head_reserve_after", d.Queue.HeadReserveAfter, "2m", parseDuration)
	refresh := parseRefresh(p, "images.refresh", cmp.Or(string(d.Images.Refresh), "1h"))
	cfg.Scopes = d.scopes(p)
	cfg.Hosts = d.hosts(refresh, p)
	cfg.Profiles = d.profiles(p)
	cfg.check(p)
}

// hosts parses the hosts map, sorted by name.
func (d *document) hosts(refresh time.Duration, p *problems) []Host {
	out := make([]Host, 0, len(d.Hosts))
	for _, name := range slices.Sorted(maps.Keys(d.Hosts)) {
		path := hostPath(name)
		if err := CheckHostName(name); err != nil {
			p.add(path, err)
		}
		dh := d.Hosts[name]
		out = append(out, dh.parse(name, path, refresh, p))
	}
	return out
}

func (d *document) scopes(p *problems) []Scope {
	seen := map[string]bool{}
	var out []Scope
	for i := range d.Scopes {
		path := "scopes[" + strconv.Itoa(i) + "]"
		s, ok := d.Scopes[i].parse(path, p)
		if !ok {
			continue
		}
		if lk := strings.ToLower(s.Key); seen[lk] {
			p.addf(path, "repeats scope %s", quote(s.Key))
		} else {
			seen[lk] = true
			out = append(out, s)
		}
	}
	return out
}

// parse answers the scope and whether it holds no problem. The runner group
// reaches GitHub only as a query value and logs only through textsafe, so it
// takes any name.
func (ds *docScope) parse(path string, p *problems) (Scope, bool) {
	before := len(p.errs)
	s := ds.target(path, p)
	s.path = path
	if s.Kind != ScopeRepo {
		s.RunnerGroup = cmp.Or(ds.RunnerGroup, "Default")
	}
	s.InstallationID = field(p, path+".installation_id", ds.InstallationID, "", positive(1<<53))
	s.DefaultProfile = cmp.Or(ds.DefaultProfile, defaultProfile)
	if !profileNameRe.MatchString(s.DefaultProfile) {
		p.add(path+".default_profile", invalid(profileNameRule, s.DefaultProfile))
	}
	s.Stopping = oneOf(p, path+".state", ds.State, scopeServed, []string{scopeServed, scopeStopping}) == scopeStopping
	return s, len(p.errs) == before
}

// target parses the one registration target a scope names.
func (ds *docScope) target(path string, p *problems) Scope {
	named := slices.DeleteFunc([]string{ds.Repo, ds.Org, ds.Enterprise}, func(v string) bool { return v == "" })
	switch {
	case len(named) != 1:
		p.addf(path, "set exactly one of repo, org or enterprise")
		return Scope{}
	case ds.Repo != "":
		owner, repo, _ := strings.Cut(ds.Repo, "/")
		if !ownerRe.MatchString(owner) || !repoRe.MatchString(repo) || repo == "." || repo == ".." {
			p.add(path+".repo", invalid("want owner/name", ds.Repo))
		}
		return Scope{Kind: ScopeRepo, Owner: owner, Repo: repo, Key: ds.Repo}
	case ds.Org != "":
		if !ownerRe.MatchString(ds.Org) {
			p.add(path+".org", invalid("want an organization name", ds.Org))
		}
		return Scope{Kind: ScopeOrg, Owner: ds.Org, Key: ds.Org}
	}
	if !ownerRe.MatchString(ds.Enterprise) {
		p.add(path+".enterprise", invalid("want an enterprise slug", ds.Enterprise))
	}
	return Scope{Kind: ScopeEnterprise, Owner: ds.Enterprise, Key: "enterprises/" + ds.Enterprise}
}
