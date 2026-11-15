package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/cplieger/go/envx"
	"github.com/cplieger/go/slogx"
)

// ControlPlane is the control plane's environment.
type ControlPlane struct {
	// DataDir holds the store; it is absolute.
	DataDir  string
	LogLevel slog.Level
	// LogLevelInvalid reports a LOG_LEVEL that did not parse, so LogLevel is
	// the default; the caller warns once its handler is installed.
	LogLevelInvalid bool
}

// LoadControlPlane reads the control plane's environment.
func LoadControlPlane() (*ControlPlane, error) {
	cp := &ControlPlane{DataDir: cmp.Or(envx.String("DATA_DIR"), "/data")}
	var ok bool
	cp.LogLevel, ok = slogx.ParseLevel(envx.String("LOG_LEVEL"), slog.LevelInfo)
	cp.LogLevelInvalid = !ok
	if !filepath.IsAbs(cp.DataDir) {
		return nil, fmt.Errorf("config: DATA_DIR: %s is not an absolute path", quote(cp.DataDir))
	}
	return cp, nil
}
