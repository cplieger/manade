package docker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cplieger/go/httpclient"
	"github.com/cplieger/go/textsafe"
)

// Image is an image's inspect answer, in the Engine API's shape.
type Image struct {
	Config struct {
		Labels map[string]string
		User   string
	}
	ID          string `json:"Id"`
	RepoDigests []string
}

// InspectImage reads an image by reference, digest reference or ID; found is
// false when the daemon has no such image.
func (c *Client) InspectImage(ctx context.Context, ref string) (img Image, found bool, err error) {
	err = c.call(ctx, "inspect", http.MethodGet, "/images/"+ref+"/json", nil, nil, &img, jsonLimit)
	if errors.Is(err, errNotFound) {
		return Image{}, false, nil
	}
	if err != nil {
		return Image{}, false, err
	}
	return img, true, nil
}

// PullImage pulls ref and waits for the pull to finish; auth is an encoded
// X-Registry-Auth value, or "" for none. A reference with neither tag nor
// digest pulls its latest tag, as the Docker CLI does: the Engine API pulls
// every tag of a bare name.
func (c *Client) PullImage(ctx context.Context, ref, auth string) error {
	ctx, cancel := context.WithTimeout(ctx, pullTimeout)
	defer cancel()
	q := url.Values{"fromImage": {ref}}
	if nameOnly(ref) {
		q.Set("tag", "latest")
	}
	body, err := c.request(ctx, c.stream, "pull", http.MethodPost, "/images/create", q, nil, authHeaders(auth))
	if err != nil {
		return err
	}
	defer httpclient.DrainClose(body)
	return readMessages(body, "pull", streamError("pull"))
}

// LoadImage loads an image tar, as `docker save` writes one, and waits for
// the load to finish.
func (c *Client) LoadImage(ctx context.Context, tar io.Reader) error {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	body, err := c.request(ctx, c.stream, "load", http.MethodPost, "/images/load", url.Values{"quiet": {"1"}}, tar, nil)
	if err != nil {
		return err
	}
	defer httpclient.DrainClose(body)
	return readMessages(body, "load", streamError("load"))
}

// streamError fails a message stream at its first error message.
func streamError(op string) func([]byte) error {
	return func(line []byte) error {
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &m) == nil && m.Error != "" {
			msg, _ := textsafe.SanitizeSingleLineCapped(m.Error, errorTextLimit, "…")
			return fmt.Errorf("docker %s: %s", op, msg)
		}
		return nil
	}
}

// nameOnly reports a reference with no tag and no digest: no colon after the
// last slash, where a tag's colon and a digest's "@sha256:" both put one. A
// colon before the last slash is a registry port.
func nameOnly(ref string) bool {
	return !strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":")
}

// readMessages hands each line of a JSON message stream to fn, refusing a
// line over messageLimit. fn's error ends the read.
func readMessages(r io.Reader, op string, fn func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), messageLimit)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("docker %s: reading the stream: %w", op, err)
	}
	return nil
}

// ImageSummary is one entry of ListImages.
type ImageSummary struct {
	Labels      map[string]string
	ID          string `json:"Id"`
	RepoTags    []string
	RepoDigests []string
}

// ListImages lists every image in the cache, intermediate layers left out.
func (c *Client) ListImages(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	if err := c.call(ctx, "images", http.MethodGet, "/images/json", nil, nil, &out, listLimit); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveImage removes ref, an image ID or a tag, without force: a tag is
// untagged, and the image goes with its last tag. The daemon refuses an
// image a container uses, or an ID more than one repository tags, which
// answers ErrConflict. An image already gone is no error. Which images are
// the host's to remove is its caller's decision.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	err := c.call(ctx, "remove", http.MethodDelete, "/images/"+ref, nil, nil, nil, 0)
	if errors.Is(err, errNotFound) {
		return nil
	}
	return err
}
