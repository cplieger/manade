// Package config loads manade's configuration for both roles, the
// orchestrator and a runner host: one YAML file every role reads whole, and
// the environment for what is per-container or secret. Every environment read
// happens here.
//
// Load validates everything it can see on its own and joins every failure
// into one error naming each field's path. Checks that need a host's own
// facts (its cgroup limits, its filesystems, its routes) belong to the host.
package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/go/yamlenv"
)

const maxConfigBytes = 1 << 20

// Role is the part a process plays, which decides the environment it reads.
type Role int

// The roles; the zero Role is neither.
const (
	RoleOrchestrator Role = iota + 1
	RoleHost
)

// Config is the validated configuration. Hosts and Profiles are sorted by
// name.
type Config struct {
	// Orchestrator is set for RoleOrchestrator only.
	Orchestrator *Orchestrator
	// Self is set for RoleHost only.
	Self *Self
	// File is the config file's absolute path: CONFIG_FILE, resolved against
	// the working directory.
	File             string
	Scopes           []Scope
	Hosts            []Host
	Profiles         []Profile
	HeadReserveAfter time.Duration
	LogLevel         slog.Level
	// LogLevelInvalid reports a LOG_LEVEL that did not parse, so LogLevel is
	// the default; the caller warns once its handler is installed.
	LogLevelInvalid bool
}

// Host answers the host named name.
func (c *Config) Host(name string) (*Host, bool) {
	for i := range c.Hosts {
		if c.Hosts[i].Name == name {
			return &c.Hosts[i], true
		}
	}
	return nil, false
}

// Profile answers the profile named name.
func (c *Config) Profile(name string) (*Profile, bool) {
	for i := range c.Profiles {
		if c.Profiles[i].Name == name {
			return &c.Profiles[i], true
		}
	}
	return nil, false
}

// Secret holds a token that prints as "[redacted]" through every fmt verb and
// slog; only Reveal answers the value. The token sits behind a pointer
// because fmt formats a holder's unexported field by reflection, skipping the
// field's own methods, and prints a pointer below the top level as an
// address, never what it points to.
type Secret struct{ v *string }

func newSecret(token string) Secret {
	if token == "" {
		return Secret{}
	}
	return Secret{v: &token}
}

// Reveal answers the token, or "" when none is set.
func (s Secret) Reveal() string {
	if s.v == nil {
		return ""
	}
	return *s.v
}

// String prints "[redacted]" for a set token.
func (s Secret) String() string {
	if s.v == nil {
		return ""
	}
	return "[redacted]"
}

// Format prints String for every verb, %#v included.
//
//deadset:ignore DS1102 -- fmt.Formatter, reached by any print of a Secret, %#v included
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue prints String.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Load reads role's environment and the config file and validates both,
// joining every failure of the two into one error.
func Load(ctx context.Context, role Role) (*Config, error) {
	cfg, envErr := readEnv(role)
	if envErr != nil {
		envErr = fmt.Errorf("config: %w", envErr)
	}
	data, err := filesafe.ReadBounded(ctx, cfg.File, maxConfigBytes)
	if err != nil {
		return nil, errors.Join(envErr, fmt.Errorf("config: CONFIG_FILE: %w", err))
	}
	if err := errors.Join(envErr, build(cfg, data)); err != nil {
		return nil, err
	}
	return cfg, nil
}

// build validates the file into cfg, which holds the environment's settings.
func build(cfg *Config, data []byte) error {
	var f fileConfig
	// The allow function admits no name, so a ${VAR} anywhere stays literal:
	// no field needs a secret, and expansion could carry one into a log line.
	// Keys are echoed: with nothing expanded, a key is only what the
	// operator wrote.
	noVars := func(string) bool { return false }
	if _, err := yamlenv.Load(data, &f, noVars, yamlenv.WithSanitizeOptions(yamlenv.WithUnknownKeyEcho(true))); err != nil {
		return fmt.Errorf("config: CONFIG_FILE: %w", err)
	}
	var p problems
	f.apply(cfg, &p)
	return p.err()
}
