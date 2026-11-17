package store

import (
	"context"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// Route is a scope's routing variable as the pool last wrote it (§9.4): its
// name and value, the value empty once the pool deleted it. A value GitHub
// holds that differs from it was set by hand.
type Route struct {
	WrittenAt time.Time `json:"written_at"`
	Scope     string    `json:"scope"`
	Variable  string    `json:"variable"`
	Value     string    `json:"value"`
}

// Routes answers every scope's route record, ordered by scope.
func (s *Store) Routes(ctx context.Context) ([]Route, error) {
	var out []Route
	err := s.db.View(ctx, func(r *bolttable.ReadTx) error {
		return routes.Scan(r, bolttable.Range{}, func(_ []byte, v *Route) (bool, error) {
			out = append(out, *v)
			return true, nil
		})
	})
	return out, err
}

// RecordRoute writes rt, replacing the record of its scope, compared
// without case as GitHub compares owner and repository names.
func (s *Store) RecordRoute(ctx context.Context, rt *Route) error {
	return s.db.Update(ctx, func(tx *bolttable.Tx) error {
		return routes.Put(tx, []byte(strings.ToLower(rt.Scope)), rt)
	})
}
