package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/manade/internal/host/probe"
)

const probeUsage = "usage: manade probe [listen]"

// stdin is what `manade probe` reads its checks from; a test seam.
var stdin io.Reader = os.Stdin

// runProbe is the probe container's side of a probe run: one outcome line
// for each check line on its standard input, or with listen the listener
// standing in for another job.
func runProbe(args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	var err error
	switch {
	case len(args) == 0:
		err = probe.Serve(ctx, stdin, stdout)
	case len(args) == 1 && args[0] == "listen":
		err = probe.Listen(ctx)
	default:
		_, _ = fmt.Fprintln(stderr, probeUsage)
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "manade probe:", err)
		return 1
	}
	return 0
}
