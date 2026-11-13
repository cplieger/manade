package docker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Volume is a volume's inspect or list entry.
type Volume struct {
	Labels map[string]string
	Name   string
}

// CreateVolume creates an anonymous volume, labelled managed over labels,
// and answers its name.
func (c *Client) CreateVolume(ctx context.Context, labels map[string]string) (string, error) {
	var out Volume
	body := map[string]any{"Labels": c.managedLabels(labels)}
	if err := c.call(ctx, "volume", http.MethodPost, "/volumes/create", nil, body, &out, jsonLimit); err != nil {
		return "", err
	}
	if out.Name == "" {
		return "", errors.New("docker volume: the daemon named no volume")
	}
	return out.Name, nil
}

// ListVolumes lists every managed volume.
func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	var out struct{ Volumes []Volume }
	if err := c.call(ctx, "volumes", http.MethodGet, "/volumes", managedFilter(nil), nil, &out, listLimit); err != nil {
		return nil, err
	}
	return out.Volumes, nil
}

// RemoveOwnedVolume removes a volume only when it carries LabelManaged;
// ErrNotOwned otherwise. A volume already gone is no error; one a container
// still uses answers ErrConflict.
func (c *Client) RemoveOwnedVolume(ctx context.Context, name string) error {
	var v Volume
	err := c.call(ctx, "volume", http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &v, jsonLimit)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if v.Labels[LabelManaged] != managedValue {
		return fmt.Errorf("%w: volume %s", ErrNotOwned, v.Name)
	}
	err = c.call(ctx, "volume", http.MethodDelete, "/volumes/"+url.PathEscape(v.Name), nil, nil, nil, 0)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
