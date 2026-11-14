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
	Driver string
}

// Info reads the daemon's version, storage driver and data root.
func (c *Client) Info(ctx context.Context) (Info, error) {
	var out Info
	err := c.call(ctx, "info", http.MethodGet, "/info", nil, nil, &out, jsonLimit)
	return out, err
}

// Ping asks whether the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, "ping", http.MethodGet, "/_ping", nil, nil, nil, 0)
}
