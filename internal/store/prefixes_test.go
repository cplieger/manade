package store

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"
)

// prefixesOf answers the value of every constant prefixes.go declares.
func prefixesOf(t *testing.T) map[string]byte {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "prefixes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := &types.Info{Defs: map[*ast.Ident]types.Object{}}
	if _, err := (&types.Config{}).Check("store", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}
	out := map[string]byte{}
	for id, obj := range info.Defs {
		if c, ok := obj.(*types.Const); ok {
			v, _ := constant.Uint64Val(c.Val())
			out[id.Name] = byte(v)
		}
	}
	return out
}

// Every table records its changes under a prefix of prefixes.go, and every
// prefix there names one table: the schema's recorded prefixes are exactly
// the file's, each once.
func TestSchemaPrefixesArePrefixesGo(t *testing.T) {
	consts := prefixesOf(t)
	var want []byte
	for _, v := range consts {
		want = append(want, v)
	}
	slices.Sort(want)
	if len(slices.Compact(slices.Clone(want))) != len(want) {
		t.Errorf("prefixes.go repeats a prefix: %v", consts)
	}
	if got := schema.Prefixes(); !slices.Equal(got, want) {
		t.Errorf("schema.Prefixes() = %x, want prefixes.go's %x", got, want)
	}
	for name, p := range map[string]byte{
		"prefixMeta": meta.Prefix(), "prefixScaleSets": scaleSets.Prefix(), "prefixRoutes": routes.Prefix(),
		"prefixJournal": journal.Prefix(), "prefixRuns": runs.Prefix(), "prefixBlobs": blobs.Prefix(),
		"prefixExits": exits.Prefix(), "prefixHostEvents": hostEvents.Prefix(), "prefixProbes": probes.Prefix(),
		"prefixHostSeen": hostSeen.Prefix(),
	} {
		if c, ok := consts[name]; !ok || c != p {
			t.Errorf("%s = %#x (declared %v), and its table's Prefix() = %#x", name, c, ok, p)
		}
	}
}
