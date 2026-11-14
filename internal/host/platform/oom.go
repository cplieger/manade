package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	"github.com/cplieger/go/filesafe"
)

// oomFileLimit caps the saved total's file, a few dozen bytes.
const oomFileLimit = 4 << 10

// oomFile is the saved OOM kill total and the cgroup count it was read at.
type oomFile struct {
	Total uint64 `json:"total"`
	Count uint64 `json:"count"`
}

// OOMTotal is the host's OOM kill total across its own restarts: the total
// saved by the previous run plus this run's cgroup count, which a recreated
// cgroup starts at 0. Its zero value is not usable; LoadOOMTotal makes one.
type OOMTotal struct {
	path string
	// base is the saved total at this run's start; written the total last
	// saved.
	base    uint64
	written uint64
}

// LoadOOMTotal reads the total saved at path. It always answers a usable
// OOMTotal: on an error, which says why the saved total was not read, it
// counts from 0.
func LoadOOMTotal(ctx context.Context, path string) (*OOMTotal, error) {
	t := &OOMTotal{path: path}
	b, err := filesafe.ReadBounded(ctx, path, oomFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("the saved OOM kill total: %w", err)
	}
	var f oomFile
	if err := json.Unmarshal(b, &f); err != nil {
		return t, fmt.Errorf("the saved OOM kill total %s: %w", path, err)
	}
	t.base, t.written = f.Total, f.Total
	return t, nil
}

// Observe answers the total for this run's cgroup count, saving it
// atomically when it changed since the last save. On a failed save the
// total is still right and the next Observe saves again.
func (t *OOMTotal) Observe(ctx context.Context, count uint64) (uint64, error) {
	total := t.base + count
	if total == t.written {
		return total, nil
	}
	b, err := json.Marshal(oomFile{Total: total, Count: count})
	if err == nil {
		_, err = filesafe.WriteFile(ctx, t.path, b, filesafe.WithMode(0o600), filesafe.WithMkdirAll(true))
	}
	if err != nil {
		return total, fmt.Errorf("saving the OOM kill total: %w", err)
	}
	t.written = total
	return total, nil
}

// HostOOMScoreAdj is the host's own processes' oom_score_adj, so a
// host-wide memory kill takes a job, never the daemon or PID 1.
const HostOOMScoreAdj = -999

// SetOOMScoreAdj sets the oom_score_adj of process pid under r.
func SetOOMScoreAdj(r Root, pid, adj int) error {
	f, err := os.OpenFile(r.Path("/proc/"+strconv.Itoa(pid)+"/oom_score_adj"), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(adj))
	return errors.Join(err, f.Close())
}
