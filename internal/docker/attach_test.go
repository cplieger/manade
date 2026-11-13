package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// upgrade answers an attach with 101 and hands the raw stream to fn.
func upgrade(fn func(rw *bufio.ReadWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = rw.Flush()
		fn(rw)
	}
}

func writeFrame(rw *bufio.ReadWriter, stream byte, payload string) {
	var hdr [8]byte
	hdr[0] = stream
	binary.BigEndian.PutUint32(hdr[4:], uint32(len(payload)))
	_, _ = rw.Write(hdr[:])
	_, _ = rw.WriteString(payload)
	_ = rw.Flush()
}

func TestAttach(t *testing.T) {
	t.Run("steps a container line by line past the call timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/attach", upgrade(func(rw *bufio.ReadWriter) {
				for {
					line, err := rw.ReadString('\n')
					if err != nil {
						return
					}
					time.Sleep(CallTimeout)
					writeFrame(rw, 1, "ok ")
					writeFrame(rw, 2, strings.TrimSpace(line)+"\n")
				}
			}))
			s, err := d.dial(t).Attach(t.Context(), "c1")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			q := url.Values(d.seen(http.MethodPost, "/containers/c1/attach")[0].query)
			for _, k := range []string{"stream", "stdin", "stdout", "stderr"} {
				if q.Get(k) != "1" {
					t.Errorf("query %s = %q", k, q.Get(k))
				}
			}
			br := bufio.NewReader(s)
			for _, check := range []string{"gateway", "listener"} {
				if _, err := io.WriteString(s, check+"\n"); err != nil {
					t.Fatal(err)
				}
				got, err := br.ReadString('\n')
				if err != nil || got != "ok "+check+"\n" {
					t.Errorf("answer = %q, %v", got, err)
				}
			}
		})
	})
	t.Run("a deadline bounds an exchange", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/attach", upgrade(func(rw *bufio.ReadWriter) { _, _ = rw.ReadString('\n') }))
			s, err := d.dial(t).Attach(t.Context(), "c1")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err := s.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Read(make([]byte, 8)); err == nil {
				t.Error("a read with no answer passed its deadline")
			}
		})
	})
	t.Run("an attach whose 101 never comes is cut at the call timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newDaemon(t, "1.51")
			d.handle("POST /containers/{id}/attach", hold)
			start := time.Now()
			_, err := d.dial(t).Attach(t.Context(), "c1")
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != CallTimeout {
				t.Errorf("Attach = %v after %s", err, time.Since(start))
			}
			if obs := d.observed(); obs[len(obs)-1] != "attach" {
				t.Errorf("observed %v, want attach last", obs)
			}
		})
	})
	t.Run("a refused attach is an API error", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/attach", func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "No such container: c1")
		})
		_, err := d.dial(t).Attach(t.Context(), "c1")
		if !errors.Is(err, errNotFound) {
			t.Errorf("Attach = %v", err)
		}
	})
	t.Run("a stream cut inside a frame is an unexpected EOF", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/attach", upgrade(func(rw *bufio.ReadWriter) {
			_, _ = rw.Write([]byte{1, 0, 0, 0, 0, 0, 0, 9, 'a', 'b'})
			_ = rw.Flush()
		}))
		s, err := d.dial(t).Attach(t.Context(), "c1")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := io.ReadAll(s); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("ReadAll = %v", err)
		}
	})
	t.Run("a malformed frame fails", func(t *testing.T) {
		d := newDaemon(t, "1.51")
		d.handle("POST /containers/{id}/attach", upgrade(func(rw *bufio.ReadWriter) {
			_, _ = rw.Write([]byte{7, 1, 0, 0, 0, 0, 0, 1, 'a'})
			_ = rw.Flush()
		}))
		s, err := d.dial(t).Attach(t.Context(), "c1")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if _, err := io.ReadAll(s); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Errorf("ReadAll = %v", err)
		}
	})
}
