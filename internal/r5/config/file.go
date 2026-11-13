package config

import (
	"cmp"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The file's shape. Scalars decode as strings so every field is parsed here
// with its own path in the error, never by go-yaml.
type fileConfig struct {
	Profiles map[string]fileProfile `yaml:"profiles"`
	Capacity struct {
		CPUs       string `yaml:"cpus"`
		Memory     string `yaml:"memory"`
		Disk       string `yaml:"disk"`
		MaxRunners string `yaml:"max_runners"`
	} `yaml:"capacity"`
	Queue struct {
		HeadReserveAfter string `yaml:"head_reserve_after"`
	} `yaml:"queue"`
	Images struct {
		Refresh string `yaml:"refresh"`
	} `yaml:"images"`
	Network struct {
		Subnet       string   `yaml:"subnet"`
		BridgeSubnet string   `yaml:"bridge_subnet"`
		DNS          []string `yaml:"dns"`
	} `yaml:"network"`
	Scopes []fileScope `yaml:"scopes"`
}

type fileScope struct {
	Repo           string `yaml:"repo"`
	Org            string `yaml:"org"`
	Enterprise     string `yaml:"enterprise"`
	RunnerGroup    string `yaml:"runner_group"`
	InstallationID string `yaml:"installation_id"`
}

// Capacity is the pool's budget. Disk and MaxRunners are 0 when unset.
type Capacity struct {
	MilliCPUs  int64
	Memory     int64
	Disk       int64
	MaxRunners int
}

// Network is the job networks' addressing. DNS is empty when jobs inherit
// the daemon's resolvers.
type Network struct {
	Subnet       netip.Prefix
	BridgeSubnet netip.Prefix
	DNS          []netip.Addr
}

// ScopeKind is a scope's registration target.
type ScopeKind int

// The scope kinds; the zero ScopeKind is none of them.
const (
	ScopeRepo ScopeKind = iota + 1
	ScopeOrg
	scopeEnterprise
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
	// InstallationID is 0 when boot resolves it.
	InstallationID int64
	Kind           ScopeKind
}

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

func (f *fileConfig) apply(cfg *Config, p *problems) {
	cfg.Capacity = f.capacity(p)
	cfg.HeadReserveAfter = field(p, "queue.head_reserve_after", f.Queue.HeadReserveAfter, "2m", parseDuration)
	cfg.ImageRefresh = field(p, "images.refresh", f.Images.Refresh, "6h", parseDuration)
	if cfg.ImageRefresh > 0 && cfg.ImageRefresh < 10*time.Minute {
		p.addf("images.refresh", "want at least 10m, got %s", quote(f.Images.Refresh))
	}
	cfg.Network = f.network(p)
	cfg.Scopes = f.scopes(cfg, p)
	cfg.Profiles = f.profiles(cfg, p)
}

func (f *fileConfig) capacity(p *problems) Capacity {
	if f.Capacity.CPUs == "" {
		p.addf("capacity.cpus", "required")
	}
	if f.Capacity.Memory == "" {
		p.addf("capacity.memory", "required")
	}
	return Capacity{
		MilliCPUs:  field(p, "capacity.cpus", f.Capacity.CPUs, "", parseCPUs),
		Memory:     field(p, "capacity.memory", f.Capacity.Memory, "", parsePositiveSize),
		Disk:       field(p, "capacity.disk", f.Capacity.Disk, "", parsePositiveSize),
		MaxRunners: int(field(p, "capacity.max_runners", f.Capacity.MaxRunners, "", positive(10000))),
	}
}

func (f *fileConfig) network(p *problems) Network {
	n := Network{
		Subnet:       field(p, "network.subnet", f.Network.Subnet, "172.30.0.0/24", parseSubnet),
		BridgeSubnet: field(p, "network.bridge_subnet", f.Network.BridgeSubnet, "172.30.1.0/24", parseSubnet),
	}
	if n.Subnet.IsValid() && n.BridgeSubnet.IsValid() && n.Subnet.Overlaps(n.BridgeSubnet) {
		p.addf("network.bridge_subnet", "overlaps network.subnet")
	}
	// IPv6 is off on the job networks, and a loopback resolver would be the
	// job container's own.
	for i, raw := range f.Network.DNS {
		a, err := netip.ParseAddr(raw)
		if err != nil || !a.Is4() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
			p.add("network.dns["+strconv.Itoa(i)+"]", invalid("want a unicast IPv4 resolver address outside loopback", raw))
		}
		n.DNS = append(n.DNS, a)
	}
	return n
}

func (f *fileConfig) scopes(cfg *Config, p *problems) []Scope {
	if len(f.Scopes) == 0 {
		p.addf("scopes", "list at least one scope")
	}
	seen := map[string]bool{}
	var out []Scope
	for i := range f.Scopes {
		path := "scopes[" + strconv.Itoa(i) + "]"
		s, ok := f.Scopes[i].parse(cfg, path, p)
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
func (fs *fileScope) parse(cfg *Config, path string, p *problems) (Scope, bool) {
	before := len(p.errs)
	s := fs.target(cfg, path, p)
	if s.Kind != ScopeRepo {
		s.RunnerGroup = cmp.Or(fs.RunnerGroup, "Default")
	}
	s.InstallationID = field(p, path+".installation_id", fs.InstallationID, "", positive(1<<53))
	return s, len(p.errs) == before
}

// target parses the one registration target a scope names.
func (fs *fileScope) target(cfg *Config, path string, p *problems) Scope {
	named := slices.DeleteFunc([]string{fs.Repo, fs.Org, fs.Enterprise}, func(v string) bool { return v == "" })
	switch {
	case len(named) != 1:
		p.addf(path, "set exactly one of repo, org or enterprise")
		return Scope{}
	case fs.Repo != "":
		owner, repo, _ := strings.Cut(fs.Repo, "/")
		if !ownerRe.MatchString(owner) || !repoRe.MatchString(repo) || repo == "." || repo == ".." {
			p.add(path+".repo", invalid("want owner/name", fs.Repo))
		}
		return Scope{Kind: ScopeRepo, Owner: owner, Repo: repo, Key: fs.Repo}
	case fs.Org != "":
		if !ownerRe.MatchString(fs.Org) {
			p.add(path+".org", invalid("want an organization name", fs.Org))
		}
		return Scope{Kind: ScopeOrg, Owner: fs.Org, Key: fs.Org}
	}
	if !ownerRe.MatchString(fs.Enterprise) {
		p.add(path+".enterprise", invalid("want an enterprise slug", fs.Enterprise))
	}
	if cfg.GitHub.AppKey != nil {
		p.addf(path+".enterprise", "an enterprise scope needs GITHUB_TOKEN: GitHub refuses App authentication for enterprise runners")
	}
	return Scope{Kind: scopeEnterprise, Owner: fs.Enterprise, Key: "enterprises/" + fs.Enterprise}
}
