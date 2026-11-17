package store

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cplieger/bolttable"
)

// ScaleSet is the record of one scale set the pool ensured: its
// scope and profile, which name it, GitHub's ID for it and when the pool
// recorded that ID.
type ScaleSet struct {
	EnsuredAt time.Time `json:"ensured_at"`
	Scope     string    `json:"scope"`
	Profile   string    `json:"profile"`
	ID        int       `json:"id"`
}

// scaleSetKey names a record by its scope, without case as GitHub compares
// owner and repository names, and its profile.
func scaleSetKey(scope, profile string) []byte {
	return []byte(strings.ToLower(scope) + "\x00" + profile)
}

// ScaleSets answers every recorded scale set, ordered by scope and profile.
func (s *Store) ScaleSets(ctx context.Context) ([]ScaleSet, error) {
	var out []ScaleSet
	err := s.view(ctx, func(r *bolttable.ReadTx) error {
		return scaleSets.Scan(r, bolttable.Range{}, func(_ []byte, v *ScaleSet) (bool, error) {
			out = append(out, *v)
			return true, nil
		})
	})
	return out, err
}

// DropScaleSet drops the record of ss, a scale set GitHub no longer has,
// with its journal row in one transaction. A record an ensure has since
// moved to another ID, or one already gone, is kept and journals nothing.
func (s *Store) DropScaleSet(ctx context.Context, ss ScaleSet, at time.Time) error {
	key := scaleSetKey(ss.Scope, ss.Profile)
	return s.update(ctx, func(tx *bolttable.Tx) error {
		cur, found, err := scaleSets.Get(tx.Read(), key)
		if err != nil || !found || cur.ID != ss.ID {
			return err
		}
		if _, err := scaleSets.Delete(tx, key); err != nil {
			return err
		}
		return write(tx, &entry{
			At: at, Actor: actor{Kind: actorPool}, Action: actionScaleSetRecordDropped, Subject: ss.Scope, Outcome: outcomeDone,
			Facts: map[string]string{"profile": ss.Profile, "scale_set_id": strconv.Itoa(ss.ID)},
		})
	})
}

// RecordScaleSets writes every one of sets in one transaction, each
// replacing the record of the same scope and profile.
func (s *Store) RecordScaleSets(ctx context.Context, sets []ScaleSet) error {
	return s.update(ctx, func(tx *bolttable.Tx) error {
		for i := range sets {
			if err := scaleSets.Put(tx, scaleSetKey(sets[i].Scope, sets[i].Profile), &sets[i]); err != nil {
				return err
			}
		}
		return nil
	})
}
