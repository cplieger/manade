package images

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/manade/internal/config"
)

// authLimit caps the registry_auth file, a Docker client config.
const authLimit = 1 << 20

// hubKeys are the registries a Docker client config files Docker Hub's
// credentials under, the CLI's own (https://index.docker.io/v1/) first.
var hubKeys = []string{"index.docker.io", "docker.io", "registry-1.docker.io"}

// errAuth is a registry_auth file the host cannot use. Its text never
// quotes the file.
var errAuth = errors.New("registry_auth")

// auth answers the X-Registry-Auth value for ref's registry from the host's
// registry_auth file, read at each call so a rotated file takes effect, or
// "" when the host has none or it names no entry for that registry.
func (m *Manager) auth(ctx context.Context, ref string) (string, error) {
	m.mu.Lock()
	path := m.host.RegistryAuth
	m.mu.Unlock()
	if path == "" {
		return "", nil
	}
	b, err := filesafe.ReadBounded(ctx, path, authLimit)
	if err != nil {
		return "", fmt.Errorf("%w: reading %s: %w", errAuth, path, err)
	}
	return encodeAuth(b, ref, path)
}

// clientConfig is the part of a Docker client config (~/.docker/config.json)
// a pull's credentials come from. Credential helpers are not read: the host
// image ships none.
type clientConfig struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
}

// encodeAuth answers the credentials of file for ref's registry as the
// Engine API takes them: base64url JSON of an AuthConfig
// (https://docs.docker.com/reference/api/engine/version/v1.51/#section/Authentication).
func encodeAuth(file []byte, ref, path string) (string, error) {
	var cfg clientConfig
	if json.Unmarshal(file, &cfg) != nil {
		return "", fmt.Errorf("%w: %s is not a Docker client config", errAuth, path)
	}
	keys := hubKeys
	if name, _ := config.Registry(ref); name != "" {
		first, _, _ := strings.Cut(ref, "/")
		keys = []string{first}
	}
	key, ok := cfg.find(keys)
	if !ok {
		return "", nil
	}
	a := cfg.Auths[key]
	out := map[string]string{"serveraddress": key}
	switch {
	case a.IdentityToken != "":
		out["identitytoken"] = a.IdentityToken
	case a.Auth != "":
		raw, err := base64.StdEncoding.DecodeString(a.Auth)
		user, pass, ok := strings.Cut(string(raw), ":")
		if err != nil || !ok {
			return "", fmt.Errorf("%w: %s: the auth of %s is not base64 of user:password", errAuth, path, key)
		}
		out["username"], out["password"] = user, pass
	default:
		out["username"], out["password"] = a.Username, a.Password
	}
	b, _ := json.Marshal(out) // a map of strings always encodes
	return base64.URLEncoding.EncodeToString(b), nil
}

// find answers the first key of cfg, in sorted order, whose registry is the
// first of want that any key names.
func (cfg *clientConfig) find(want []string) (string, bool) {
	keys := slices.Sorted(maps.Keys(cfg.Auths))
	for _, w := range want {
		for _, k := range keys {
			if registryKey(k) == w {
				return k, true
			}
		}
	}
	return "", false
}

// registryKey answers a client config key's registry: its host and port,
// without the scheme or path the CLI may write.
func registryKey(key string) string {
	key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
	host, _, _ := strings.Cut(key, "/")
	return host
}
