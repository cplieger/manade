package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/host"
)

const hostUsage = "usage: manade host [health]"

// runHost runs `manade host`, the runner host's supervisor, or `manade host
// health`, its container healthcheck.
func runHost(args []string, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		return serveHost(stderr)
	case len(args) == 1 && args[0] == "health":
		health.RunProbe(host.HealthMarker, health.WithMaxAge(host.HealthLease.Duration()))
		return 0
	}
	_, _ = fmt.Fprintln(stderr, hostUsage)
	return 2
}

// restartCode is the exit of a host that restarts to apply its section:
// non-zero, so on-failure restarts it too (EX_TEMPFAIL, sysexits.h).
const restartCode = 75

func serveHost(stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	level := slogx.Setup(slogx.Options{Output: stderr})
	log := slog.Default()
	self, err := config.LoadSelf()
	if err != nil {
		log.Error("The runner host's environment does not validate", "error", err)
		return 1
	}
	level.Set(self.LogLevel)
	if self.LogLevelInvalid {
		log.Warn("LOG_LEVEL is not a known level, so the host logs at info")
	}
	log.Info("The runner host is starting", "host", self.Name, "version", version)
	err = host.Run(ctx, self, &host.Options{
		Log: log, Version: version,
		Health: health.NewStatus(health.WithMarker(host.HealthMarker), health.WithLogger(log)),
	})
	return hostExit(log, err)
}

// hostExit is the exit code of a host whose Run answered err.
func hostExit(log *slog.Logger, err error) int {
	switch {
	case errors.Is(err, host.ErrRestart):
		log.Info("The runner host exits to restart on its new section", "code", restartCode)
		return restartCode
	case err != nil:
		log.Error("The runner host stopped on a failure", "error", err)
		return 1
	}
	return 0
}
