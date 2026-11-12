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

// The pool's Docker labels. CreateContainer and EnsureNetwork set the pool
// labels (managed, pool, created) themselves; the caller sets the rest.
const (
	LabelManaged  = "manade.managed"
	labelPool     = "manade.pool"
	LabelCreated  = "manade.created"
	LabelScope    = "manade.scope"
	LabelProfile  = "manade.profile"
	LabelRunner   = "manade.runner"
	LabelRunnerID = "manade.runner-id"
	LabelProbe    = "manade.probe"
	// LabelRunnerImage is baked into each shipped runner image at build.
	LabelRunnerImage = "manade.runner-image"

	managedValue = "true"
)

// CreateRequest is a container create body, in the Engine API's field names.
type CreateRequest struct {
	Labels     map[string]string `json:"Labels,omitempty"`
	Image      string            `json:"Image"`
	User       string            `json:"User,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	HostConfig HostConfig        `json:"HostConfig"`
}

// HostConfig is the subset of the create body's HostConfig the pool sets.
type HostConfig struct {
	Tmpfs       map[string]string `json:"Tmpfs,omitempty"`
	StorageOpt  map[string]string `json:"StorageOpt,omitempty"`
	PidsLimit   *int64            `json:"PidsLimit,omitempty"`
	Init        *bool             `json:"Init,omitempty"`
	LogConfig   LogConfig         `json:"LogConfig"`
	NetworkMode string            `json:"NetworkMode,omitempty"`
	Runtime     string            `json:"Runtime,omitempty"`
	Mounts      []Mount           `json:"Mounts,omitempty"`
	CapAdd      []string          `json:"CapAdd,omitempty"`
	CapDrop     []string          `json:"CapDrop,omitempty"`
	SecurityOpt []string          `json:"SecurityOpt,omitempty"`
	GroupAdd    []string          `json:"GroupAdd,omitempty"`
	DNS         []string          `json:"Dns,omitempty"`
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

// errCreateWarnings is a create the daemon answered with warnings, such as a
// limit the kernel discarded. The container is already removed, and the
// error's text carries the warnings sanitized.
var errCreateWarnings = errors.New("docker create: the daemon warned")

// CreateContainer creates a container named name and returns its ID. It sets
// the pool labels over any in req. A create the daemon answers with warnings
// is removed and fails with errCreateWarnings.
func (c *Client) CreateContainer(ctx context.Context, name string, req *CreateRequest) (string, error) {
	body := *req
	body.Labels = c.poolLabels(req.Labels)
	var out struct {
		ID       string `json:"Id"`
		Warnings []string
	}
	if err := c.call(ctx, "create", http.MethodPost, "/containers/create", url.Values{"name": {name}}, &body, &out, jsonLimit); err != nil {
		return "", err
	}
	warnings := slices.DeleteFunc(out.Warnings, func(w string) bool { return strings.TrimSpace(w) == "" })
	if len(warnings) == 0 {
		return out.ID, nil
	}
	text, _ := textsafe.SanitizeSingleLineCapped(strings.Join(warnings, "; "), 4*errorTextLimit, "…")
	werr := fmt.Errorf("%w: %s", errCreateWarnings, text)
	if err := c.remove(ctx, out.ID); err != nil {
		return "", errors.Join(werr, err)
	}
	return "", werr
}

// StartContainer starts a container this client created.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.call(ctx, "start", http.MethodPost, "/containers/"+id+"/start", nil, nil, nil, 0)
}

// Summary is one entry of ListContainers.
type Summary struct {
	Labels map[string]string
	ID     string `json:"Id"`
	// State is the daemon's state word: created, running, exited and so on.
	State string
}

// ListContainers lists every container of this pool, running or not.
func (c *Client) ListContainers(ctx context.Context) ([]Summary, error) {
	q := c.labelFilter(nil)
	q.Set("all", "1")
	var out []Summary
	if err := c.call(ctx, "list", http.MethodGet, "/containers/json", q, nil, &out, listLimit); err != nil {
		return nil, err
	}
	return out, nil
}

// Container is a container's inspect answer, in the Engine API's shape.
type Container struct {
	Config struct {
		Labels map[string]string
	}
	// HostConfig holds what a reservation is rebuilt from.
	HostConfig struct {
		StorageOpt map[string]string
		NanoCPUs   int64 `json:"NanoCpus"`
		Memory     int64
	}
	ID string `json:"Id"`
	// Image is the ID of the image the container was created from.
	Image string
	State struct {
		StartedAt  time.Time
		FinishedAt time.Time
		ExitCode   int
		OOMKilled  bool
	}
}

// DiskBytes is the writable layer's size limit, 0 when none is set. The pool
// writes it as a plain byte count; any other form fails.
func (c *Container) DiskBytes() (int64, error) {
	s, ok := c.HostConfig.StorageOpt["size"]
	if !ok {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("container %s: StorageOpt size %q is not a byte count", c.ID, textsafe.SanitizeSingleLineBounded(s, 32))
	}
	return n, nil
}

// InspectContainer reads a container by ID; found is false when it is gone.
func (c *Client) InspectContainer(ctx context.Context, id string) (ctr Container, found bool, err error) {
	err = c.call(ctx, "inspect", http.MethodGet, "/containers/"+id+"/json", nil, nil, &ctr, jsonLimit)
	if errors.Is(err, errNotFound) {
		return Container{}, false, nil
	}
	if err != nil {
		return Container{}, false, err
	}
	return ctr, true, nil
}

// RemoveOwned force-removes a container only when it carries this pool's
// managed and pool labels; ErrNotOwned otherwise. It re-inspects first and
// removes the inspected ID, so a name cannot be swapped in between. A
// container already gone is no error.
func (c *Client) RemoveOwned(ctx context.Context, id string) error {
	ctr, found, err := c.InspectContainer(ctx, id)
	if err != nil || !found {
		return err
	}
	if !c.owned(ctr.Config.Labels) {
		return fmt.Errorf("%w: container %s", ErrNotOwned, ctr.ID)
	}
	return c.remove(ctx, ctr.ID)
}

// remove force-removes by ID with no ownership check, for a container this
// client has just created.
func (c *Client) remove(ctx context.Context, id string) error {
	err := c.call(ctx, "remove", http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}}, nil, nil, 0)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
