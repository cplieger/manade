package main

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/cplieger/manade/internal/host"
)

// A host that restarts to apply its section exits 75, which on-failure
// restarts as unless-stopped does.
func TestHostExit(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"stopped":   {nil, 0},
		"restart":   {fmt.Errorf("joined: %w", errors.Join(host.ErrRestart, nil)), 75},
		"a failure": {errors.New("dockerd exited"), 1},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hostExit(log, tc.err); got != tc.want {
				t.Errorf("hostExit(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
