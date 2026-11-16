package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/slogx"
)

// defaultIPv6Echo is the endpoint a host learns the machine's /64 from when
// MANADE_IPV6_ECHO is unset.
const defaultIPv6Echo = "https://cloudflare.com/cdn-cgi/trace"

// Self is a host's own environment.
type Self struct {
	// Name is MANADE_HOST, the hosts key whose section the host serves.
	Name string
	// TLSDir holds ca.pem, <name>.pem and <name>-key.pem.
	TLSDir string
	// IPv6Echo is where the host learns the machine's /64: host-side, since
	// the drop of the machine's own IPv6 LAN rests on what it answers.
	IPv6Echo string
	// RegistryAuth is the Docker client config the image manager pulls with.
	RegistryAuth RegistryAuth
	// Listen is where the host API is served.
	Listen Address
	// Ceiling bounds every section the host accepts.
	Ceiling Ceiling
	// APIUID owns the API socket.
	APIUID   int
	LogLevel slog.Level
	// LogLevelInvalid reports a LOG_LEVEL that did not parse, so LogLevel is
	// the default; the caller warns once its handler is installed.
	LogLevelInvalid bool
}

// RegistryAuth is a Docker client config: File, read again at each pull so
// a rotated file takes effect, or the Value itself. Both are empty when the
// host pulls anonymously.
type RegistryAuth struct {
	Value Secret
	File  string
}

// hostCredentials are the control plane's secrets a host refuses to be given.
var hostCredentials = []envx.Key{
	"GITHUB_APP_PRIVATE_KEY", "GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE",
}

// LoadSelf reads a runner host's environment, joining every failure into
// one error.
func LoadSelf() (*Self, error) {
	s, errs := readSelf()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return s, nil
}

func readSelf() (*Self, []error) {
	var errs []error
	s := &Self{Name: envx.String("MANADE_HOST"), TLSDir: cmp.Or(envx.String("TLS_DIR"), hostTLSDir), APIUID: 65532}
	var ok bool
	s.LogLevel, ok = slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	s.LogLevelInvalid = !ok
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
	var err error
	if s.IPv6Echo, err = parseEcho(cmp.Or(envx.String("MANADE_IPV6_ECHO"), defaultIPv6Echo)); err != nil {
		errs = append(errs, fmt.Errorf("MANADE_IPV6_ECHO: %w", err))
	}
	if s.RegistryAuth, err = readRegistryAuth(); err != nil {
		errs = append(errs, err)
	}
	if s.Ceiling, err = readCeiling(); err != nil {
		errs = append(errs, err)
	}
	for _, key := range hostCredentials {
		if envx.String(key) != "" {
			errs = append(errs, fmt.Errorf("%s: a host is never given the control plane's GitHub credential; remove it from this host's environment", key))
		}
	}
	return s, errs
}

// readRegistryAuth reads MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE.
// The file form keeps its path, not its content: the image manager reads it
// at each pull. No error quotes the value.
func readRegistryAuth() (RegistryAuth, error) {
	value, file := envx.String("MANADE_REGISTRY_AUTH"), envx.String("MANADE_REGISTRY_AUTH_FILE")
	switch {
	case value != "" && file != "":
		return RegistryAuth{}, errors.New("set MANADE_REGISTRY_AUTH or MANADE_REGISTRY_AUTH_FILE, not both")
	case file != "":
		if _, err := parseHostPath(file); err != nil {
			return RegistryAuth{}, fmt.Errorf("MANADE_REGISTRY_AUTH_FILE: %w", err)
		}
		return RegistryAuth{File: file}, nil
	}
	return RegistryAuthValue(value), nil
}

// RegistryAuthValue answers the credentials of a Docker client config given
// as its content.
func RegistryAuthValue(content string) RegistryAuth { return RegistryAuth{Value: newSecret(content)} }

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
	return parseHostPort(raw)
}

func parseEcho(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != schemeHTTPS || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", invalid("want an https:// URL", raw)
	}
	return raw, nil
}
