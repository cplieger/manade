package host

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host/platform"
)

// xtable is one iptables table: its chains in declaration order.
type xtable struct {
	chains map[string][]string
	order  []string
}

func newTable(builtin ...string) *xtable {
	t := &xtable{chains: map[string][]string{}}
	for _, c := range builtin {
		t.declare(c)
	}
	return t
}

func (t *xtable) declare(c string) {
	if _, ok := t.chains[c]; !ok {
		t.order = append(t.order, c)
	}
	t.chains[c] = []string{}
}

// fakeIPT is a kernel with only ip_tables and ip_set, as the floor has,
// driven through the legacy binaries: restore, save, -S, -I and -D act on
// in-memory tables as the real ones act on the kernel's.
type fakeIPT struct {
	tables     map[bool]map[string]*xtable
	sets       map[string]string
	fail       map[string]error
	failOnce   map[string]error
	ran        []string
	nftFiles   []string
	ipsetFiles []string
	mu         sync.Mutex
	nft        bool
	noSetMatch bool
}

// newFakeIPT has a filter table in both families and no raw table; nat6
// adds ip6tables' nat table.
func newFakeIPT(nat6 bool) *fakeIPT {
	f := &fakeIPT{
		tables: map[bool]map[string]*xtable{
			false: {"filter": newTable("INPUT", "FORWARD", "OUTPUT"), "nat": newTable("PREROUTING", "INPUT", "OUTPUT", "POSTROUTING")},
			true:  {"filter": newTable("INPUT", "FORWARD", "OUTPUT")},
		},
		sets:     map[string]string{},
		fail:     map[string]error{},
		failOnce: map[string]error{},
	}
	if nat6 {
		f.tables[true]["nat"] = newTable("PREROUTING", "INPUT", "OUTPUT", "POSTROUTING")
	}
	return f
}

func (f *fakeIPT) run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.ran = append(f.ran, line)
	for k, err := range f.fail {
		if strings.HasPrefix(line, k) {
			return nil, err
		}
	}
	for k, err := range f.failOnce {
		if strings.HasPrefix(line, k) {
			delete(f.failOnce, k)
			return nil, err
		}
	}
	base := filepath.Base(name)
	if strings.HasPrefix(base, "ip") && strings.Contains(base, "tables") && filepath.Dir(name) != platform.LegacyDir {
		return nil, fmt.Errorf("%s: the nf_tables variant on a kernel without nf_tables", name)
	}
	v6 := strings.HasPrefix(base, "ip6")
	switch base {
	case "nft":
		if !f.nft {
			return nil, errors.New("nft: no nf_tables")
		}
		if slices.Equal(args, []string{"-f", "-"}) {
			f.nftFiles = append(f.nftFiles, string(stdin))
		}
		if slices.Equal(args, []string{"-j", "list", "ruleset"}) {
			return []byte(`{"nftables":[{"metainfo":{"json_schema_version":1}}]}`), nil
		}
		return nil, nil
	case "iptables", "ip6tables":
		return f.iptables(v6, args)
	case "iptables-restore", "ip6tables-restore":
		return nil, f.restore(v6, stdin)
	case "iptables-save", "ip6tables-save":
		return f.save(v6), nil
	case "ipset":
		return f.ipset(stdin, args)
	}
	return nil, fmt.Errorf("fake kernel: unknown command %s", line)
}

func (f *fakeIPT) table(v6 bool, name string) (*xtable, error) {
	t, ok := f.tables[v6][name]
	if !ok {
		return nil, fmt.Errorf("can't initialize %s table `%s': Table does not exist", map[bool]string{false: "iptables", true: "ip6tables"}[v6], name)
	}
	return t, nil
}

func (f *fakeIPT) iptables(v6 bool, args []string) ([]byte, error) {
	a := slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "-w" })
	if len(a) < 3 || a[0] != "-t" {
		return nil, fmt.Errorf("fake iptables: %q", args)
	}
	t, err := f.table(v6, a[1])
	if err != nil {
		return nil, err
	}
	op, chain := a[2], ""
	if len(a) > 3 {
		chain = a[3]
	}
	switch {
	case op == "-n" && len(a) == 4 && a[3] == "-L":
		return nil, nil
	case op == "-S":
		var b strings.Builder
		for _, r := range t.chains[chain] {
			b.WriteString("-A " + chain + " " + r + "\n")
		}
		return []byte(b.String()), nil
	case op == "-I" && len(a) == 7 && a[4] == "1" && a[5] == "-j":
		t.chains[chain] = slices.Insert(t.chains[chain], 0, "-j "+a[6])
		return nil, nil
	case op == "-D" && len(a) == 6 && a[4] == "-j":
		i := slices.Index(t.chains[chain], "-j "+a[5])
		if i < 0 {
			return nil, errors.New("Bad rule (does a matching rule exist in that chain?)")
		}
		t.chains[chain] = slices.Delete(t.chains[chain], i, i+1)
		return nil, nil
	}
	return nil, fmt.Errorf("fake iptables: %q", args)
}

// restore applies a --noflush payload: a declared chain is emptied, or
// created, and each -A appends.
func (f *fakeIPT) restore(v6 bool, payload []byte) error {
	var t *xtable
	sc := bufio.NewScanner(bytes.NewReader(payload))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "*"):
			var err error
			if t, err = f.table(v6, line[1:]); err != nil {
				return err
			}
		case line == "COMMIT":
			t = nil
		case strings.HasPrefix(line, ":"):
			name, _, _ := strings.Cut(line[1:], " ")
			t.declare(name)
		case strings.HasPrefix(line, "-A "):
			chain, rule, _ := strings.Cut(line[3:], " ")
			if strings.Contains(rule, "--match-set") && f.noSetMatch {
				return errors.New("Couldn't load match `set':No such file or directory")
			}
			t.chains[chain] = append(t.chains[chain], rule)
		}
	}
	return nil
}

func (f *fakeIPT) save(v6 bool) []byte {
	var b strings.Builder
	b.WriteString("# Generated by fake ip(6)tables-save\n")
	for _, name := range []string{"filter", "nat"} {
		t, ok := f.tables[v6][name]
		if !ok {
			continue
		}
		b.WriteString("*" + name + "\n")
		for _, c := range t.order {
			b.WriteString(":" + c + " ACCEPT [0:0]\n")
		}
		for _, c := range t.order {
			for _, r := range t.chains[c] {
				b.WriteString("-A " + c + " " + r + "\n")
			}
		}
		b.WriteString("COMMIT\n")
	}
	return []byte(b.String())
}

func (f *fakeIPT) ipset(stdin []byte, args []string) ([]byte, error) {
	switch strings.Join(args, " ") {
	case "list -n":
		return nil, nil
	case "restore -exist":
		f.ipsetFiles = append(f.ipsetFiles, string(stdin))
		for line := range strings.Lines(string(stdin)) {
			fs := strings.Fields(line)
			if len(fs) > 2 && fs[0] == "create" {
				f.sets[fs[1]] = "Header: " + strings.Join(fs[3:], " ")
			}
		}
		return nil, nil
	case "list -t":
		var b strings.Builder
		for _, n := range slices.Sorted(maps.Keys(f.sets)) {
			fmt.Fprintf(&b, "Name: %s\nType: hash:ip\nRevision: 6\n%s\nNumber of entries: 0\n\n", n, f.sets[n])
		}
		return []byte(b.String()), nil
	}
	return nil, fmt.Errorf("fake ipset: %q", args)
}

// chain answers a chain's rules.
func (f *fakeIPT) chain(v6 bool, table, chain string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.tables[v6][table].chains[chain])
}

// edit changes the tables from outside, as a job or the daemon would.
func (f *fakeIPT) edit(fn func(tables map[bool]map[string]*xtable)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f.tables)
}

// payloads answers the nft and ipset payloads loaded, in order.
func (f *fakeIPT) payloads() (nft, ipset []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nftFiles), slices.Clone(f.ipsetFiles)
}

func (f *fakeIPT) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ran)
}

// tree writes files, each path absolute under the answered Root.
func tree(t *testing.T, files map[string]string) platform.Root {
	t.Helper()
	dir := t.TempDir()
	for p, body := range files {
		writeAt(t, platform.Root(dir), p, body)
	}
	return platform.Root(dir)
}

func writeAt(t *testing.T, r platform.Root, p, body string) {
	t.Helper()
	full := r.Path(p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readAt(t *testing.T, r platform.Root, p string) string {
	t.Helper()
	b, err := os.ReadFile(r.Path(p))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }

// section is an internet host named "lan" with the default resolvers and
// the given allow entries.
func section(allow ...string) *config.Host {
	h := &config.Host{Name: "lan", DNS: config.DefaultDNS, Egress: config.Egress{Internet: true}}
	for _, a := range allow {
		h.Egress.Allow = append(h.Egress.Allow, config.Allow{
			To: pfx(a), Protocol: config.ProtocolTCP, Ports: []config.PortRange{{First: 5432, Last: 5432}},
		})
	}
	return h
}

// idleConn is a DNS listener that never receives: its reads block on a
// channel, which a synctest bubble counts as durably blocked, until Close
// or fail ends them.
type idleConn struct {
	err    error
	closed chan struct{}
	once   sync.Once
}

func newIdleConn() *idleConn { return &idleConn{closed: make(chan struct{})} }

func (c *idleConn) end(err error) {
	c.once.Do(func() {
		c.err = err
		close(c.closed)
	})
}

func (c *idleConn) wait() error {
	<-c.closed
	return c.err
}

// isClosed reports whether something closed or failed the listener.
func (c *idleConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *idleConn) ReadFrom([]byte) (int, net.Addr, error)  { return 0, nil, c.wait() }
func (*idleConn) WriteTo(b []byte, _ net.Addr) (int, error) { return len(b), nil }
func (c *idleConn) Accept() (net.Conn, error)               { return nil, c.wait() }
func (c *idleConn) Close() error                            { c.end(net.ErrClosed); return nil }
func (c *idleConn) LocalAddr() net.Addr                     { return c.Addr() }
func (*idleConn) Addr() net.Addr                            { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53} }
func (*idleConn) SetDeadline(time.Time) error               { return nil }
func (*idleConn) SetReadDeadline(time.Time) error           { return nil }
func (*idleConn) SetWriteDeadline(time.Time) error          { return nil }
