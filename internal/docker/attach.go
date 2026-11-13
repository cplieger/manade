package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Stream is an attached container's standard input and output. Reads
// return its stdout and stderr payloads joined, with Docker's framing
// removed. It has no deadline of its own: SetDeadline bounds each exchange.
type Stream struct {
	conn net.Conn
	br   *bufio.Reader
	left uint32
}

// Attach attaches to container id's standard input and output, which it
// must have been created to allow (OpenStdin with StdinOnce). CallTimeout
// bounds only the daemon's 101 answer; the open stream outlives it. Closing
// the stream closes the container's standard input.
func (c *Client) Attach(ctx context.Context, id string) (*Stream, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	start := time.Now()
	s, err := c.attach(ctx, id)
	c.observe("attach", time.Since(start))
	return s, err
}

func (c *Client) attach(ctx context.Context, id string) (*Stream, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker attach: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	q := url.Values{"stream": {"1"}, "stdin": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker"+c.path("/containers/"+url.PathEscape(id)+"/attach")+"?"+q.Encode(), http.NoBody)
	if err == nil {
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "tcp")
		err = req.Write(conn)
	}
	var resp *http.Response
	br := bufio.NewReader(conn)
	if err == nil {
		resp, err = http.ReadResponse(br, req) //nolint:bodyclose // a 101's body is the stream, which Stream.Close closes; any other is drained by apiError
	}
	// A connection whose deadline ctx's end set is spent, whatever it read.
	switch {
	case !stop():
		err = errors.Join(err, ctx.Err())
	case err == nil && resp.StatusCode != http.StatusSwitchingProtocols:
		err = apiError("attach", resp, fmt.Errorf("status %d", resp.StatusCode))
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("docker attach: %w", err)
	}
	return &Stream{conn: conn, br: br}, nil
}

// Write sends p to the container's standard input.
func (s *Stream) Write(p []byte) (int, error) { return s.conn.Write(p) }

// Read reads the container's output, its frames' payloads in order. A
// stream that ends inside a frame fails with io.ErrUnexpectedEOF.
func (s *Stream) Read(p []byte) (int, error) {
	for s.left == 0 {
		var hdr [8]byte
		if _, err := io.ReadFull(s.br, hdr[:]); err != nil {
			return 0, err
		}
		if hdr[0] > 2 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			return 0, errors.New("docker attach: malformed frame")
		}
		s.left = binary.BigEndian.Uint32(hdr[4:])
	}
	if uint64(len(p)) > uint64(s.left) {
		p = p[:s.left]
	}
	n, err := s.br.Read(p)
	s.left -= uint32(n) //nolint:gosec // G115: n is at most len(p), at most s.left
	if errors.Is(err, io.EOF) && s.left > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// SetDeadline bounds the stream's reads and writes until it is set again;
// the zero time removes the bound.
func (s *Stream) SetDeadline(t time.Time) error { return s.conn.SetDeadline(t) }

// Close ends the stream.
func (s *Stream) Close() error { return s.conn.Close() }
