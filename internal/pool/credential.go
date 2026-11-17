package pool

import (
	"context"
	"errors"
	"time"

	"github.com/cplieger/manade/internal/config"
	"github.com/cplieger/manade/internal/github"
)

// errNoCredential answers every GitHub call while the settings hold no
// credential.
var errNoCredential = errors.New("pool: no GitHub credential is set")

// noCredential is the GitHub side of a revision with no credential: every
// call refuses, and the sweep reads nothing.
var noCredential = &GitHub{ScaleSets: refusing{}, REST: refusing{}, Sweep: refusing{}}

type refusing struct{}

func (refusing) Ensure(context.Context, *config.Config, *config.Scope) ([]github.ScaleSet, error) {
	return nil, errNoCredential
}

func (refusing) Listen(ctx context.Context, _ *config.Scope, _ github.ScaleSet, _ string, _ github.Handler) {
	<-ctx.Done()
}

func (refusing) JIT(context.Context, *config.Scope, github.ScaleSet, string) (github.JIT, error) {
	return github.JIT{}, errNoCredential
}

func (refusing) RemoveRunner(context.Context, *config.Scope, int64) error { return errNoCredential }
func (refusing) Recheck(context.Context) error                            { return nil }

func (refusing) Exists(context.Context, *config.Scope, github.ScaleSet) (bool, error) {
	return false, errNoCredential
}

func (refusing) Private(context.Context, *config.Scope) (bool, error) { return false, errNoCredential }

func (refusing) Variable(context.Context, *config.Scope, string) (value string, found bool, err error) {
	return "", false, errNoCredential
}

func (refusing) SetVariable(context.Context, *config.Scope, string, string) error {
	return errNoCredential
}

func (refusing) DeleteVariable(context.Context, *config.Scope, string) error { return errNoCredential }

func (refusing) Runner(context.Context, *config.Scope, int64) (github.Runner, bool, error) {
	return github.Runner{}, false, errNoCredential
}

func (refusing) Run(ctx context.Context, _ *config.Config, _ func() bool, _ func(time.Time, []github.Queue)) {
	<-ctx.Done()
}
