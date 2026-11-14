package platform

import (
	"slices"
	"testing"
)

func TestParseMountinfo(t *testing.T) {
	ms, err := parseMountinfo("" +
		"36 35 98:0 /mnt1 /mnt\\040two rw,noatime master:1 - ext3 /dev/root rw,errors=continue\n" +
		"37 35 0:5 / /sys/fs/cgroup rw,memory_localevents shared:2 master:3 - cgroup2 cgroup rw,nsdelegate\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("got %d mounts, want 2", len(ms))
	}
	m := ms[0]
	if m.Root != "/mnt1" || m.Point != "/mnt two" || m.FSType != "ext3" || m.Super != "rw,errors=continue" {
		t.Errorf("mount 0 = %+v", m)
	}
	if ms[1].hasSuper("memory_localevents") || !ms[1].hasSuper("nsdelegate") {
		t.Errorf("mount 1 super options %q: the mount option counted as a super option", ms[1].Super)
	}
}

func TestParseMountinfoRefusesAShortLine(t *testing.T) {
	for _, s := range []string{"36 35 98:0 / / rw ext3\n", "36 35 98:0 / / - ext3\n", "36 35 98:0 / / rw - ext3 /dev\n"} {
		if _, err := parseMountinfo(s); err == nil {
			t.Errorf("parseMountinfo(%q) = nil error", s)
		}
	}
}

func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{`/a\040b`: "/a b", `/a\134b`: `/a\b`, `/a\011`: "/a\t", `/a\9`: `/a\9`, `/a\`: `/a\`} {
		if got := unescape(in); got != want {
			t.Errorf("unescape(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStartMounts(t *testing.T) {
	ms, err := Mountinfo(tree(t, v2Layout()))
	if err != nil {
		t.Fatal(err)
	}
	ms = append(ms, Mount{Point: "/etc/hosts"}, Mount{Point: "/etc/hostname"}, Mount{Point: "/srv/cache"}, Mount{Point: "/srv/cache"})
	got := StartMounts(ms)
	want := []string{"/sys/fs/cgroup", "/var/lib/docker", "/srv/cache"}
	if !slices.Equal(got, want) {
		t.Errorf("StartMounts = %q, want %q", got, want)
	}
}
