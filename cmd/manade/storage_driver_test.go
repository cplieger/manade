package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/cplieger/manade/internal/host/platform"
)

func stubPick(t *testing.T, p platform.StoragePick, err error) *[]string {
	t.Helper()
	var dirs []string
	old, oldOut := pickStorage, stdout
	t.Cleanup(func() { pickStorage, stdout = old, oldOut })
	pickStorage = func(r platform.Root, dir string, who platform.For) (platform.StoragePick, error) {
		if r != platform.System || who != platform.ForJob {
			t.Errorf("pick for %q, %d, want the system root for a job", r, who)
		}
		dirs = append(dirs, dir)
		return p, err
	}
	return &dirs
}

func TestRunStorageDriver(t *testing.T) {
	t.Run("answers the driver alone on stdout", func(t *testing.T) {
		dirs := stubPick(t, platform.StoragePick{Driver: platform.VFS, Why: "the data root is overlayfs"}, nil)
		var out, errOut bytes.Buffer
		stdout = &out
		if code := run([]string{"storage-driver", "/var/lib/docker"}, &errOut); code != 0 {
			t.Fatalf("run = %d: %s", code, errOut.String())
		}
		if out.String() != "vfs\n" {
			t.Errorf("stdout = %q, want vfs alone", out.String())
		}
		if errOut.String() != "manade storage-driver: vfs (the data root is overlayfs)\n" {
			t.Errorf("stderr = %q, want the pick and why", errOut.String())
		}
		if len(*dirs) != 1 || (*dirs)[0] != "/var/lib/docker" {
			t.Errorf("picked for %q, want /var/lib/docker", *dirs)
		}
	})
	t.Run("a failed pick exits 1 with nothing on stdout", func(t *testing.T) {
		stubPick(t, platform.StoragePick{}, errors.New("statfs: no such file"))
		var out, errOut bytes.Buffer
		stdout = &out
		if code := run([]string{"storage-driver", "/nope"}, &errOut); code != 1 {
			t.Errorf("run = %d, want 1", code)
		}
		if out.Len() != 0 || !bytes.Contains(errOut.Bytes(), []byte("no such file")) {
			t.Errorf("stdout, stderr = %q, %q", out.String(), errOut.String())
		}
	})
}
