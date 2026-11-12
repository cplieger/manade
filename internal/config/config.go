// Package config loads manade's configuration: the YAML file for scopes,
// profiles, capacity, the queue, images and networks, and the environment for
// what is per-deployment or secret. Every environment read happens here.
//
// Load validates everything it can see on its own and joins every failure
// into one error naming each field's path. CheckDaemon adds the checks that
// need the Docker daemon's own facts, once the daemon has answered.
package config

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/go/slogx"
	"github.com/cplieger/go/yamlenv"
)

const maxConfigBytes = 1 << 20

// poolNameRe bounds POOL_NAME at 14 bytes so the isolated network's bridge
// interface, the pool name plus one digit, fits Linux's 15-byte IFNAMSIZ.
var poolNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,13}$`)

// Config is the validated configuration. Profiles are sorted by name.
type Config struct {
	GitHub GitHub
	// MetricsListenAddr is "" when no listener is wanted; the bind is its
	// only check.
	MetricsListenAddr string
	PoolName          string
	DockerSocket      string
	DiagDir           string
	Scopes            []Scope
	Profiles          []Profile
	Network           Network
	Capacity          Capacity
	DiagRetention     time.Duration
	HeadReserveAfter  time.Duration
	ImageRefresh      time.Duration
	LogLevel          slog.Level
	// LogLevelInvalid reports a LOG_LEVEL that did not parse, so LogLevel is
	// the default; the caller warns once its handler is installed.
	LogLevelInvalid bool
}

// GitHub holds the one credential form and the proxy for GitHub traffic.
// Exactly one of AppKey and Token is set.
type GitHub struct {
	AppKey *creds.AppKey
	// Proxy is nil without GITHUB_PROXY. Its userinfo may hold a password:
	// log it only through Redacted.
	Proxy *url.URL
	Token Secret
}

// Secret holds a token that prints as "[redacted]" through every fmt verb and
// slog; only Reveal answers the value.
type Secret struct{ v string }

// Reveal answers the token, or "" when none is set.
func (s Secret) Reveal() string { return s.v }

// String prints "[redacted]" for a set token.
//
//deadset:ignore DS1102 -- fmt.Stringer, reached by any print of a Secret, which must never show the token
func (s Secret) String() string {
	if s.v == "" {
		return ""
	}
	return "[redacted]"
}

// Format prints String for every verb, %#v included.
//
//deadset:ignore DS1102 -- fmt.Formatter, reached by any print of a Secret, %#v included
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue prints String.
//
//deadset:ignore DS1102 -- slog.LogValuer, reached when a Secret is logged
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Load reads the environment and the config file and validates both,
// joining every failure of the two into one error.
func Load(ctx context.Context) (*Config, error) {
	cfg, configFile, envErr := readEnv()
	if envErr != nil {
		envErr = fmt.Errorf("config: %w", envErr)
	}
	data, err := filesafe.ReadBounded(ctx, configFile, maxConfigBytes)
	if err != nil {
		return nil, errors.Join(envErr, fmt.Errorf("config: CONFIG_FILE: %w", err))
	}
	if err := errors.Join(envErr, build(cfg, data)); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readEnv answers the environment's settings and the config file's path.
func readEnv() (*Config, string, error) {
	var errs []error
	cfg := &Config{
		PoolName:     cmp.Or(envx.String("POOL_NAME"), "manade"),
		DockerSocket: cmp.Or(envx.String("DOCKER_SOCKET"), "/var/run/docker.sock"),
		DiagDir:      envx.String("DIAG_DIR"),
		// An untrimmed " :9464" fails the bind on a host lookup the
		// operator cannot see.
		MetricsListenAddr: strings.TrimSpace(envx.String("METRICS_LISTEN_ADDR")),
		DiagRetention:     168 * time.Hour,
	}
	if !poolNameRe.MatchString(cfg.PoolName) {
		errs = append(errs, fmt.Errorf("POOL_NAME: %w", invalid("want 1 to 14 lowercase letters, digits or dashes, starting with a letter", cfg.PoolName)))
	}
	if _, err := parseHostPath(cfg.DockerSocket); err != nil {
		errs = append(errs, fmt.Errorf("DOCKER_SOCKET: %w", err))
	}
	switch retention, set, err := envx.DurationStrict("DIAG_RETENTION"); {
	case err != nil:
		errs = append(errs, err)
	case set && retention <= 0:
		errs = append(errs, errors.New("DIAG_RETENTION: want a positive duration"))
	case set:
		cfg.DiagRetention = retention
	}
	var ok bool
	cfg.LogLevel, ok = slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	cfg.LogLevelInvalid = !ok
	if raw := envx.String("GITHUB_PROXY"); raw != "" {
		proxy, err := parseProxy(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_PROXY: %w", err))
		}
		cfg.GitHub.Proxy = proxy
	}
	errs = append(errs, readCredential(&cfg.GitHub)...)
	return cfg, cmp.Or(envx.String("CONFIG_FILE"), "/config/config.yaml"), errors.Join(errs...)
}

// readCredential sets exactly one credential form. No error quotes a secret
// or a client ID: envx's errors name only the key.
func readCredential(gh *GitHub) []error {
	var errs []error
	secret := func(key envx.Key) (string, bool) {
		v, err := envx.Secret(key)
		if errors.As(err, new(*envx.MissingError)) {
			return "", false
		}
		if err != nil {
			errs = append(errs, err)
			return "", false
		}
		return v, true
	}
	clientID := envx.String("GITHUB_APP_CLIENT_ID")
	pemKey, hasKey := secret("GITHUB_APP_PRIVATE_KEY")
	token, hasToken := secret("GITHUB_TOKEN")
	if len(errs) > 0 {
		return errs
	}
	hasApp := clientID != "" || hasKey
	switch {
	case hasApp && hasToken:
		return []error{errors.New("set either GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or GITHUB_TOKEN, not both")}
	case hasToken:
		gh.Token = Secret{v: token}
	case clientID == "" || !hasKey:
		return []error{errors.New("set GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or GITHUB_TOKEN")}
	default:
		key, err := creds.ParseAppKeyPEM(clientID, []byte(pemKey))
		if err != nil {
			return []error{fmt.Errorf("GITHUB_APP_PRIVATE_KEY: %w", err)}
		}
		gh.AppKey = key
	}
	return nil
}

func parseProxy(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		// The value may carry a proxy password, so it is never echoed.
		return nil, errors.New("want an http:// or https:// proxy URL with a host and no path or query")
	}
	return u, nil
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
