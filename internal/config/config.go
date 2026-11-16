// Package config is manade's settings model: the one parser that turns a
// settings document (JSON) into the validated model or field errors by
// path, for every write path and for a host's own check, and each role's
// environment. Every environment read happens here.
//
// The parser validates everything a document states on its own and reports every
// failure at its field path. Checks that need a host's own facts (its cgroup
// limits, its filesystems, its routes) belong to the host.
package config

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/go/filesafe"
)

const maxDocumentBytes = 1 << 20

// Config is the validated configuration. Hosts and Profiles are sorted by
// name.
type Config struct {
	// Self is set on a runner host only.
	Self *Self
	// File is the host's settings document's absolute path: CONFIG_FILE,
	// resolved against the working directory.
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

// fieldError is one refusal at a field path such as
// "hosts.nas.egress.allow[0].to"; the empty path is the document itself.
type fieldError struct {
	Path    string
	Message string
}

// fieldErrors is every refusal of one document, in the order found.
type fieldErrors []fieldError

func (fe fieldErrors) Error() string {
	lines := make([]string, len(fe))
	for i, e := range fe {
		lines[i] = e.Path + ": " + e.Message
		if e.Path == "" {
			lines[i] = e.Message
		}
	}
	return "config: " + strings.Join(lines, "\n")
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
//
//deadset:ignore DS1101 -- slog.LogValuer, reached by any slog attribute holding a Secret
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// LoadHost reads a runner host's environment and its settings document and
// validates both, joining every failure of the two into one error.
func LoadHost(ctx context.Context) (*Config, error) {
	cfg, envErr := readHostEnv()
	if envErr != nil {
		envErr = fmt.Errorf("config: %w", envErr)
	}
	data, err := filesafe.ReadBounded(ctx, cfg.File, maxDocumentBytes)
	if err != nil {
		return nil, errors.Join(envErr, fmt.Errorf("config: CONFIG_FILE: %w", err))
	}
	if err := errors.Join(envErr, build(cfg, data, nil)); err != nil {
		return nil, err
	}
	return cfg, nil
}

// build validates settings document data into cfg, which holds the
// environment's settings. prev is the model data replaces, or nil for a
// first document; the rule on a profile being deleted reads it.
func build(cfg *Config, data []byte, prev *Config) error {
	var d document
	if err := json.Unmarshal(data, &d, json.RejectUnknownMembers(true)); err != nil {
		return fieldErrors{decodeError(err)}
	}
	var p problems
	d.apply(cfg, &p)
	if prev != nil {
		cfg.checkDeleting(prev, &p)
	}
	return p.err()
}

// decodeError answers a document that did not decode as its one refusal: a
// decode stops at the first value it cannot read.
func decodeError(err error) fieldError {
	var se *json.SemanticError
	var sy *jsontext.SyntacticError
	switch {
	case errors.As(err, &sy):
		return fieldError{Path: pointerPath(sy.JSONPointer), Message: "want one JSON object: " + sy.Err.Error()}
	case !errors.As(err, &se):
		return fieldError{Message: "want one JSON object: " + err.Error()}
	}
	path := pointerPath(se.JSONPointer)
	switch {
	case errors.Is(se.Err, json.ErrUnknownName):
		return fieldError{Path: path, Message: "unknown key"}
	case se.Err != nil:
		return fieldError{Path: path, Message: se.Err.Error()}
	case se.GoType == nil:
		return fieldError{Path: path, Message: "want another kind of value"}
	}
	t := se.GoType
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return fieldError{Path: path, Message: "want an object"}
	case reflect.Slice:
		return fieldError{Path: path, Message: "want a list"}
	}
	return fieldError{Path: path, Message: "want a string"}
}

// pointerPath answers a JSON pointer as a field path. A number is a list
// index everywhere but as a host's or a profile's name, the document's only
// maps.
func pointerPath(ptr jsontext.Pointer) string {
	var b strings.Builder
	prev := ""
	depth := 0
	for tok := range ptr.Tokens() {
		_, err := strconv.Atoi(tok)
		switch {
		case err == nil && (depth != 1 || (prev != "hosts" && prev != "profiles")):
			b.WriteString("[" + tok + "]")
		case depth == 0:
			b.WriteString(tok)
		default:
			b.WriteString("." + tok)
		}
		prev = tok
		depth++
	}
	return b.String()
}
