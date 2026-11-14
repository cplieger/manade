package images

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"time"

	"github.com/cplieger/manade/internal/docker"
)

// probeRepo is the probe image's repository; its tag is the host's version.
const probeRepo = "manade-probe"

// probeFile is where the probe image holds the host's binary.
const probeFile = "manade"

// tagRe is a tag Docker accepts.
var tagRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// probeRef answers the probe image's reference for version; a version that
// is no valid tag is tagged "dev", as any build that is not a release.
func probeRef(version string) string {
	if !tagRe.MatchString(version) {
		version = "dev"
	}
	return probeRepo + ":" + version
}

// LoadProbe builds the probe image from the host's own binary, one layer,
// loads it into the daemon and answers its ID, which ProbeImage answers
// from then on. A probe create answered with a missing image calls it
// again. It needs no registry.
func (m *Manager) LoadProbe(ctx context.Context) (string, error) {
	f, err := os.Open(m.binary)
	if err != nil {
		return "", fmt.Errorf("the probe image: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("the probe image: %w", err)
	}
	sum := sha256.New()
	var layer countWriter
	if err = writeLayer(io.MultiWriter(sum, &layer), f, st.Size()); err != nil {
		return "", fmt.Errorf("the probe image: %w", err)
	}
	cfg := m.probeConfig("sha256:" + hex.EncodeToString(sum.Sum(nil)))
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(writeImage(pw, f, st.Size(), int64(layer), cfg, probeRef(m.version)))
	}()
	err = m.daemon.LoadImage(ctx, pr)
	_ = pr.CloseWithError(errors.New("the load ended"))
	<-done
	if err != nil {
		return "", fmt.Errorf("loading the probe image: %w", err)
	}
	img, found, err := m.daemon.InspectImage(ctx, probeRef(m.version))
	switch {
	case err != nil:
		return "", fmt.Errorf("the probe image: %w", err)
	case !found || img.Config.Labels[docker.LabelProbeImage] == "":
		return "", errors.New("the probe image: the daemon holds no labelled " + probeRef(m.version) + " after its load")
	}
	m.mu.Lock()
	m.probeID = img.ID
	m.mu.Unlock()
	return img.ID, nil
}

// probeConfig is the image config of a one-layer image whose layer has
// diffID: the Docker image spec's fields a load reads
// (https://github.com/moby/docker-image-spec/blob/v1.3.1/spec.md).
func (m *Manager) probeConfig(diffID string) []byte {
	cfg := map[string]any{
		"architecture": runtime.GOARCH,
		"os":           "linux",
		"config": map[string]any{
			"Entrypoint": []string{"/" + probeFile},
			"Labels":     map[string]string{docker.LabelProbeImage: m.version},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{diffID}},
	}
	b, _ := json.Marshal(cfg) // maps of strings and lists always encode
	return b
}

// writeLayer writes the layer tar: the binary at /manade, executable by any
// user, at a fixed time so two writes are byte for byte the same.
func writeLayer(w io.Writer, f *os.File, size int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	hdr := &tar.Header{Name: probeFile, Mode: 0o755, Size: size, Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := io.CopyN(tw, f, size); err != nil {
		return err
	}
	return tw.Close()
}

// writeImage writes the image tar a load takes, in `docker save`'s form: a
// manifest naming the config, the tag and the layer.
func writeImage(w io.Writer, f *os.File, size, layerSize int64, cfg []byte, tag string) error {
	manifest, _ := json.Marshal([]map[string]any{{"Config": "config.json", "RepoTags": []string{tag}, "Layers": []string{"layer.tar"}}})
	tw := tar.NewWriter(w)
	for _, e := range []struct {
		name string
		body []byte
	}{{"manifest.json", manifest}, {"config.json", cfg}} {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		if _, err := tw.Write(e.body); err != nil {
			return err
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "layer.tar", Mode: 0o644, Size: layerSize, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if err := writeLayer(tw, f, size); err != nil {
		return err
	}
	return tw.Close()
}

// countWriter counts what is written to it.
type countWriter int64

func (c *countWriter) Write(p []byte) (int, error) {
	*c += countWriter(len(p))
	return len(p), nil
}
