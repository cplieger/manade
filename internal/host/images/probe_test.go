package images

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cplieger/manade/internal/docker"
)

// captureDaemon keeps the tar a load sends, then loads it into the fake.
type captureDaemon struct {
	*fakeDaemon
	tar []byte
}

func (c *captureDaemon) LoadImage(ctx context.Context, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	c.tar = b
	return c.fakeDaemon.LoadImage(ctx, bytes.NewReader(b))
}

func TestLoadProbe(t *testing.T) {
	hs := newHarness(t, internetHost())
	bin := []byte("\x7fELF the host binary")
	if err := os.WriteFile(hs.cfg.Binary, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cd := &captureDaemon{fakeDaemon: hs.d}
	hs.cfg.Daemon = cd
	m := New(hs.cfg)
	id, err := m.LoadProbe(t.Context())
	if err != nil {
		t.Fatalf("LoadProbe: %v", err)
	}
	if m.ProbeImage() != id {
		t.Errorf("ProbeImage = %q, want %q", m.ProbeImage(), id)
	}
	img, found, _ := hs.d.InspectImage(t.Context(), ProbeRepo+":"+testVersion)
	if !found || img.ID != id || img.Config.Labels[docker.LabelProbeImage] != testVersion {
		t.Errorf("probe image = %+v, %v; want tagged %s:%s and labelled", img, found, ProbeRepo, testVersion)
	}
	files := untar(t, cd.tar)
	var cfg struct {
		Config struct{ Entrypoint []string } `json:"config"`
		OS     string                        `json:"os"`
	}
	if err := json.Unmarshal(files["config.json"], &cfg); err != nil || cfg.OS != "linux" || len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/manade" {
		t.Errorf("config = %+v, %v; want linux with entrypoint /manade", cfg, err)
	}
	layer := tar.NewReader(bytes.NewReader(files["layer.tar"]))
	hdr, err := layer.Next()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(layer)
	if hdr.Name != "manade" || hdr.Mode&0o555 != 0o555 || !bytes.Equal(got, bin) {
		t.Errorf("layer holds %s mode %o, %q; want manade, executable by anyone, the binary", hdr.Name, hdr.Mode, got)
	}
	if _, err := layer.Next(); err != io.EOF {
		t.Errorf("layer holds more than the binary: %v", err)
	}
	sum := sha256.Sum256(files["config.json"])
	if id != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("ID %s is not the config's digest", id)
	}
	again, err := m.LoadProbe(t.Context())
	if err != nil || again != id {
		t.Errorf("LoadProbe again = %q, %v; want the same image", again, err)
	}
}

func TestLoadProbeFailures(t *testing.T) {
	t.Run("noBinary", func(t *testing.T) {
		hs := newHarness(t, internetHost())
		hs.cfg.Binary = filepath.Join(t.TempDir(), "absent")
		if _, err := New(hs.cfg).LoadProbe(t.Context()); err == nil {
			t.Error("LoadProbe = nil, want the missing binary named")
		}
	})
	t.Run("unlabelledAfterLoad", func(t *testing.T) {
		hs := newHarness(t, internetHost())
		hs.cfg.Daemon = &noLoad{hs.d}
		m := New(hs.cfg)
		if _, err := m.LoadProbe(t.Context()); err == nil || m.ProbeImage() != "" {
			t.Errorf("LoadProbe = %v, ProbeImage %q; want a failure and no probe image", err, m.ProbeImage())
		}
	})
}

// noLoad answers a load without loading anything.
type noLoad struct{ *fakeDaemon }

func (*noLoad) LoadImage(_ context.Context, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func TestProbeRef(t *testing.T) {
	for v, want := range map[string]string{
		"v1.0.0-dev.12": ProbeRepo + ":v1.0.0-dev.12",
		"dev":           ProbeRepo + ":dev",
		"":              ProbeRepo + ":dev",
		"v1+build":      ProbeRepo + ":dev",
		"-x":            ProbeRepo + ":dev",
	} {
		if got := probeRef(v); got != want {
			t.Errorf("probeRef(%q) = %q, want %q", v, got, want)
		}
	}
}

func untar(t *testing.T, b []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name], _ = io.ReadAll(tr)
	}
}
