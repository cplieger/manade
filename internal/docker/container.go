package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/go/textsafe"
)

// The labels this package owns. LabelManaged marks every container and
// volume the host created, and only those are removed; LabelRunnerImage is baked into
// each shipped runner image at build, and LabelProbeImage into the probe
// image the host builds.
const (
	LabelManaged     = "manade.managed"
	labelCreated     = "manade.created"
	LabelRunnerImage = "manade.runner-image"
	LabelProbeImage  = "manade.probe-image"

	managedValue = "true"
)

// createPoll is how often a create that timed out is inspected again while
// its container has not appeared.
const createPoll = 5 * time.Second

// CreateRequest is a container create body, in the Engine API's field names.
type CreateRequest struct {
	Labels     map[string]string `json:"Labels,omitempty"`
	Image      string            `json:"Image"`
	User       string            `json:"User,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
	// OpenStdin and StdinOnce, with AttachStdin and AttachStdout, let an
	// Attach before the start step the container over its standard input.
	OpenStdin    bool `json:"OpenStdin,omitempty"`
	StdinOnce    bool `json:"StdinOnce,omitempty"`
	AttachStdin  bool `json:"AttachStdin,omitempty"`
	AttachStdout bool `json:"AttachStdout,omitempty"`
	AttachStderr bool `json:"AttachStderr,omitempty"`
}

// HostConfig is the subset of the create body's HostConfig the host sets.
type HostConfig struct {
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`
	StorageOpt  map[string]string `json:"StorageOpt,omitempty"`
	PidsLimit   *int64            `json:"PidsLimit,omitempty"`
	Init        *bool             `json:"Init,omitempty"`
	LogConfig   LogConfig         `json:"LogConfig"`
	NetworkMode string            `json:"NetworkMode,omitempty"`
	Runtime     string            `json:"Runtime,omitempty"`
	CpusetCpus  string            `json:"CpusetCpus,omitempty"`
	Mounts      []Mount           `json:"Mounts,omitempty"`
	CapAdd      []string          `json:"CapAdd,omitempty"`
	CapDrop     []string          `json:"CapDrop,omitempty"`
	SecurityOpt []string          `json:"SecurityOpt,omitempty"`
	GroupAdd    []string          `json:"GroupAdd,omitempty"`
	Memory      int64             `json:"Memory,omitempty"`
	MemorySwap  int64             `json:"MemorySwap,omitempty"`
	NanoCPUs    int64             `json:"NanoCpus,omitempty"`
	ShmSize     int64             `json:"ShmSize,omitempty"`
	Privileged  bool              `json:"Privileged,omitempty"`
}

// LogConfig is the container's log driver and its options.
type LogConfig struct {
	Config map[string]string `json:"Config,omitempty"`
	Type   string            `json:"Type,omitempty"`
}

// Mount is one HostConfig.Mounts entry: a named volume or a bind.
type Mount struct {
	Type     string `json:"Type"`
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly,omitempty"`
}

// Outcome is how a create or start ended.
type Outcome int

// The outcomes.
const (
	// Done is a create that made its container, or a start that started
	// it (it may have exited since).
	Done Outcome = iota + 1
	// Failed is a call the daemon refused, or a create whose container
	// never appeared or drew warnings; Result.Err says why. Nothing is in
	// flight, and an ID with it is a container left for the caller to
	// remove.
	Failed
	// Pending is a call whose fate is unknown because a call timed out or
	// ctx ended: the caller makes the same call again later (a Create
	// again under the same name) and never removes the container meanwhile.
	Pending
)

// Result is a Create or Start's outcome. ID is the container's, once known.
type Result struct {
	Err     error
	ID      string
	Outcome Outcome
}

// errCreateWarnings is a create the daemon answered with warnings, such as a
// limit the kernel discarded. The container is already removed, and the
// error's text carries the warnings sanitized.
var errCreateWarnings = errors.New("docker create: the daemon warned")

// Create creates a container named name, labelled managed over any labels in
// req. A create refused because name is in use, or whose call timed out, is
// followed by an inspect of name, and after a timeout a 404 is inspected again
// for one more CallTimeout before the create fails. A container found so is
// this create's own only when it is managed and carries every label of req;
// another holder fails the create with no ID (ErrConflict for a name in use),
// and its own lacking a limit req sets fails with its ID and errLimitDiscarded.
// A create refused for its image fails with an error matching ErrImageMissing.
func (c *Client) Create(ctx context.Context, name string, req *CreateRequest) Result {
	body := *req
	body.Labels = c.managedLabels(req.Labels)
	var out struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	timedOut, err := timed(ctx, func(ctx context.Context) error {
		return c.callIn(ctx, "create", http.MethodPost, "/containers/create", url.Values{"name": {name}}, &body, &out, jsonLimit)
	})
	switch {
	case err == nil:
		return c.created(ctx, out.ID, out.Warnings)
	case ctx.Err() != nil:
		return Result{Outcome: Pending, Err: err}
	case errors.Is(err, ErrConflict):
		return c.adopt(ctx, name, req, err)
	case !timedOut:
		return Result{Outcome: Failed, Err: err}
	}
	return c.afterCreateTimeout(ctx, name, req, err)
}

// adopt judges a create of req refused because name is in use (ownIfMine).
func (c *Client) adopt(ctx context.Context, name string, req *CreateRequest, callErr error) Result {
	ctr, found, err := c.InspectContainer(ctx, name)
	switch {
	case err != nil:
		return Result{Outcome: Pending, Err: errors.Join(callErr, err)}
	case !found:
		return Result{Outcome: Failed, Err: callErr}
	}
	return ownIfMine(&ctr, req, callErr)
}

// ownIfMine judges ctr, found under the name a create of req used: with
// its limits (withLimits) when it is managed and carries every label of
// req, Failed with no ID and callErr otherwise, so the caller never removes
// a container it does not own.
func ownIfMine(ctr *Container, req *CreateRequest, callErr error) Result {
	if ctr.Config.Labels[LabelManaged] != managedValue {
		return Result{Outcome: Failed, Err: callErr}
	}
	for k, v := range req.Labels {
		if k != LabelManaged && k != labelCreated && ctr.Config.Labels[k] != v {
			return Result{Outcome: Failed, Err: callErr}
		}
	}
	return withLimits(ctr, &req.HostConfig)
}

// errLimitDiscarded is a container found after a create whose answer the
// client never read that lacks a limit the create set: the daemon discarded
// it with a warning the client did not see.
var errLimitDiscarded = errors.New("docker create: the container lacks a requested limit")

// withLimits is Done for ctr when it has every limit want sets, and Failed
// with its ID otherwise: the daemon discards some limits with only a warning
// (verifyPlatformContainerResources,
// https://github.com/moby/moby/blob/docker-v29.0.0/daemon/daemon_unix.go).
func withLimits(ctr *Container, want *HostConfig) Result {
	got := &ctr.HostConfig
	var field string
	var have, asked any
	switch {
	case want.Memory != 0 && got.Memory != want.Memory:
		field, have, asked = "Memory", got.Memory, want.Memory
	case want.MemorySwap != 0 && got.MemorySwap != want.MemorySwap:
		field, have, asked = "MemorySwap", got.MemorySwap, want.MemorySwap
	case want.NanoCPUs != 0 && got.NanoCPUs != want.NanoCPUs:
		field, have, asked = "NanoCpus", got.NanoCPUs, want.NanoCPUs
	case want.CpusetCpus != "" && got.CpusetCpus != want.CpusetCpus:
		field, have, asked = "CpusetCpus", strconv.Quote(got.CpusetCpus), strconv.Quote(want.CpusetCpus)
	case want.PidsLimit != nil && (got.PidsLimit == nil || *got.PidsLimit != *want.PidsLimit):
		field, have, asked = "PidsLimit", pids(got.PidsLimit), *want.PidsLimit
	case want.StorageOpt["size"] != "" && got.StorageOpt["size"] != want.StorageOpt["size"]:
		field, have, asked = "StorageOpt size", strconv.Quote(got.StorageOpt["size"]), strconv.Quote(want.StorageOpt["size"])
	default:
		return Result{Outcome: Done, ID: ctr.ID}
	}
	return Result{Outcome: Failed, ID: ctr.ID, Err: fmt.Errorf("%w: container %s: %s is %v, the create set %v", errLimitDiscarded, ctr.ID, field, have, asked)}
}

func pids(p *int64) any {
	if p == nil {
		return "unset"
	}
	return *p
}

// created removes a container the daemon warned about and fails it.
func (c *Client) created(ctx context.Context, id string, warnings []string) Result {
	warnings = slices.DeleteFunc(warnings, func(w string) bool { return strings.TrimSpace(w) == "" })
	if len(warnings) == 0 {
		return Result{Outcome: Done, ID: id}
	}
	text, _ := textsafe.SanitizeSingleLineCapped(strings.Join(warnings, "; "), 4*errorTextLimit, "…")
	werr := fmt.Errorf("%w: %s", errCreateWarnings, text)
	if err := c.remove(ctx, id); err != nil {
		return Result{Outcome: Failed, ID: id, Err: errors.Join(werr, err)}
	}
	return Result{Outcome: Failed, Err: werr}
}

// afterCreateTimeout judges a create of req whose call timed out by the
// container InspectSettled finds under name (ownIfMine). The window's end
// fails the create only when the last inspect answered 404; an inspect that
// errs, the window's end cutting it included, leaves it pending.
func (c *Client) afterCreateTimeout(ctx context.Context, name string, req *CreateRequest, callErr error) Result {
	ctr, found, err := c.InspectSettled(ctx, name)
	switch {
	case err != nil:
		return Result{Outcome: Pending, Err: errors.Join(callErr, err)}
	case found:
		return ownIfMine(&ctr, req, callErr)
	}
	return Result{Outcome: Failed, Err: fmt.Errorf("%w; no container %s appeared within a further %s", callErr, name, CallTimeout)}
}

// InspectSettled inspects name the way a create that may still commit
// needs: until a container appears, or one more CallTimeout passes with
// every inspect answering 404, which alone answers not found. An inspect
// that errs, or ctx ending, answers the error.
func (c *Client) InspectSettled(ctx context.Context, name string) (Container, bool, error) {
	end := time.Now().Add(CallTimeout)
	wctx, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	tick := time.NewTicker(createPoll)
	defer tick.Stop()
	for {
		ctr, found, err := c.inspectIn(wctx, name)
		if err != nil || found {
			return ctr, found, err
		}
		select {
		case <-ctx.Done():
			return Container{}, false, ctx.Err()
		case <-wctx.Done():
			return Container{}, false, nil
		case <-tick.C:
		}
		// A tick ready together with the window's end must not start an
		// inspect the window would cut; wctx.Err may not be set yet then.
		if !time.Now().Before(end) && ctx.Err() == nil {
			return Container{}, false, nil
		}
	}
}

// Start starts container id. A start that times out is followed by an
// inspect: a container past created counts as started, and one still
// created gets the start issued again, whose own error alone fails the
// start. Docker's 304 for a container already running is a success.
func (c *Client) Start(ctx context.Context, id string) Result {
	timedOut, err := c.startOnce(ctx, id)
	switch {
	case err == nil:
		return Result{Outcome: Done, ID: id}
	case ctx.Err() != nil:
		return Result{Outcome: Pending, ID: id, Err: err}
	case !timedOut:
		return Result{Outcome: Failed, ID: id, Err: err}
	}
	var ctr Container
	var found bool
	timedOut, ierr := timed(ctx, func(ctx context.Context) error {
		var ierr error
		ctr, found, ierr = c.inspectIn(ctx, id)
		return ierr
	})
	switch {
	case ierr != nil || timedOut:
		return Result{Outcome: Pending, ID: id, Err: errors.Join(err, ierr)}
	case !found:
		return Result{Outcome: Failed, ID: id, Err: fmt.Errorf("%w; the container is gone", err)}
	case ctr.State.Status != StateCreated:
		return Result{Outcome: Done, ID: id}
	}
	timedOut, err = c.startOnce(ctx, id)
	switch {
	case err == nil:
		return Result{Outcome: Done, ID: id}
	case timedOut || ctx.Err() != nil:
		return Result{Outcome: Pending, ID: id, Err: err}
	}
	return Result{Outcome: Failed, ID: id, Err: err}
}

func (c *Client) startOnce(ctx context.Context, id string) (timedOut bool, err error) {
	timedOut, err = timed(ctx, func(ctx context.Context) error {
		return c.callIn(ctx, "start", http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil, 0)
	})
	if e, ok := errors.AsType[*daemonError](err); ok && e.Code == http.StatusNotModified {
		return false, nil
	}
	return timedOut, err
}

// The container states the daemon reports, as it names them.
const (
	StateCreated    = "created"
	StateRunning    = "running"
	StatePaused     = "paused"
	StateRestarting = "restarting"
	StateRemoving   = "removing"
	StateExited     = "exited"
	StateDead       = "dead"
)

// Summary is one entry of ListContainers.
type Summary struct {
	Labels map[string]string
	ID     string `json:"Id"`
	// State is the daemon's state word, one of the State constants.
	State string
}

// ListContainers lists every managed container, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]Summary, error) {
	q := managedFilter(nil)
	q.Set("all", "1")
	var out []Summary
	if err := c.call(ctx, "list", http.MethodGet, "/containers/json", q, nil, &out, listLimit); err != nil {
		return nil, err
	}
	return out, nil
}

// Endpoint is a container's address on one network.
type Endpoint struct {
	IPAddress         string
	Gateway           string
	GlobalIPv6Address string
	IPv6Gateway       string
}

// Container is a container's inspect answer, in the Engine API's shape.
type Container struct {
	Config struct {
		Labels map[string]string
	}
	NetworkSettings struct {
		Networks map[string]Endpoint
	}
	ID    string `json:"Id"`
	State struct {
		StartedAt  time.Time
		FinishedAt time.Time
		// Status is one of the State constants.
		Status    string
		ExitCode  int
		OOMKilled bool
	}
	// HostConfig holds the limits the container was built with.
	HostConfig struct {
		StorageOpt map[string]string
		PidsLimit  *int64
		CpusetCpus string
		NanoCPUs   int64 `json:"NanoCpus"`
		Memory     int64
		MemorySwap int64
	}
}

// DiskBytes is the writable layer's size limit, 0 when none is set. The host
// writes it as a plain byte count; any other form fails.
func (ctr *Container) DiskBytes() (int64, error) {
	s, ok := ctr.HostConfig.StorageOpt["size"]
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("container %s: StorageOpt size %q is not a byte count", ctr.ID, textsafe.SanitizeSingleLineBounded(s, 32))
	}
	return n, nil
}

// InspectContainer reads a container by ID or name; found is false when it
// is gone.
func (c *Client) InspectContainer(ctx context.Context, ref string) (ctr Container, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	return c.inspectIn(ctx, ref)
}

func (c *Client) inspectIn(ctx context.Context, ref string) (ctr Container, found bool, err error) {
	err = c.callIn(ctx, "inspect", http.MethodGet, "/containers/"+url.PathEscape(ref)+"/json", nil, nil, &ctr, jsonLimit)
	if errors.Is(err, errNotFound) {
		return Container{}, false, nil
	}
	if err != nil {
		return Container{}, false, err
	}
	return ctr, true, nil
}

// errNotOwned is a removal refused for an object without LabelManaged.
var errNotOwned = errors.New("docker: not created by this host")

// StopContainer sends the container SIGTERM and kills it after grace. A
// container already stopped or gone is no error.
func (c *Client) StopContainer(ctx context.Context, id string, grace time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, grace+CallTimeout)
	defer cancel()
	q := url.Values{"t": {strconv.Itoa(int(grace.Seconds()))}}
	err := c.callIn(ctx, "stop", http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil, 0)
	if e, ok := errors.AsType[*daemonError](err); ok && (e.Code == http.StatusNotModified || e.Code == http.StatusNotFound) {
		return nil
	}
	return err
}

// RemoveOwned force-removes a container only when it carries LabelManaged;
// errNotOwned otherwise. It re-inspects first and removes the inspected ID,
// so a name cannot be swapped in between. A container already gone is no
// error; one whose removal is already in progress answers ErrConflict.
func (c *Client) RemoveOwned(ctx context.Context, ref string) error {
	ctr, found, err := c.InspectContainer(ctx, ref)
	if err != nil || !found {
		return err
	}
	if ctr.Config.Labels[LabelManaged] != managedValue {
		return fmt.Errorf("%w: container %s", errNotOwned, ctr.ID)
	}
	return c.remove(ctx, ctr.ID)
}

// remove force-removes by ID with no ownership check, for a container this
// client has just created.
func (c *Client) remove(ctx context.Context, id string) error {
	err := c.call(ctx, "remove", http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil, 0)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
