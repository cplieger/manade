package store

import (
	"encoding/binary"
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

// tables is every table and index this build declares. meta holds one row
// per fact, each decoded by its owner, so a write of one fact leaves every
// other byte for byte; journal holds one row per change to this file's
// state, keyed in the order written.
type tables struct {
	meta         *bolttable.Table[[]byte, json.RawMessage]
	scaleSets    *bolttable.Table[[]byte, ScaleSet]
	routes       *bolttable.Table[[]byte, Route]
	journal      *bolttable.Table[uint64, Entry]
	journalCount *bolttable.Counter[uint64, Entry]
	runs         *bolttable.Table[uint64, Run]
	runsByRunner *bolttable.Index[uint64, Run]
	runsOpen     *bolttable.Index[uint64, Run]
	runsByAge    *bolttable.Index[uint64, Run]
	blobs        *bolttable.Table[uint64, Blob]
	exits        *bolttable.Table[[]byte, Exit]
	rollups      *bolttable.Table[[]byte, Rollup]
	cuts         *bolttable.Table[[]byte, Cut]
}

var schema, declared = declare(bolttable.NewSchema(schemaName))

var (
	meta, scaleSets, routes      = declared.meta, declared.scaleSets, declared.routes
	journal, journalCount        = declared.journal, declared.journalCount
	runs, runsByRunner, runsOpen = declared.runs, declared.runsByRunner, declared.runsOpen
	runsByAge, blobs, exits      = declared.runsByAge, declared.blobs, declared.exits
	rollups, cuts                = declared.rollups, declared.cuts
)

// declare declares every namespace of this build in s.
func declare(s *bolttable.Schema) (*bolttable.Schema, *tables) {
	var t tables
	t.meta = bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("meta")), prefixMeta, "meta",
		bolttable.BytesKey{}, bolttable.JSON[json.RawMessage](1), bolttable.TableOptions{}))
	gh := bolttable.Must(s.Namespace("github"))
	t.scaleSets = bolttable.Must(bolttable.Define(gh, prefixScaleSets, "scalesets",
		bolttable.BytesKey{}, bolttable.JSON[ScaleSet](1), bolttable.TableOptions{}))
	t.routes = bolttable.Must(bolttable.Define(gh, prefixRoutes, "routes",
		bolttable.BytesKey{}, bolttable.JSON[Route](1), bolttable.TableOptions{}))
	t.journal = bolttable.Must(bolttable.Define(bolttable.Must(s.Namespace("journal")), prefixJournal, "journal",
		bolttable.Uint64Key{}, bolttable.JSON[Entry](1), bolttable.TableOptions{}))
	t.journalCount = bolttable.Must(bolttable.DefineCounter(t.journal, "journal_rows", 1, func(uint64, *Entry) int64 { return 1 }))
	hist := bolttable.Must(s.Namespace("history"))
	t.runs = bolttable.Must(bolttable.Define(hist, prefixRuns, "runs",
		bolttable.Uint64Key{}, bolttable.JSON[Run](1), bolttable.TableOptions{}))
	t.runsByRunner, t.runsOpen, t.runsByAge = runIndexes(t.runs)
	t.blobs = bolttable.Must(bolttable.Define(hist, prefixBlobs, "runblobs",
		bolttable.Uint64Key{}, bolttable.JSON[Blob](1), bolttable.TableOptions{}))
	if err := bolttable.CascadePrefix(t.runs, t.blobs, 1, func(id uint64) []byte { return binary.BigEndian.AppendUint64(nil, id) }); err != nil {
		panic(err)
	}
	t.exits = bolttable.Must(bolttable.Define(hist, prefixExits, "exits",
		bolttable.BytesKey{}, bolttable.JSON[Exit](1), bolttable.TableOptions{}))
	ru := bolttable.Must(s.Namespace("rollups"))
	t.rollups = bolttable.Must(bolttable.Define(ru, prefixRollups, "rollups",
		bolttable.BytesKey{}, bolttable.JSON[Rollup](1), bolttable.TableOptions{}))
	t.cuts = bolttable.Must(bolttable.Define(ru, prefixCut, "cut",
		bolttable.BytesKey{}, bolttable.JSON[Cut](1), bolttable.TableOptions{}))
	return s, &t
}
