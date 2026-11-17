package github

import (
	"context"
	"sync"
	"testing"
	"time"
)

// cutCtx is a bounded call's context that a test ends as its deadline
// would, with no wall-clock timer an unheld call on a loaded machine could
// also reach.
type cutCtx struct {
	context.Context
	done chan struct{}
	err  error
	mu   sync.Mutex
}

func (c *cutCtx) Done() <-chan struct{} { return c.done }

func (c *cutCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *cutCtx) end(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
		close(c.done)
	}
}

// cutter records every context a seam made and the bound each was asked for.
type cutter struct {
	ctxs   []*cutCtx
	bounds []time.Duration
	mu     sync.Mutex
}

// cutAt replaces *seam, restored when t ends, with a cutter's.
func cutAt(t *testing.T, seam *func(context.Context, time.Duration) (context.Context, context.CancelFunc)) *cutter {
	t.Helper()
	c := &cutter{}
	prev := *seam
	*seam = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx := &cutCtx{Context: parent, done: make(chan struct{})}
		stop := context.AfterFunc(parent, func() { ctx.end(parent.Err()) })
		c.mu.Lock()
		c.ctxs, c.bounds = append(c.ctxs, ctx), append(c.bounds, d)
		c.mu.Unlock()
		return ctx, func() { stop(); ctx.end(context.Canceled) }
	}
	t.Cleanup(func() { *seam = prev })
	return c
}

// last ends the newest context as its deadline would.
func (c *cutter) last() {
	c.mu.Lock()
	ctx := c.ctxs[len(c.ctxs)-1]
	c.mu.Unlock()
	ctx.end(context.DeadlineExceeded)
}

// asked answers the bounds the seam was asked for, oldest first.
func (c *cutter) asked() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.bounds...)
}
