package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"

	"github.com/cplieger/go/httpclient"
)

// logReadLimit bounds the bytes read from a log tail.
const logReadLimit = 4 << 20

// LogKeep is how many of a log tail's last bytes Logs keeps.
const LogKeep = 256 << 10

// errTooLarge is returned when an archive or file passes its caller's limit.
var errTooLarge = errors.New("docker: content over its size limit")

// Logs returns the container's last n log lines, stdout and stderr
// interleaved, raw. Only whole lines of the tail's last LogKeep bytes are
// returned, so a line longer than LogKeep is not. A tail over 4 MiB fails
// with errTooLarge, and a negative n fails before any request.
func (c *Client) Logs(ctx context.Context, id string, n int) ([]string, error) {
	if n < 0 {
		return nil, fmt.Errorf("docker logs: a negative line count (%d)", n)
	}
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(n)}}
	body, err := c.request(ctx, c.api, "logs", http.MethodGet, "/containers/"+id+"/logs", q, nil, nil)
	if err != nil {
		return nil, err
	}
	defer httpclient.DrainClose(body)
	lr := &io.LimitedReader{R: body, N: logReadLimit}
	text, partial, err := demux(lr)
	if lr.N == 0 {
		switch _, rerr := io.ReadFull(body, make([]byte, 1)); {
		case rerr == nil:
			return nil, fmt.Errorf("%w: the log tail passes %d bytes", errTooLarge, logReadLimit)
		case !errors.Is(rerr, io.EOF):
			return nil, fmt.Errorf("docker logs: %w", rerr)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("docker logs: %w", err)
	}
	if partial {
		_, text, _ = bytes.Cut(text, []byte("\n"))
	}
	if len(text) == 0 {
		return nil, nil
	}
	lines := bytes.Split(bytes.TrimSuffix(text, []byte("\n")), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, string(bytes.TrimSuffix(l, []byte("\r"))))
	}
	return out, nil
}

// demux joins the payloads of a multiplexed log stream, keeping the last
// LogKeep bytes; partial is whether those start inside a line. A container
// without a TTY, as every runner is, frames its output with an 8-byte header:
// the stream (0, 1 or 2), three zero bytes and the payload's big-endian length
// (https://docs.docker.com/reference/api/engine/version/v1.51/#tag/Container/operation/ContainerAttach).
// A stream that ends inside a frame fails with io.ErrUnexpectedEOF.
func demux(r io.Reader) (out []byte, partial bool, err error) {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return out, partial, nil
			}
			return out, partial, err
		}
		if hdr[0] > 2 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return out, partial, errors.New("malformed log frame")
		}
		chunk, err := payload(r, int64(binary.BigEndian.Uint32(hdr[4:])))
		out = append(out, chunk...)
		if cut := len(out) - LogKeep; cut > 0 {
			partial = out[cut-1] != '\n'
			out = append(out[:0], out[cut:]...)
		}
		if err != nil {
			return out, partial, err
		}
	}
}

// payload reads one frame's size bytes, failing with io.ErrUnexpectedEOF
// when the stream ends first.
func payload(r io.Reader, size int64) ([]byte, error) {
	chunk, err := io.ReadAll(io.LimitReader(r, size))
	if err == nil && int64(len(chunk)) < size {
		err = io.ErrUnexpectedEOF
	}
	return chunk, err
}

// CopyArchive writes the tar archive of path inside the container to w, at
// most limit bytes, and fails with errTooLarge past it, leaving what it wrote
// in w. found is false when the container or path does not exist.
func (c *Client) CopyArchive(ctx context.Context, id, p string, w io.Writer, limit int64) (found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	body, err := c.request(ctx, c.api, "archive", http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {p}}, nil, nil)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer httpclient.DrainClose(body)
	n, err := io.Copy(w, io.LimitReader(body, limit+1))
	if err != nil {
		return true, fmt.Errorf("docker archive: %w", err)
	}
	if n > limit {
		return true, fmt.Errorf("%w: the archive of %s passes %d bytes", errTooLarge, p, limit)
	}
	return true, nil
}

// ReadFile returns the content of the regular file at the absolute path p
// inside the container, at most limit bytes; found is false when it does not
// exist. Anything but a regular file, a symlink included, is an error: the
// archive holds the link, not its target.
func (c *Client) ReadFile(ctx context.Context, id, p string, limit int64) (data []byte, found bool, err error) {
	var buf bytes.Buffer
	// A tar header and its padding add at most a few KiB to the file.
	found, err = c.CopyArchive(ctx, id, p, &buf, limit+16<<10)
	if err != nil || !found {
		return nil, found, err
	}
	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		return nil, true, fmt.Errorf("docker archive: reading %s: %w", p, err)
	}
	if hdr.Typeflag != tar.TypeReg || hdr.Name != path.Base(p) {
		return nil, true, fmt.Errorf("docker archive: %s is not a regular file", p)
	}
	if hdr.Size > limit {
		return nil, true, fmt.Errorf("%w: %s is %d bytes, over %d", errTooLarge, p, hdr.Size, limit)
	}
	data, err = io.ReadAll(tr)
	if err != nil {
		return nil, true, fmt.Errorf("docker archive: reading %s: %w", p, err)
	}
	return data, true, nil
}

// Events hands the container ID of each managed container's die event to fn
// until ctx ends or the stream fails, and returns why: ctx.Err() once ctx is
// done. The stream has no deadline of its own; ctx bounds it.
func (c *Client) Events(ctx context.Context, fn func(containerID string)) error {
	q := managedFilter(map[string][]string{"type": {"container"}, "event": {"die"}})
	body, err := c.request(ctx, c.stream, "events", http.MethodGet, "/events", q, nil, nil)
	if err != nil {
		return err
	}
	defer httpclient.DrainClose(body)
	err = readMessages(body, "events", func(line []byte) error {
		var m struct {
			Actor struct{ ID string }
		}
		if uerr := json.Unmarshal(line, &m); uerr != nil {
			return fmt.Errorf("docker events: decoding an event: %w", uerr)
		}
		fn(m.Actor.ID)
		return nil
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		err = errors.New("docker events: the daemon closed the stream")
	}
	return err
}
