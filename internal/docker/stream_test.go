package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// frame is one multiplexed log frame of stream (1 stdout, 2 stderr).
func frame(stream byte, payload string) []byte {
	b := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(b[4:], uint32(len(payload)))
	return append(b, payload...)
}

func TestLogs(t *testing.T) {
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		// 64 frames of 64 KiB, headers included, fill the 4 MiB read limit.
		full := bytes.Repeat(frame(1, strings.Repeat("a", 64<<10-9)+"\n"), 64)
		switch r.PathValue("id") {
		case "bad":
			_, _ = w.Write([]byte("plain text, not framed\n"))
		case "atLimit":
			_, _ = w.Write(full)
		case "cutAtTheLimit":
			// A body declared longer than what is sent ends in a read error.
			w.Header().Set("Content-Length", strconv.Itoa(len(full)+1))
			_, _ = w.Write(full)
		case "overOnAFrameEdge":
			_, _ = w.Write(append(full, frame(1, "line past the limit\n")...))
		case "overInsideAFrame":
			_, _ = w.Write(frame(1, strings.Repeat("b", 5<<20)))
		default:
			var b []byte
			b = append(b, frame(1, "line 1\nline 2\r\n")...)
			b = append(b, frame(2, "error: li")...)
			b = append(b, frame(1, "ne 3\nline 4\n")...)
			_, _ = w.Write(b)
		}
	})
	c := d.dial(t)
	t.Run("joinsBothStreamsAndKeepsTheTail", func(t *testing.T) {
		lines, err := c.Logs(t.Context(), "c1", 3)
		if err != nil {
			t.Fatalf("Logs: %v", err)
		}
		if want := []string{"line 2", "error: line 3", "line 4"}; !slices.Equal(lines, want) {
			t.Errorf("Logs = %q, want %q", lines, want)
		}
		q := d.seen(http.MethodGet, "/containers/c1/logs")[0].query
		if q["tail"][0] != "3" || q["stdout"][0] != "1" || q["stderr"][0] != "1" {
			t.Errorf("query = %v", q)
		}
	})
	t.Run("refusesAnUnframedStream", func(t *testing.T) {
		if _, err := c.Logs(t.Context(), "bad", 50); err == nil {
			t.Error("Logs on an unframed stream succeeded")
		}
	})
	t.Run("readsATailOfExactlyTheLimit", func(t *testing.T) {
		if lines, err := c.Logs(t.Context(), "atLimit", 1); err != nil || len(lines) != 1 {
			t.Errorf("Logs = %d lines, %v; want the last line of a 4 MiB tail", len(lines), err)
		}
	})
	t.Run("failsABodyCutAtTheLimit", func(t *testing.T) {
		if lines, err := c.Logs(t.Context(), "cutAtTheLimit", 1); err == nil || errors.Is(err, ErrTooLarge) {
			t.Errorf("Logs = %d lines, %v; want the read error of a body cut after 4 MiB", len(lines), err)
		}
	})
	t.Run("refusesANegativeCount", func(t *testing.T) {
		if lines, err := c.Logs(t.Context(), "negative", -1); err == nil {
			t.Errorf("Logs = %q, nil; want an error for a negative line count", lines)
		}
		if got := d.seen(http.MethodGet, "/containers/negative/logs"); len(got) != 0 {
			t.Errorf("sent %d requests, want none", len(got))
		}
	})
	for _, id := range []string{"overOnAFrameEdge", "overInsideAFrame"} {
		t.Run(id, func(t *testing.T) {
			if lines, err := c.Logs(t.Context(), id, 50); !errors.Is(err, ErrTooLarge) {
				t.Errorf("Logs = %d lines, %v; want ErrTooLarge for a tail past 4 MiB", len(lines), err)
			}
		})
	}
}

func TestDemux(t *testing.T) {
	t.Run("keepsTheLastBytesPastTheCap", func(t *testing.T) {
		var in []byte
		for range 3 {
			in = append(in, frame(1, strings.Repeat("a", LogKeep/2))...)
		}
		in = append(in, frame(2, "end")...)
		out, err := demux(bytes.NewReader(in))
		if err != nil || len(out) != LogKeep || !bytes.HasSuffix(out, []byte("aend")) {
			t.Errorf("demux = %d bytes, %v; want the last %d ending in the last frame", len(out), err, LogKeep)
		}
	})
	t.Run("aCutFrameFails", func(t *testing.T) {
		in := frame(1, "complete\n")
		in = append(in, frame(1, "partial line")[:12]...)
		if out, err := demux(bytes.NewReader(in)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("demux = %q, %v; want io.ErrUnexpectedEOF", out, err)
		}
	})
	t.Run("aCutHeaderFails", func(t *testing.T) {
		if _, err := demux(bytes.NewReader([]byte{1, 0, 0})); err == nil {
			t.Error("demux of a cut header succeeded")
		}
	})
}

// tarOf builds an archive holding one entry.
func tarOf(t *testing.T, hdr *tar.Header, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr.Size = int64(len(content))
	if hdr.Typeflag != tar.TypeReg {
		hdr.Size = 0
		content = ""
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestArchive(t *testing.T) {
	passwd := "root:x:0:0:root:/root:/bin/bash\nrunner:x:1001:1001::/home/runner:/bin/bash\n"
	archives := map[string][]byte{
		"/etc/passwd": tarOf(t, &tar.Header{Name: "passwd", Typeflag: tar.TypeReg, Mode: 0o644}, passwd),
		"/etc/group":  tarOf(t, &tar.Header{Name: "group", Typeflag: tar.TypeSymlink, Linkname: "/tmp/group"}, ""),
		"/etc/big":    tarOf(t, &tar.Header{Name: "big", Typeflag: tar.TypeReg, Mode: 0o644}, strings.Repeat("x", 4096)),
		"/etc/other":  tarOf(t, &tar.Header{Name: "elsewhere", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		"/garbage":    []byte("not a tar archive at all, but long enough to read a header from it" + strings.Repeat(" ", 512)),
	}
	d := newDaemon(t, "1.51")
	d.handle("GET /containers/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		a, ok := archives[r.URL.Query().Get("path")]
		if !ok || r.PathValue("id") != "probe" {
			writeError(w, http.StatusNotFound, "Could not find the file")
			return
		}
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(a)
	})
	c := d.dial(t)
	t.Run("readFileReturnsARegularFile", func(t *testing.T) {
		data, found, err := c.ReadFile(t.Context(), "probe", "/etc/passwd", 1<<20)
		if err != nil || !found || string(data) != passwd {
			t.Errorf("ReadFile = %q, %v, %v", data, found, err)
		}
	})
	t.Run("readFileMissingIsNotFound", func(t *testing.T) {
		for _, id := range []string{"probe", "gone"} {
			if _, found, err := c.ReadFile(t.Context(), id, "/etc/shadow", 1<<20); found || err != nil {
				t.Errorf("ReadFile(%s) = %v, %v; want not found, nil", id, found, err)
			}
		}
	})
	t.Run("readFileRefusesASymlink", func(t *testing.T) {
		if _, found, err := c.ReadFile(t.Context(), "probe", "/etc/group", 1<<20); !found || err == nil {
			t.Errorf("ReadFile on a symlink = %v, %v; want an error", found, err)
		}
	})
	t.Run("readFileRefusesAnotherEntry", func(t *testing.T) {
		if _, _, err := c.ReadFile(t.Context(), "probe", "/etc/other", 1<<20); err == nil {
			t.Error("ReadFile accepted an entry named for another path")
		}
	})
	t.Run("readFileRefusesGarbage", func(t *testing.T) {
		if _, _, err := c.ReadFile(t.Context(), "probe", "/garbage", 1<<20); err == nil {
			t.Error("ReadFile accepted a non-tar answer")
		}
	})
	t.Run("readFileCapsTheSize", func(t *testing.T) {
		if _, _, err := c.ReadFile(t.Context(), "probe", "/etc/big", 1024); !errors.Is(err, ErrTooLarge) {
			t.Errorf("ReadFile over its limit = %v, want ErrTooLarge", err)
		}
		if data, _, err := c.ReadFile(t.Context(), "probe", "/etc/big", 4096); err != nil || len(data) != 4096 {
			t.Errorf("ReadFile at its limit = %d bytes, %v", len(data), err)
		}
	})
	t.Run("copyArchiveWritesTheTar", func(t *testing.T) {
		var buf bytes.Buffer
		found, err := c.CopyArchive(t.Context(), "probe", "/etc/passwd", &buf, 1<<20)
		if err != nil || !found || !bytes.Equal(buf.Bytes(), archives["/etc/passwd"]) {
			t.Errorf("CopyArchive = %v, %v, %d bytes", found, err, buf.Len())
		}
	})
	t.Run("copyArchiveCapsTheSize", func(t *testing.T) {
		var buf bytes.Buffer
		limit := int64(len(archives["/etc/big"]) - 1)
		if _, err := c.CopyArchive(t.Context(), "probe", "/etc/big", &buf, limit); !errors.Is(err, ErrTooLarge) {
			t.Errorf("CopyArchive over its limit = %v, want ErrTooLarge", err)
		}
		buf.Reset()
		if _, err := c.CopyArchive(t.Context(), "probe", "/etc/big", &buf, limit+1); err != nil {
			t.Errorf("CopyArchive at its limit = %v", err)
		}
	})
}

func TestEvents(t *testing.T) {
	release := make(chan struct{})
	d := newDaemon(t, "1.51")
	d.handle("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for _, id := range []string{"c1", "c2"} {
			_, _ = w.Write([]byte(`{"Type":"container","Action":"die","Actor":{"ID":"` + id + `","Attributes":{"exitCode":"0"}}}` + "\n"))
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	c := d.dial(t)
	t.Run("deliversDieEventsUntilCancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		got := make(chan string, 4)
		done := make(chan error, 1)
		go func() { done <- c.Events(ctx, func(id string) { got <- id }) }()
		for _, want := range []string{"c1", "c2"} {
			select {
			case id := <-got:
				if id != want {
					t.Errorf("event = %q, want %q", id, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("no event within 10 s")
			}
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Events after cancel = %v, want context.Canceled", err)
		}
		var filters map[string][]string
		if err := json.Unmarshal([]byte(d.seen(http.MethodGet, "/events")[0].query["filters"][0]), &filters); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(filters["event"], []string{"die"}) || !slices.Equal(filters["type"], []string{"container"}) ||
			!slices.Equal(filters["label"], []string{"manade.managed=true"}) {
			t.Errorf("filters = %v", filters)
		}
	})
	t.Run("aClosedStreamIsAnError", func(t *testing.T) {
		close(release)
		if err := c.Events(t.Context(), func(string) {}); err == nil || errors.Is(err, context.Canceled) {
			t.Errorf("Events on a closed stream = %v, want an error", err)
		}
	})
}
