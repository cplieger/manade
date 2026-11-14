package main

import (
	"fmt"
	"io"
	"os"

	"github.com/cplieger/manade/internal/host/platform"
)

// stdout is where storage-driver answers its pick; a test seam.
var stdout io.Writer = os.Stdout

// pickStorage is the pick storage-driver makes; a test seam, since the real
// one mounts a test overlay.
var pickStorage = platform.PickStorage

// runStorageDriver picks the graph driver for a job's own daemon on dir,
// the host's pick with vfs where neither overlay2 nor btrfs fits: the
// driver alone on stdout, for the docker image's wrapper to pass to
// dockerd, and why on stderr, for the job's log.
func runStorageDriver(dir string, stderr io.Writer) int {
	p, err := pickStorage(platform.System, dir, platform.ForJob)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "manade storage-driver:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, p.Driver)
	_, _ = fmt.Fprintf(stderr, "manade storage-driver: %s (%s)\n", p.Driver, p.Why)
	return 0
}
