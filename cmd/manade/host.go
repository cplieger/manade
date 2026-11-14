package main

import (
	"context"
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

func serveHost(stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	level := slogx.Setup(slogx.Options{Output: stderr})
	log := slog.Default()
	cfg, err := config.Load(ctx, config.RoleHost)
	if err != nil {
		log.Error("The runner host's config does not validate", "error", err)
		return 1
	}
	level.Set(cfg.LogLevel)
	if cfg.LogLevelInvalid {
		log.Warn("LOG_LEVEL is not a known level, so the host logs at info")
	}
	log.Info("The runner host is starting", "host", cfg.Self.Name, "version", version)
	err = host.Run(ctx, cfg, &host.Options{
		Log: log, Version: version, Hangup: hup,
		Reload: func(ctx context.Context) (*config.Config, error) { return config.Load(ctx, config.RoleHost) },
		Health: health.NewStatus(health.WithMarker(host.HealthMarker), health.WithLogger(log)),
	})
	if err != nil {
		log.Error("The runner host stopped on a failure", "error", err)
		return 1
	}
	return 0
}
