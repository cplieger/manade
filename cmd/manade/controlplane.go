package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/cplieger/go/slogx"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/store"
)

const controlPlaneUsage = "usage: manade control-plane"

// runControlPlane runs `manade control-plane`.
func runControlPlane(args []string, stderr io.Writer) int {
	if len(args) != 0 {
		_, _ = fmt.Fprintln(stderr, controlPlaneUsage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return serveControlPlane(ctx, stderr)
}

// serveControlPlane holds the store open until ctx ends.
func serveControlPlane(ctx context.Context, stderr io.Writer) int {
	level := slogx.Setup(slogx.Options{Output: stderr})
	log := slog.Default()
	cfg, err := config.LoadControlPlane()
	if err != nil {
		log.Error("The control plane's environment does not validate", "error", err)
		return 1
	}
	level.Set(cfg.LogLevel)
	if cfg.LogLevelInvalid {
		log.Warn("LOG_LEVEL is not a known level, so the control plane logs at info")
	}
	path := filepath.Join(cfg.DataDir, store.File)
	st, err := store.Open(path, store.Options{Log: log, Build: version})
	if err != nil {
		log.Error("The control plane's store did not open", "error", err)
		return 1
	}
	log.Info("The control plane is running", "version", version, "store", path)
	<-ctx.Done()
	if err := st.Close(); err != nil {
		log.Error("The control plane's store did not close cleanly", "error", err)
		return 1
	}
	return 0
}
