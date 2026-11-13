package docker

import (
	"context"
	"net/http"
)

// Info is the subset of the daemon's /info the host reports.
type Info struct {
	ServerVersion string
	// Driver is the storage driver; a graph driver's name, never the
	// containerd snapshotter's, when the daemon was started with one.
	Driver   string
	DataRoot string `json:"DockerRootDir"`
}

// Info reads the daemon's version, storage driver and data root.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, "info", http.MethodGet, "/info", nil, nil, &out, jsonLimit)
	return out, err
}
