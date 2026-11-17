//go:build integration

package hostit

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/manade/internal/hostapi"
)

const trueProfile = `"tiny": {"runner_image": "base", "cpus": 1, "memory": "512MiB", "scopes": ["acme/app"], "command": ["/bin/true"]}`

// A host with no section waits on its connection, boots on the first one
// delivered, and refuses one beyond its ceiling, keeping the section and
// rules it runs.
func TestSectionOverTheSocket(t *testing.T) {
	cp := newControlPlane(t)
	h := startHost(t, cp, nil)
	st := h.await("its first status", time.Minute, func(*hostapi.Status) bool { return true })
	if st.Section.State != hostapi.SectionWaiting || st.Ceiling.AllowOpen {
		t.Fatalf("a first boot's status = %+v, ceiling %+v; want waiting with the default ceiling", st.Section, st.Ceiling)
	}
	first := settings(`{}`)
	if ans, err := h.deliver(1, first); err != nil || ans.Hash != hashOf(t, first) {
		t.Fatalf("PUT /v1/section = %+v, %v", ans, err)
	}
	running := h.up(hashOf(t, first))
	_, err := h.deliver(2, settings(`{"egress": {"open": true}}`))
	sr, ok := errors.AsType[*hostapi.SectionRefusedError](err)
	if !ok || !slices.ContainsFunc(sr.Errors, func(e hostapi.FieldError) bool { return e.Path == "hosts.default.egress.open" }) {
		t.Fatalf("an open section on a host without MANADE_ALLOW_OPEN = %v, want refused at hosts.default.egress.open", err)
	}
	after := h.await("the refusal in its status", time.Minute, func(st *hostapi.Status) bool { return st.Section.Refused != nil })
	if after.Section.Hash != running.Section.Hash || after.PolicyHash != running.PolicyHash {
		t.Errorf("after the refusal the host runs %s with policy %s, want %s and %s",
			after.Section.Hash, after.PolicyHash, running.Section.Hash, running.PolicyHash)
	}
}

// A network.subnet change restarts the host: its last events line says so,
// it exits 75, restart: unless-stopped boots it again in a fresh network
// namespace, and it reconnects running the new section on the new subnet.
func TestSubnetChangeRestartsTheHost(t *testing.T) {
	cp := newControlPlane(t)
	h := startHost(t, cp, nil, "--restart", "unless-stopped")
	first := settings(`{"network": {"subnet": "10.231.0.0/24"}}`)
	if _, err := h.deliver(1, first); err != nil {
		t.Fatal(err)
	}
	st := h.up(hashOf(t, first))
	before := h.link("")
	// Rules no boot writes, in the backend the host loads: present after the
	// restart only if the old namespace's rules were kept.
	marker := [2][]string{{"nft", "add", "table", "inet", "manadetest_marker"}, {"nft", "list", "table", "inet", "manadetest_marker"}}
	if st.Rules.Backend == "iptables" {
		marker = [2][]string{{"iptables-legacy", "-N", "MANADETEST_MARKER"}, {"iptables-legacy", "-S", "MANADETEST_MARKER"}}
	}
	docker(t, append([]string{"exec", h.ctr}, marker[0]...)...)
	events := make(chan []hostapi.EventType, 1)
	go func() {
		var got []hostapi.EventType
		_ = before.API.Events(context.Background(), func(e *hostapi.Event) { got = append(got, e.Type) })
		events <- got
	}()
	next := settings(`{"network": {"subnet": "10.232.0.0/24"}}`)
	ans, err := h.deliver(2, next)
	if err != nil || ans.Applies != hostapi.SectionPendingRestart {
		t.Fatalf("PUT of a new subnet = %+v, %v; want pending_restart", ans, err)
	}
	select {
	case got := <-events:
		if len(got) == 0 || got[len(got)-1] != hostapi.EventRestarting {
			t.Errorf("events = %v, want the restarting line last", got)
		}
	case <-time.After(3 * time.Minute):
		t.Fatal("the events stream stayed open past the restart")
	}
	after := h.link(before.Instance)
	h.up(hashOf(t, next))
	if n, _ := strconv.Atoi(docker(t, "inspect", "-f", "{{.RestartCount}}", h.ctr)); n != 1 {
		t.Errorf("restart count = %d, want 1", n)
	}
	if logs, _ := exec.Command("docker", "logs", h.ctr).CombinedOutput(); !strings.Contains(string(logs), "code=75") {
		t.Errorf("the host's log names no exit 75:\n%s", logs)
	}
	if after.Section != "2" {
		t.Errorf("the restarted host's upgrade named revision %q, want 2", after.Section)
	}
	if out, err := exec.Command("docker", append([]string{"exec", h.ctr}, marker[1]...)...).CombinedOutput(); err == nil {
		t.Errorf("the restarted run kept the old namespace's rules:\n%s", out)
	}
	if got := h.inner("network", "inspect", "-f", "{{range .IPAM.Config}}{{.Subnet}} {{end}}", "manade"); !strings.Contains(got, "10.232.0.0/24") || strings.Contains(got, "10.231.0.0/24") {
		t.Errorf("the job network's subnets = %q, want 10.232.0.0/24 alone", got)
	}
}

// A job network removed inside the host is recreated by the next start
// that needs it, the probe's and a runner's.
func TestJobNetworkRecreated(t *testing.T) {
	cp := newControlPlane(t)
	h := startHost(t, cp, []string{"MANADE_ALLOW_OPEN=true"})
	boot := settings(`{"egress": {"open": true}}`)
	if _, err := h.deliver(1, boot); err != nil {
		t.Fatal(err)
	}
	h.up(hashOf(t, boot))
	h.load("base")
	withTiny := settings(`{"egress": {"open": true}}`, trueProfile)
	if _, err := h.deliver(2, withTiny); err != nil {
		t.Fatal(err)
	}
	h.await("profile tiny ready", 5*time.Minute, func(st *hostapi.Status) bool {
		return slices.ContainsFunc(st.Profiles, func(p hostapi.ProfileStatus) bool { return p.Name == "tiny" && p.Ready })
	})
	t.Run("byTheProbe", func(t *testing.T) {
		h.inner("network", "rm", "manade")
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
		defer cancel()
		if _, err := h.api().Probe(ctx, 30*time.Minute); err != nil {
			t.Fatalf("POST /v1/probe with the job network removed: %v", err)
		}
		h.inner("network", "inspect", "manade")
	})
	t.Run("byARunner", func(t *testing.T) {
		h.inner("network", "rm", "manade")
		if r, logs := h.run(t, "tiny", 5*time.Minute); *r.ExitCode != 0 {
			t.Fatalf("exit %d with the job network removed; logs:\n%s", *r.ExitCode, logs)
		}
		h.inner("network", "inspect", "manade")
	})
}

// The control plane's image owns its data and socket directories, so its
// socket is created on an empty named volume whichever of it and its host
// starts first, and the host connects over it.
func TestControlPlaneImageSocket(t *testing.T) {
	needImage(t, appImage())
	needImage(t, baked("host"))
	for _, hostFirst := range []bool{false, true} {
		t.Run("hostFirst="+strconv.FormatBool(hostFirst), func(t *testing.T) {
			id := randHex(t)
			sock, data := "manadetest-sock-"+id, "manadetest-data-"+id
			cpCtr, hostCtr := "manadetest-cp-"+id, "manadetest-host-"+id
			t.Cleanup(func() {
				if t.Failed() {
					for _, c := range []string{cpCtr, hostCtr} {
						out, _ := exec.Command("docker", "logs", "--tail", "100", c).CombinedOutput()
						t.Logf("%s log:\n%s", c, out)
					}
				}
				_ = exec.Command("docker", "rm", "-f", "-v", cpCtr, hostCtr).Run()
				_ = exec.Command("docker", "volume", "rm", sock, data).Run()
			})
			runCP := func() {
				docker(t, "run", "-d", "--name", cpCtr, "-v", sock+":/run/manade", "-v", data+":/data", appImage(), "control-plane")
			}
			runHost := func() {
				docker(t, "run", "-d", "--name", hostCtr, "--privileged", cgroupns(t), "-v", sock+":/run/manade", "-v", "/var/lib/docker", baked("host"))
			}
			if hostFirst {
				runHost()
				awaitLog(t, hostCtr, "The host could not reach the control plane")
				runCP()
			} else {
				runCP()
				runHost()
			}
			awaitLog(t, cpCtr, `msg="A host connected" host=default`)
			awaitOps(t, cpCtr)
			// The host image has a shell; the control plane's has none.
			got := docker(t, "run", "--rm", "--entrypoint", "stat", "-v", sock+":/s", "-v", data+":/d", baked("host"),
				"-c", "%n %u:%g %A", "/s", "/s/hosts.sock", "/d", "/d/manade.db")
			want := []string{"/s 65532:65532 drwxr-xr-x", "/s/hosts.sock 65532:65532 srw-------", "/d 65532:65532 drwxr-xr-x", "/d/manade.db 65532:65532 -rw-------"}
			if lines := strings.Split(got, "\n"); !slices.Equal(lines, want) {
				t.Errorf("the volumes hold\n%s\nwant\n%s", got, strings.Join(want, "\n"))
			}
		})
	}
}

// awaitOps waits for the control plane in container ctr to answer /healthz
// 200 on its ops port's loopback, its first generation booted, and then
// reads it booted on /metrics. The host image's busybox wget sends the
// request from inside ctr's network namespace.
func awaitOps(t *testing.T, ctr string) {
	t.Helper()
	get := func(path string) (string, error) {
		out, err := exec.CommandContext(t.Context(), "docker", "run", "--rm", "--network", "container:"+ctr, "--entrypoint", "wget",
			baked("host"), "-q", "-O", "-", "http://127.0.0.1:7465"+path).CombinedOutput()
		return string(out), err
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		body, err := get("/healthz")
		if err == nil && strings.Contains(body, `"status":"ok"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's /healthz never answered 200: %v %s", ctr, err, body)
		}
	}
	if body, err := get("/metrics"); err != nil || !strings.Contains(body, "\nmanade_booting 0\n") {
		t.Errorf("%s's /metrics = %v, want manade_booting 0:\n%s", ctr, err, body)
	}
}

// awaitLog waits for container ctr's log to hold line.
func awaitLog(t *testing.T, ctr, line string) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		logs, _ := exec.Command("docker", "logs", ctr).CombinedOutput()
		if strings.Contains(string(logs), line) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s logged no %q:\n%s", ctr, line, logs)
		}
	}
}
