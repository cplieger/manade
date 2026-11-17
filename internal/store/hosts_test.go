package store

import (
	"slices"
	"testing"
	"time"
)

// A host's events read in time order from an instant, its probes newest
// first, and neither names another host's, even one whose name extends
// it; each leaves at its cutoff.
func TestHostEventsAndProbesByHost(t *testing.T) {
	s := mustOpen(t, testPath(t), "v1.0.0-dev.1")
	defer mustClose(t, s)
	update(t, s, func(h *HistoryTx) error {
		for i, host := range []string{"nas", "nas2", "nas", "nas"} {
			at := t0.Add(time.Duration(i) * time.Hour)
			if err := h.PutHostEvent(&HostEvent{At: at, Host: host, Kind: HostEventState, Detail: at.Format(time.Kitchen)}); err != nil {
				return err
			}
			if err := h.PutProbe(&Probe{At: at, Host: host, Failure: at.Format(time.Kitchen)}); err != nil {
				return err
			}
		}
		return nil
	})
	read := func(from time.Time) (evs, ps []string) {
		update(t, s, func(h *HistoryTx) error {
			if err := h.HostEvents("nas", from, func(e *HostEvent) (bool, error) {
				evs = append(evs, e.Detail)
				return true, nil
			}); err != nil {
				return err
			}
			return h.Probes("nas", func(p *Probe) (bool, error) {
				ps = append(ps, p.Failure)
				return true, nil
			})
		})
		return evs, ps
	}
	t.Run("eventsFromAnInstant", func(t *testing.T) {
		if evs, _ := read(t0.Add(time.Hour)); !slices.Equal(evs, []string{"2:00PM", "3:00PM"}) {
			t.Errorf("events = %v, want nas's from 1:00PM", evs)
		}
	})
	t.Run("probesNewestFirst", func(t *testing.T) {
		if _, ps := read(time.Time{}); !slices.Equal(ps, []string{"3:00PM", "2:00PM", "12:00PM"}) {
			t.Errorf("probes = %v, want nas's newest first", ps)
		}
	})
	t.Run("deleteBefore", func(t *testing.T) {
		update(t, s, func(h *HistoryTx) error {
			if err := h.DeleteHostEventsBefore(t0.Add(2 * time.Hour)); err != nil {
				return err
			}
			return h.DeleteProbesBefore(t0.Add(3 * time.Hour))
		})
		evs, ps := read(time.Time{})
		if !slices.Equal(evs, []string{"2:00PM", "3:00PM"}) || !slices.Equal(ps, []string{"3:00PM"}) {
			t.Errorf("events %v and probes %v, want those at or after their cutoffs", evs, ps)
		}
	})
}
