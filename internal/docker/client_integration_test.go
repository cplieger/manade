//go:build integration

package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// The suite runs against a real daemon at DOCKER_SOCKET, or Docker's default
// socket: go test -tags integration ./internal/docker/.
const integrationImage = "busybox:1.37"

func realClient(t *testing.T, pool string) *Client {
	t.Helper()
	socket := os.Getenv("DOCKER_SOCKET")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	c, err := Dial(t.Context(), socket, pool)
	if err != nil {
		t.Fatalf("Dial %s: %v", socket, err)
	}
	return c
}

func testPool(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "mt" + hex.EncodeToString(b)
}

func TestIntegrationDaemon(t *testing.T) {
	c := realClient(t, testPool(t))
	info, err := c.Info(t.Context())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if err := info.CheckLimits(); err != nil {
		t.Errorf("CheckLimits: %v", err)
	}
	if !slices.Contains(info.Runtimes, "runc") || info.DataRoot == "" {
		t.Errorf("Info = %+v, want runc and a data root", info)
	}
	if _, err := c.SocketGID(); err != nil {
		t.Errorf("SocketGID: %v", err)
	}
}

func TestIntegrationNetwork(t *testing.T) {
	pool := testPool(t)
	c := realClient(t, pool)
	spec := NetworkSpec{Name: pool + "-isolated", Bridge: pool + "0", Subnet: netip.MustParsePrefix("172.31.250.0/24")}
	t.Cleanup(func() {
		_ = c.call(context.Background(), "network", http.MethodDelete, "/networks/"+spec.Name, nil, nil, nil, 0)
	})
	if err := c.EnsureNetwork(t.Context(), spec); err != nil {
		t.Fatalf("EnsureNetwork create: %v", err)
	}
	if err := c.EnsureNetwork(t.Context(), spec); err != nil {
		t.Errorf("EnsureNetwork on the network it created: %v", err)
	}
	other := spec
	other.Bridge = pool + "9"
	if err := c.EnsureNetwork(t.Context(), other); !errors.Is(err, errNetworkMismatch) {
		t.Errorf("EnsureNetwork with another bridge name = %v, want errNetworkMismatch", err)
	}
	other = spec
	other.ICC = true
	if err := c.EnsureNetwork(t.Context(), other); !errors.Is(err, errNetworkMismatch) {
		t.Errorf("EnsureNetwork with ICC on = %v, want errNetworkMismatch", err)
	}
}

func TestIntegrationContainerLifecycle(t *testing.T) {
	pool := testPool(t)
	c := realClient(t, pool)
	ctx := t.Context()
	if err := c.PullImage(ctx, integrationImage); err != nil {
		t.Fatalf("PullImage: %v", err)
	}
	img, found, err := c.InspectImage(ctx, integrationImage)
	if err != nil || !found {
		t.Fatalf("InspectImage = %v, %v", found, err)
	}
	if err := c.RemoveOwnedImage(ctx, img.ID); !errors.Is(err, ErrNotOwned) {
		t.Errorf("RemoveOwnedImage on a tagged, unlabelled image = %v, want ErrNotOwned", err)
	}

	events := make(chan string, 8)
	evctx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	go func() { _ = c.Events(evctx, func(id string) { events <- id }) }()

	// The container's own 3-second sleep gives the event stream time to
	// subscribe; the API has no subscription signal to wait on.
	pids, initOn := int64(64), true
	id, err := c.CreateContainer(ctx, pool+"-run", &CreateRequest{
		Image:      img.ID,
		Entrypoint: []string{"/bin/sh", "-c", "sleep 3; echo hello; echo oops >&2; exit 3"},
		Labels:     map[string]string{LabelRunner: pool + "-run"},
		HostConfig: HostConfig{
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges:true"},
			Memory: 64 << 20, MemorySwap: 64 << 20, NanoCPUs: 5e8, PidsLimit: &pids, Init: &initOn,
			NetworkMode: "none", LogConfig: LogConfig{Type: "json-file", Config: map[string]string{"max-size": "10m", "max-file": "3"}},
		},
	})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	t.Cleanup(func() { _ = c.RemoveOwned(context.Background(), id) })
	if err := c.StartContainer(ctx, id); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	waitEvent(t, events, id)

	ctr, found, err := c.InspectContainer(ctx, id)
	if err != nil || !found || ctr.State.ExitCode != 3 || ctr.State.FinishedAt.IsZero() || ctr.Config.Labels[labelPool] != pool || ctr.HostConfig.Memory != 64<<20 {
		t.Errorf("InspectContainer = %+v, %v, %v", ctr, found, err)
	}
	lines, err := c.Logs(ctx, id, 50)
	if err != nil || !slices.Equal(lines, []string{"hello", "oops"}) {
		t.Errorf("Logs = %q, %v", lines, err)
	}
	passwd, found, err := c.ReadFile(ctx, id, "/etc/passwd", 1<<20)
	if err != nil || !found || !strings.HasPrefix(string(passwd), "root:x:0:0:") {
		t.Errorf("ReadFile /etc/passwd = %q, %v, %v", passwd, found, err)
	}
	list, err := c.ListContainers(ctx)
	if err != nil || len(list) != 1 || list[0].ID != id || list[0].State != "exited" {
		t.Errorf("ListContainers = %+v, %v", list, err)
	}

	foreign := realClient(t, testPool(t))
	fid, err := foreign.CreateContainer(ctx, pool+"-foreign", &CreateRequest{Image: img.ID, Entrypoint: []string{"true"}, HostConfig: HostConfig{NetworkMode: "none"}})
	if err != nil {
		t.Fatalf("CreateContainer in another pool: %v", err)
	}
	t.Cleanup(func() { _ = foreign.RemoveOwned(context.Background(), fid) })
	if err := c.RemoveOwned(ctx, fid); !errors.Is(err, ErrNotOwned) {
		t.Errorf("RemoveOwned of another pool's container = %v, want ErrNotOwned", err)
	}

	if err := c.RemoveOwned(ctx, id); err != nil {
		t.Fatalf("RemoveOwned: %v", err)
	}
	if _, found, err := c.InspectContainer(ctx, id); found || err != nil {
		t.Errorf("InspectContainer after removal = %v, %v", found, err)
	}
}

func waitEvent(t *testing.T, events <-chan string, id string) {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case got := <-events:
			if got == id {
				return
			}
		case <-timeout:
			t.Fatalf("no die event for %s within 30 s", id)
		}
	}
}
