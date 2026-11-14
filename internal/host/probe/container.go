package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/cplieger/manade/internal/docker"
	"github.com/cplieger/manade/internal/jobspec"
)

// errInFlight is a daemon call or an attach that timed out: its container
// may still change, so the run leaves it for a later pass.
var errInFlight = errors.New("timed out and may still be in flight")

// started is one of a run's containers, running on the job network.
type started struct {
	stream Stream
	ep     endpoint
}

// endpoint is a container's addresses on the job network.
type endpoint docker.Endpoint

func (e endpoint) addr4() netip.Addr { a, _ := netip.ParseAddr(e.IPAddress); return a }

func (e endpoint) gateway4() netip.Addr { a, _ := netip.ParseAddr(e.Gateway); return a }

func (e endpoint) addr6(on bool) netip.Addr {
	if !on {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(e.GlobalIPv6Address)
	return a
}

func (e endpoint) gateway6(on bool) netip.Addr {
	if !on {
		return netip.Addr{}
	}
	a, _ := netip.ParseAddr(e.IPv6Gateway)
	return a
}

// addrs are the container's addresses, the probe's sources.
func (e endpoint) addrs() []netip.Addr {
	var out []netip.Addr
	for _, a := range []netip.Addr{e.addr4(), e.addr6(true)} {
		if a.IsValid() {
			out = append(out, a)
		}
	}
	return out
}

// container creates and starts run's container of role, attached first
// when stdin is set, and reads its addresses. A create answered with a
// missing image loads the probe image again, once.
func (r *Runner) container(ctx context.Context, run, role string, stdin bool) (*started, error) {
	name := "manade-" + role + "-" + run
	what := "the probe's " + role + " container"
	pr := &jobspec.Probe{Run: run, ImageID: r.cfg.Image.ProbeImage(), Command: listenerCommand, Stdin: stdin}
	if stdin {
		pr.Command = probeCommand
	}
	res := r.cfg.Daemon.Create(ctx, name, r.cfg.Spec().Probe(pr))
	if res.Outcome == docker.Failed && errors.Is(res.Err, docker.ErrImageMissing) {
		id, err := r.cfg.Image.LoadProbe(ctx)
		if err != nil {
			return nil, fmt.Errorf("loading the probe image for %s: %w", what, err)
		}
		pr.ImageID = id
		res = r.cfg.Daemon.Create(ctx, name, r.cfg.Spec().Probe(pr))
	}
	if err := callErr(what+"'s create", res); err != nil {
		return nil, err
	}
	s := &started{}
	if stdin {
		stream, err := r.attach(ctx, res.ID)
		if err != nil {
			return nil, fmt.Errorf("%s's attach %w: %w", what, errInFlight, err)
		}
		s.stream = stream
	}
	if err := callErr(what+"'s start", r.cfg.Daemon.Start(ctx, res.ID)); err != nil {
		closeStream(s.stream)
		return nil, err
	}
	ctr, found, err := r.cfg.Daemon.InspectContainer(ctx, res.ID)
	switch {
	case err != nil:
		closeStream(s.stream)
		return nil, fmt.Errorf("%s's inspect %w: %w", what, errInFlight, err)
	case !found:
		closeStream(s.stream)
		return nil, fmt.Errorf("%s is gone after its start", what)
	}
	s.ep = endpoint(ctr.NetworkSettings.Networks[r.cfg.Network])
	if !s.ep.addr4().IsValid() {
		closeStream(s.stream)
		return nil, fmt.Errorf("%s has no address on network %s", what, r.cfg.Network)
	}
	return s, nil
}

// attach attaches within one call timeout, the 101's bound: the stream it
// answers has no deadline of its own.
func (r *Runner) attach(ctx context.Context, id string) (Stream, error) {
	actx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return r.cfg.Attach(actx, id)
}

func closeStream(s Stream) {
	if s != nil {
		_ = s.Close()
	}
}

// callErr is a create's or start's failure, errInFlight for one pending.
func callErr(what string, res docker.Result) error {
	switch res.Outcome {
	case docker.Done:
		return nil
	case docker.Pending:
		return fmt.Errorf("%s %w: %w", what, errInFlight, res.Err)
	}
	return fmt.Errorf("%s failed: %w", what, res.Err)
}

// remove removes run's two containers.
func (r *Runner) remove(ctx context.Context, run string) {
	for _, role := range []string{roleListen, roleProbe} {
		if err := r.cfg.Daemon.RemoveOwned(ctx, "manade-"+role+"-"+run); err != nil {
			r.cfg.Log.Warn("Removing a probe container failed; a later pass removes it", "run", run, "error", err)
		}
	}
}

// tidy removes each labelled probe container but current's whose run ended
// at least one call timeout ago; one answered 409 is tried at the next pass.
func (r *Runner) tidy(ctx context.Context, current string) {
	list, err := r.cfg.Daemon.ListContainers(ctx)
	if err != nil {
		r.cfg.Log.Warn("Listing leftover probe containers failed", "error", err)
		return
	}
	for i := range list {
		c := &list[i]
		run, ok := c.Labels[jobspec.LabelProbe]
		if !ok || run == current {
			continue
		}
		if ended, running := r.Ended(run); running || time.Since(ended) < callTimeout {
			continue
		}
		if err := r.cfg.Daemon.RemoveOwned(ctx, c.ID); err != nil && !errors.Is(err, docker.ErrConflict) {
			r.cfg.Log.Warn("Removing a leftover probe container failed", "container", c.ID, "error", err)
		}
	}
}
