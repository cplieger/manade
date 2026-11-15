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

// metaRow records who last opened the file. Fields are only ever added.
type metaRow struct {
	Build  string `json:"build,omitempty"`
	Format int    `json:"format"`
}

var metaKey = []byte("store")

var schema, meta = declare()

// declare builds the whole schema. A table whose writer is not built yet is
// declared with a raw JSON value: its digest is the same as with its row
// type, which the writer's change gives it.
func declare() (*bolttable.Schema, *bolttable.Table[[]byte, metaRow]) {
	s := bolttable.NewSchema(schemaName)
	ns := func(name string) *bolttable.Namespace { return bolttable.Must(s.Namespace(name)) }
	byName, seq := bolttable.BytesKey{}, bolttable.Uint64Key{}

	m := bolttable.Must(bolttable.Define(ns("meta"), prefixMeta, "meta", byName, bolttable.JSON[metaRow](1), bolttable.TableOptions{}))
	raw(ns("secrets"), prefixSecrets, "values", byName)
	hosts := ns("hosts")
	raw(hosts, prefixHosts, "identities", byName)
	raw(hosts, prefixTombstones, "tombstones", byName)
	raw(ns("github"), prefixScaleSets, "scalesets", byName)
	raw(ns("journal"), prefixJournal, "entries", seq)
	history := ns("history")
	raw(history, prefixRuns, "runs", seq)
	raw(history, prefixRunBlobs, "runblobs", seq)
	raw(history, prefixHostEvents, "hostevents", seq)
	raw(history, prefixProbes, "probes", seq)
	raw(ns("rollups"), prefixRollups, "days", byName)
	return s, m
}

func raw[K any](n *bolttable.Namespace, prefix byte, name string, key bolttable.KeyCodec[K]) {
	bolttable.Must(bolttable.Define(n, prefix, name, key, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
}
