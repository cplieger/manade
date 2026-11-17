package store

import (
	"slices"
	"time"

	"github.com/cplieger/bolttable"
)

// The host events' kinds (§21 b5-8).
const (
	// HostEventState is a change of the host's state as the pool serves
	// it; a gap when State is empty.
	HostEventState = "state"
	// HostEventRestart is a new instance of the host's supervisor.
	HostEventRestart = "restart"
	// HostEventRulesRestored is the host loading its egress rules again,
	// Count times since the last event.
	HostEventRulesRestored = "rules_restored"
	// HostEventPostureRebuilt is a new policy hash.
	HostEventPostureRebuilt = "posture_rebuilt"
	// HostEventImagePulled is a new image in service for Profile.
	HostEventImagePulled = "image_pulled"
	// HostEventProof is a change of the proof's verdict, Outcome.
	HostEventProof = "proof"
)

// The proof outcomes a probe and a proof event record.
const (
	ProofPassed = "passed"
	ProofFailed = "failed"
)

// HostEvent is one host transition. Detail is a state's detail, a failed
// proof's check or a gap's cause.
type HostEvent struct {
	At       time.Time `json:"at"`
	Host     string    `json:"host"`
	Kind     string    `json:"kind"`
	State    string    `json:"state,omitempty"`
	Detail   string    `json:"detail,omitempty"`
	Outcome  string    `json:"outcome,omitempty"`
	Instance string    `json:"instance,omitempty"`
	Profile  string    `json:"profile,omitempty"`
	Image    string    `json:"image,omitempty"`
	Digest   string    `json:"digest,omitempty"`
	Count    uint64    `json:"count,omitempty"`
}

// Probe is one completed proof of a host: At is when it ended, Failure the
// verdict's reason, empty when it passed, and Dropped the checks past the
// ones kept.
type Probe struct {
	At       time.Time    `json:"at"`
	Host     string       `json:"host"`
	Instance string       `json:"instance,omitempty"`
	Policy   string       `json:"policy,omitempty"`
	Unproven string       `json:"unproven,omitempty"`
	Failure  string       `json:"failure,omitempty"`
	Checks   []ProbeCheck `json:"checks,omitempty"`
	Dropped  int          `json:"dropped,omitempty"`
	Proven   bool         `json:"proven"`
}

// ProbeCheck is one check of a probe.
type ProbeCheck struct {
	Name       string `json:"name"`
	Leg        string `json:"leg"`
	Target     string `json:"target"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Pass       bool   `json:"pass"`
	Required   bool   `json:"required"`
	Unproven   bool   `json:"unproven,omitempty"`
}

// HostSeen is what the history last recorded of a host, which each new
// observation is compared with. Instance is empty until a status was
// recorded; Digests holds the digest in service by profile; Proof is the
// last verdict, empty before the first.
type HostSeen struct {
	Digests      map[string]string `json:"digests,omitempty"`
	Instance     string            `json:"instance,omitempty"`
	Policy       string            `json:"policy,omitempty"`
	State        string            `json:"state,omitempty"`
	Detail       string            `json:"detail,omitempty"`
	Proof        string            `json:"proof,omitempty"`
	ProofFailure string            `json:"proof_failure,omitempty"`
	Restored     uint64            `json:"restored,omitempty"`
}

// PutHostEvent appends e to the host events.
func (h *HistoryTx) PutHostEvent(e *HostEvent) error {
	id, err := hostEvents.NextID(h.tx)
	if err != nil {
		return err
	}
	return hostEvents.Put(h.tx, id, e)
}

// HostEvents visits host's events at or after from, oldest first, until fn
// answers false.
func (h *HistoryTx) HostEvents(host string, from time.Time, fn func(*HostEvent) (bool, error)) error {
	prefix := append([]byte(host), 0)
	rg := bolttable.Range{Prefix: prefix, From: slices.Concat(prefix, ageBytes(from))}
	return hostEventsByHost.Scan(h.tx.Read(), rg, func(_ []byte, id uint64, _ []byte) (bool, error) {
		e, found, err := hostEvents.Get(h.tx.Read(), id)
		if err != nil || !found {
			return err == nil, err
		}
		return fn(&e)
	})
}

// DeleteHostEventsBefore deletes every host event before cutoff.
func (h *HistoryTx) DeleteHostEventsBefore(cutoff time.Time) error {
	return deleteBefore(h.tx, hostEvents, hostEventsByAge, cutoff)
}

// PutProbe appends p to the probes.
func (h *HistoryTx) PutProbe(p *Probe) error {
	id, err := probes.NextID(h.tx)
	if err != nil {
		return err
	}
	return probes.Put(h.tx, id, p)
}

// Probes visits host's probes, newest first, until fn answers false.
func (h *HistoryTx) Probes(host string, fn func(*Probe) (bool, error)) error {
	rg := bolttable.Range{Prefix: append([]byte(host), 0), Reverse: true}
	return probesByHost.Scan(h.tx.Read(), rg, func(_ []byte, id uint64, _ []byte) (bool, error) {
		p, found, err := probes.Get(h.tx.Read(), id)
		if err != nil || !found {
			return err == nil, err
		}
		return fn(&p)
	})
}

// DeleteProbesBefore deletes every probe that ended before cutoff.
func (h *HistoryTx) DeleteProbesBefore(cutoff time.Time) error {
	return deleteBefore(h.tx, probes, probesByAge, cutoff)
}

// HostSeen answers what the history last recorded of host.
func (h *HistoryTx) HostSeen(host string) (HostSeen, bool, error) {
	return hostSeen.Get(h.tx.Read(), []byte(host))
}

// PutHostSeen records s as what the history last recorded of host.
func (h *HistoryTx) PutHostSeen(host string, s *HostSeen) error {
	return hostSeen.Put(h.tx, []byte(host), s)
}

// HostsSeen visits the name of every host the history recorded until fn
// answers false.
func (h *HistoryTx) HostsSeen(fn func(host string) (bool, error)) error {
	return hostSeen.Scan(h.tx.Read(), bolttable.Range{}, func(k []byte, _ *HostSeen) (bool, error) { return fn(string(k)) })
}

// DeleteHostSeen forgets what the history recorded of host.
func (h *HistoryTx) DeleteHostSeen(host string) error {
	_, err := hostSeen.Delete(h.tx, []byte(host))
	return err
}

// deleteBefore deletes every row of t whose byAge entry is before cutoff.
func deleteBefore[V any](tx *bolttable.Tx, t *bolttable.Table[uint64, V], byAge *bolttable.Index[uint64, V], cutoff time.Time) error {
	var ids []uint64
	err := byAge.Scan(tx.Read(), bolttable.Range{To: ageBytes(cutoff)}, func(_ []byte, id uint64, _ []byte) (bool, error) {
		ids = append(ids, id)
		return true, nil
	})
	for _, id := range ids {
		if err != nil {
			break
		}
		_, err = t.Delete(tx, id)
	}
	return err
}

// timeIndexes declares a host-and-time and a time index on t, each row
// keyed by at.
func timeIndexes[V any](t *bolttable.Table[uint64, V], name string, host func(*V) string, at func(*V) time.Time) (byHost, byAge *bolttable.Index[uint64, V]) {
	byHost = bolttable.Must(bolttable.DefineIndex(t, name+"_by_host", bolttable.IndexOptions{Version: 1},
		func(_ uint64, v *V) []bolttable.Entry {
			return []bolttable.Entry{{Key: append(append([]byte(host(v)), 0), ageBytes(at(v))...)}}
		}))
	byAge = bolttable.Must(bolttable.DefineIndex(t, name+"_by_age", bolttable.IndexOptions{Version: 1},
		func(_ uint64, v *V) []bolttable.Entry { return []bolttable.Entry{{Key: ageBytes(at(v))}} }))
	return byHost, byAge
}
