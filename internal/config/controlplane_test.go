package config

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func clearControlPlaneEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"DATA_DIR", "LOG_LEVEL"} {
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
