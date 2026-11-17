package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
)

func clearControlPlaneEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"DATA_DIR", "LOG_LEVEL", "GITHUB_PROXY", "GITHUB_APP_CLIENT_ID", "GITHUB_APP_PRIVATE_KEY",
		"GITHUB_APP_PRIVATE_KEY_FILE", "GITHUB_TOKEN", "GITHUB_TOKEN_FILE", "OPS_LISTEN_ADDR", "ALLOWED_HOSTS",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

func TestLoadControlPlaneDefaults(t *testing.T) {
	clearControlPlaneEnv(t)
	cp, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cp.DataDir != "/data" || cp.LogLevel != slog.LevelInfo || cp.LogLevelInvalid {
		t.Errorf("LoadControlPlane() = %+v, want /data at info", cp)
	}
	if cp.OpsAddr != ":7465" || len(cp.AllowedHosts) != 0 {
		t.Errorf("OpsAddr, AllowedHosts = %q, %q; want :7465 and none", cp.OpsAddr, cp.AllowedHosts)
	}
}

// OPS_LISTEN_ADDR is edge-trimmed and an empty value is the default, so
// the ops listener cannot be turned off (§5.2).
func TestLoadControlPlaneOpsAddr(t *testing.T) {
	for value, want := range map[string]string{"127.0.0.1:9000": "127.0.0.1:9000", " :9000\u200b\n": ":9000", "  ": ":7465"} {
		t.Run(strings.TrimSpace(value), func(t *testing.T) {
			clearControlPlaneEnv(t)
			t.Setenv("OPS_LISTEN_ADDR", value)
			cp, err := LoadControlPlane()
			if err != nil {
				t.Fatal(err)
			}
			if cp.OpsAddr != want {
				t.Errorf("OpsAddr = %q, want %q", cp.OpsAddr, want)
			}
		})
	}
}

func TestLoadControlPlaneAllowedHosts(t *testing.T) {
	clearControlPlaneEnv(t)
	t.Setenv("ALLOWED_HOSTS", " manade , ,192.168.1.77,prometheus.lan")
	cp, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"manade", "192.168.1.77", "prometheus.lan"}; !slices.Equal(cp.AllowedHosts, want) {
		t.Errorf("AllowedHosts = %q, want %q", cp.AllowedHosts, want)
	}
}

// An entry no Host can match refuses start, naming the variable and the
// entry: the ops listener would refuse it, and a scraper whose name was
// dropped reads as a target down.
func TestLoadControlPlaneRefusesAnUnusableAllowedHost(t *testing.T) {
	for _, entry := range []string{"https://manade.lan", "manade.lan/metrics", "10.0.0.0/8"} {
		t.Run(strings.ReplaceAll(entry, "/", "_"), func(t *testing.T) {
			clearControlPlaneEnv(t)
			t.Setenv("ALLOWED_HOSTS", "manade,"+entry)
			_, err := LoadControlPlane()
			if err == nil || !strings.Contains(err.Error(), "ALLOWED_HOSTS") || !strings.Contains(err.Error(), entry) {
				t.Errorf("LoadControlPlane() = %v, want a refusal naming ALLOWED_HOSTS and %q", err, entry)
			}
		})
	}
}

func TestLoadControlPlaneReadsItsEnvironment(t *testing.T) {
	clearControlPlaneEnv(t)
	t.Setenv("DATA_DIR", "/srv/manade")
	t.Setenv("LOG_LEVEL", "debug")
	cp, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cp.DataDir != "/srv/manade" || cp.LogLevel != slog.LevelDebug {
		t.Errorf("LoadControlPlane() = %+v, want /srv/manade at debug", cp)
	}
}

func TestLoadControlPlaneRefusesARelativeDataDir(t *testing.T) {
	clearControlPlaneEnv(t)
	t.Setenv("DATA_DIR", "data")
	_, err := LoadControlPlane()
	if err == nil || !strings.Contains(err.Error(), "DATA_DIR") {
		t.Errorf("LoadControlPlane() = %v, want an error naming DATA_DIR", err)
	}
}

func TestLoadControlPlaneFlagsAnUnknownLogLevel(t *testing.T) {
	clearControlPlaneEnv(t)
	t.Setenv("LOG_LEVEL", "loud")
	cp, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if !cp.LogLevelInvalid || cp.LogLevel != slog.LevelInfo {
		t.Errorf("LoadControlPlane() = %+v, want info with LogLevelInvalid", cp)
	}
}

func testAppKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// The environment's credential is optional: a first run stores one from the
// UI.
func TestLoadControlPlaneCredential(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		clearControlPlaneEnv(t)
		cp, err := LoadControlPlane()
		if err != nil {
			t.Fatal(err)
		}
		if cp.GitHub.Set() || cp.GitHub.Proxy != nil {
			t.Errorf("GitHub = %+v, want no credential and no proxy", cp.GitHub)
		}
	})
	t.Run("token", func(t *testing.T) {
		clearControlPlaneEnv(t)
		t.Setenv("GITHUB_TOKEN", "test-token")
		cp, err := LoadControlPlane()
		if err != nil {
			t.Fatal(err)
		}
		if cp.GitHub.Token.Reveal() != "test-token" || cp.GitHub.AppKey != nil {
			t.Errorf("GitHub = %+v, want the token alone", cp.GitHub)
		}
	})
	t.Run("app", func(t *testing.T) {
		clearControlPlaneEnv(t)
		t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.test")
		t.Setenv("GITHUB_APP_PRIVATE_KEY", testAppKeyPEM(t))
		t.Setenv("GITHUB_PROXY", "http://proxy.lan:3128")
		cp, err := LoadControlPlane()
		if err != nil {
			t.Fatal(err)
		}
		if cp.GitHub.AppKey == nil || cp.GitHub.Token.Reveal() != "" || cp.GitHub.Proxy.Host != "proxy.lan:3128" {
			t.Errorf("GitHub = %+v, want the App key behind the proxy", cp.GitHub)
		}
	})
}

func TestLoadControlPlaneCredentialRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"both", map[string]string{"GITHUB_TOKEN": "t", "GITHUB_APP_CLIENT_ID": "Iv1.test"}, "not both"},
		{"clientIDAlone", map[string]string{"GITHUB_APP_CLIENT_ID": "Iv1.test"}, "GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY"},
		{"keyAlone", map[string]string{"GITHUB_APP_PRIVATE_KEY": "x"}, "GITHUB_APP_CLIENT_ID with GITHUB_APP_PRIVATE_KEY"},
		{"badKey", map[string]string{"GITHUB_APP_CLIENT_ID": "Iv1.test", "GITHUB_APP_PRIVATE_KEY": "not a key"}, "GITHUB_APP_PRIVATE_KEY:"},
		{"proxyScheme", map[string]string{"GITHUB_PROXY": "socks5://p:1080"}, "GITHUB_PROXY: want an http:// or https:// proxy URL"},
		{"proxyPath", map[string]string{"GITHUB_PROXY": "http://p:3128/x"}, "GITHUB_PROXY: want an http:// or https:// proxy URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearControlPlaneEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := LoadControlPlane()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("LoadControlPlane() = %v, want %q", err, tc.want)
			}
		})
	}
}

// A proxy password never reaches an error.
func TestLoadControlPlaneProxyErrorHidesItsValue(t *testing.T) {
	clearControlPlaneEnv(t)
	t.Setenv("GITHUB_PROXY", "ftp://user:hunter2@p:21")
	_, err := LoadControlPlane()
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("LoadControlPlane() = %v, want a refusal that does not quote the value", err)
	}
}
