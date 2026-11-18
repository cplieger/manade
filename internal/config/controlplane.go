package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/cplieger/forgeapi/v3/creds"
	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/slogx"
)

// defaultOpsAddr is OPS_LISTEN_ADDR's default, the port after the product's
// 7463 and the hosts' 7464 that the fleet's port table, IANA and
// Prometheus's default port allocations all leave free.
const defaultOpsAddr = ":7465"

// ControlPlane is the control plane's environment.
type ControlPlane struct {
	GitHub GitHub
	// DataDir holds the store; it is absolute.
	DataDir string
	// OpsAddr is the ops listener's address, never empty.
	OpsAddr string
	// AllowedHosts are the Host names the host check admits besides a
	// request from the machine itself; each is one CanonicalHost accepts.
	AllowedHosts []string
	LogLevel     slog.Level
	// LogLevelInvalid reports a LOG_LEVEL that did not parse, so LogLevel is
	// the default; the caller warns once its handler is installed.
	LogLevelInvalid bool
}

// GitHub is the environment's GitHub credential, at most one of AppKey and
// Token, and the proxy for every GitHub call.
type GitHub struct {
	AppKey *creds.AppKey
	// Proxy is nil without GITHUB_PROXY. Its userinfo may hold a password,
	// so it is never logged.
	Proxy *url.URL
	Token Secret
}

// Set reports whether the environment holds a credential.
func (g *GitHub) Set() bool { return g.AppKey != nil || g.Token.Reveal() != "" }

// LoadControlPlane reads the control plane's environment.
func LoadControlPlane() (*ControlPlane, error) {
	cp := &ControlPlane{
		DataDir: cmp.Or(envx.String("DATA_DIR"), "/data"),
		OpsAddr: httpserver.ListenAddr(envx.String("OPS_LISTEN_ADDR"), defaultOpsAddr),
	}
	var ok bool
	cp.LogLevel, ok = slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	cp.LogLevelInvalid = !ok
	var errs []error
	if !filepath.IsAbs(cp.DataDir) {
		errs = append(errs, fmt.Errorf("config: DATA_DIR: %s is not an absolute path", quote(cp.DataDir)))
	}
	if raw := envx.String("GITHUB_PROXY"); raw != "" {
		proxy, err := parseProxy(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("config: GITHUB_PROXY: %w", err))
		}
		cp.GitHub.Proxy = proxy
	}
	if err := readCredential(&cp.GitHub); err != nil {
		errs = append(errs, err)
	}
	var err error
	if cp.AllowedHosts, err = allowedHosts(envx.String("ALLOWED_HOSTS")); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return cp, nil
}

// readCredential sets at most one credential form. No error quotes a secret
// or a client ID: envx's errors name only the key.
func readCredential(gh *GitHub) error {
	var errs []error
	secret := func(key envx.Key) (string, bool) {
		v, err := envx.Secret(key)
		if errors.As(err, new(*envx.MissingError)) {
			return "", false
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("config: %w", err))
			return "", false
		}
		return v, true
	}
	clientID := envx.String("GITHUB_APP_CLIENT_ID")
	pemKey, hasKey := secret("GITHUB_APP_PRIVATE_KEY")
	token, hasToken := secret("GITHUB_TOKEN")
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	hasApp := clientID != "" || hasKey
	switch {
	case hasApp && hasToken:
		return errors.New("config: set either GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or GITHUB_TOKEN, not both")
	case hasToken:
		gh.Token = newSecret(token)
	case !hasApp:
	case clientID == "" || !hasKey:
		return errors.New("config: set GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY, or neither")
	default:
		key, err := creds.ParseAppKeyPEM(clientID, []byte(pemKey))
		if err != nil {
			return fmt.Errorf("config: GITHUB_APP_PRIVATE_KEY: %w", err)
		}
		gh.AppKey = key
	}
	return nil
}

// allowedHosts reads ALLOWED_HOSTS, a comma-separated list. An entry no Host
// can match refuses start rather than being dropped: the ops listener refuses
// one too, and a scraper whose name is dropped reads as a target down.
func allowedHosts(raw string) ([]string, error) {
	var hosts []string
	for e := range strings.SplitSeq(raw, ",") {
		if e = strings.TrimSpace(e); e != "" {
			hosts = append(hosts, e)
		}
	}
	_, invalid := httpserver.ParseHostList(hosts)
	if len(invalid) == 0 {
		return hosts, nil
	}
	for i, v := range invalid {
		invalid[i] = quote(v)
	}
	return nil, fmt.Errorf("config: ALLOWED_HOSTS: no Host can match %s: give bare host names or IP addresses, with no scheme, path or prefix", strings.Join(invalid, ", "))
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
