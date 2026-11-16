package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"

	"github.com/cplieger/manade/internal/docker"
)

// jobNetworkSpec is the job network the rules match.
func jobNetworkSpec(subnet, subnet6 netip.Prefix) *docker.NetworkSpec {
	return &docker.NetworkSpec{Name: jobNetwork, Bridge: jobBridge, Subnet: subnet, Subnet6: subnet6}
}

// jobDaemon is the host daemon as runners and the probe start containers on
// the job network: a start the daemon refuses because the network is gone,
// which a prune inside the host removes while nothing is attached, runs
// EnsureNetwork and is issued once more.
type jobDaemon struct {
	*docker.Client
	network *docker.NetworkSpec
	log     *slog.Logger
	// mu serialises the recreation, so two starts that both find the
	// network gone never race each other's create.
	mu sync.Mutex
}

var _ runnerDaemon = (*jobDaemon)(nil)

// Start starts container id, recreating the job network once when the
// daemon refuses the start for its absence.
func (d *jobDaemon) Start(ctx context.Context, id string) docker.Result {
	res := d.Client.Start(ctx, id)
	if res.Outcome != docker.Failed || !errors.Is(res.Err, docker.ErrNetworkMissing) {
		return res
	}
	d.log.Warn("The job network is gone, so it is created again and the start issued once more", "container", id, "error", res.Err)
	d.mu.Lock()
	err := d.EnsureNetwork(ctx, d.network)
	d.mu.Unlock()
	if err != nil {
		res.Err = fmt.Errorf("%w; creating the job network again: %w", res.Err, err)
		return res
	}
	return d.Client.Start(ctx, id)
}
