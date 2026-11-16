package store

import (
	"encoding/json"

	"github.com/cplieger/bolttable"
)

// schemaName enters every namespace's digest.
const schemaName = "manade"

// format is the store format this build reads and writes. It changes only
// with a change no namespace copy can carry, and a build refuses a file of a
// higher format.
const format = 1

// stamp is who last opened the file. Every open rewrites it whole, so it
// holds the format and the build and nothing else.
type stamp struct {
	Build  string `json:"build,omitempty"`
	Format int    `json:"format"`
}

// stampKey is the stamp's key in meta. Every other fact meta keeps has a key
// of its own, which no open writes.
var stampKey = []byte("store")

// meta holds one row per fact, each decoded by the code that owns it, so a
// write of one fact leaves every other byte for byte.
var schema, meta = declare()

func declare() (*bolttable.Schema, *bolttable.Table[[]byte, json.RawMessage]) {
	s := bolttable.NewSchema(schemaName)
	m := bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("meta")), prefixMeta, "meta",
		bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	return s, m
}
