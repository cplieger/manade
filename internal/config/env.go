package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/forgeapi/v2/creds"
	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/slogx"
)

// Orchestrator holds the orchestrator's environment.
type Orchestrator struct {
	GitHub GitHub
	// MetricsListenAddr is "" when no listener is wanted; the bind is its
	// only check.
	MetricsListenAddr string
	PoolName          string
	DiagDir           string
	// TLSDir holds ca.pem, orchestrator.pem and orchestrator-key.pem.
	TLSDir string
	// secretFiles are the credential _FILE paths the environment names,
	// made absolute.
	secretFiles   []secretPath
	DiagRetention time.Duration
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

// Self is a host's own environment.
type Self struct {
	// Name is MANADE_HOST, the hosts key whose section the host serves.
	Name string
	// TLSDir holds ca.pem, <name>.pem and <name>-key.pem.
	TLSDir string
	// Listen is where the host API is served.
	Listen Address
	// APIUID owns the API socket.
	APIUID int
}

// poolNameRe bounds POOL_NAME at 14 bytes, which keeps a runner's name,
// <pool>-<profile>-<12 hex digits>, at 60 bytes or fewer.
var poolNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,13}$`)

// hostCredentials are the orchestrator's secrets a host refuses to be given.
var hostCredentials = []envx.Key{
	"GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE",
}

// readEnv answers the environment's settings for role.
func readEnv(role Role) (*Config, error) {
	var errs []error
	file, err := filepath.Abs(cmp.Or(envx.String("CONFIG_FILE"), "/config/config.yaml"))
	if err != nil {
		errs = append(errs, fmt.Errorf("CONFIG_FILE: %w", err))
	}
	cfg := &Config{File: file}
	var ok bool
	cfg.LogLevel, ok = slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	cfg.LogLevelInvalid = !ok
	var roleErrs []error
	switch role {
	case RoleOrchestrator:
		cfg.Orchestrator, roleErrs = readOrchestrator()
	case RoleHost:
		cfg.Self, roleErrs = readSelf()
	default:
		roleErrs = []error{fmt.Errorf("unknown role %d", role)}
	}
	errs = append(errs, roleErrs...)
	return cfg, errors.Join(errs...)
}

func readOrchestrator() (*Orchestrator, []error) {
	var errs []error
	o := &Orchestrator{
		PoolName: cmp.Or(envx.String("POOL_NAME"), "manade"),
		DiagDir:  envx.String("DIAG_DIR"),
		TLSDir:   cmp.Or(envx.String("TLS_DIR"), "/secrets/tls"),
		// An untrimmed " :9464" fails the bind on a host lookup the
		// operator cannot see.
		MetricsListenAddr: strings.TrimSpace(envx.String("METRICS_LISTEN_ADDR")),
		DiagRetention:     168 * time.Hour,
	}
	if !poolNameRe.MatchString(o.PoolName) {
		errs = append(errs, fmt.Errorf("POOL_NAME: %w", invalid("want 1 to 14 lowercase letters, digits or dashes, starting with a letter", o.PoolName)))
	}
	if _, err := parseHostPath(o.TLSDir); err != nil {
		errs = append(errs, fmt.Errorf("TLS_DIR: %w", err))
	}
	switch retention, set, err := envx.DurationStrict("DIAG_RETENTION"); {
	case err != nil:
		errs = append(errs, err)
	case set && retention <= 0:
		errs = append(errs, errors.New("DIAG_RETENTION: want a positive duration"))
	case set:
		o.DiagRetention = retention
	}
	if raw := envx.String("GITHUB_PROXY"); raw != "" {
		proxy, err := parseProxy(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_PROXY: %w", err))
		}
		o.GitHub.Proxy = proxy
	}
	for _, key := range []envx.Key{"GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN_FILE"} {
		raw := envx.String(key)
		if raw == "" {
			continue
		}
		abs, err := filepath.Abs(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		o.secretFiles = append(o.secretFiles, secretPath{key: string(key), path: abs, remedy: "the file a mount of its own"})
	}
	errs = append(errs, readCredential(&o.GitHub)...)
	return o, errs
}

func readSelf() (*Self, []error) {
	var errs []error
	s := &Self{Name: envx.String("MANADE_HOST"), TLSDir: cmp.Or(envx.String("TLS_DIR"), hostTLSDir), APIUID: 65532}
	switch {
	case s.Name == "":
		errs = append(errs, errors.New("MANADE_HOST: required, the hosts key this host serves"))
	case CheckHostName(s.Name) != nil:
		errs = append(errs, fmt.Errorf("MANADE_HOST: %w", CheckHostName(s.Name)))
	default:
		var err error
		if s.Listen, err = parseListen(envx.String("MANADE_API_LISTEN"), s.Name); err != nil {
			errs = append(errs, fmt.Errorf("MANADE_API_LISTEN: %w", err))
		}
	}
	if _, err := parseHostPath(s.TLSDir); err != nil {
		errs = append(errs, fmt.Errorf("TLS_DIR: %w", err))
	}
	if raw := envx.String("MANADE_API_UID"); raw != "" {
		uid, err := strconv.Atoi(raw)
		if err != nil || uid < 0 || uid > 1<<31-2 {
			errs = append(errs, fmt.Errorf("MANADE_API_UID: %w", invalid("want a numeric uid", raw)))
		}
		s.APIUID = uid
	}
	for _, key := range hostCredentials {
		if envx.String(key) != "" {
			errs = append(errs, fmt.Errorf("%s: a host is never given the orchestrator's GitHub credential; remove it from this host's environment", key))
		}
	}
	return s, errs
}

// parseListen answers the API's listen address: the host's default socket
// when raw is empty, a unix:// path, or a TCP host:port.
func parseListen(raw, name string) (Address, error) {
	if raw == "" {
		return Address{Socket: defaultSocket(name)}, nil
	}
	if path, ok := strings.CutPrefix(raw, "unix://"); ok {
		if _, err := parseHostPath(path); err != nil {
			return Address{}, err
		}
		return Address{Socket: path}, nil
	}
	return parseHostPort(raw, true)
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
		gh.Token = newSecret(token)
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
	if err != nil || (u.Scheme != "http" && u.Scheme != schemeHTTPS) || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		// The value may carry a proxy password, so it is never echoed.
		return nil, errors.New("want an http:// or https:// proxy URL with a host and no path or query")
	}
	return u, nil
}
