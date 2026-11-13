package obs

import (
	"context"
	"strings"
	"time"

	"github.com/cplieger/go/measure"
)

// Source answers the pool's state. Concurrent scrapes and Pass call it, so it
// must be safe for concurrent use, and Pass's caller must not hold a lock
// the Source takes.
type Source func() Snapshot

// Snapshot is the pool's state at one instant.
type Snapshot struct {
	// HeadSince is the queue time of the one queue's head, zero with no head.
	HeadSince time.Time
	Counts    []Counts
	Reserved  Resources
}

// Resources is what live runners reserve.
type Resources struct {
	MilliCPUs int64
	Memory    int64
	Disk      int64
}

// Counts is one scope and profile's runners and jobs. Every job sits in at
// most one of Queued, Unassignable and the three unfundable counts.
// NotAllowed counts jobs for a profile the scope may not use, so its
// ScaleSet need not exist.
type Counts struct {
	ScaleSet
	Starting, Idle, Busy                 int
	Queued, AssignedUnstarted            int
	Unassignable                         int
	NotAllowed, NotServing, ImageMissing int
}

// SetSource installs the pool's state. Until it is set, the scrape-time
// families and the record's pool fields are empty.
func (m *Metrics) SetSource(src Source) { m.src.Store(&src) }

type descs struct {
	runners, queued, assigned, unassignable, unfundable  *measure.Desc
	reservedCPUs, reservedMemory, reservedDisk, headWait *measure.Desc
}

// collector declares the scrape-time families; reserved disk only beside
// a disk budget.
func (d *descs) collector(collect func(context.Context, *measure.Emitter), disk bool) *measure.Collector {
	scopeProfile := []string{scopeLabel, profileLabel}
	d.runners = measure.NewGaugeDesc("runners", "Runners now, by state.", []string{scopeLabel, profileLabel, "state"})
	d.queued = measure.NewGaugeDesc("jobs_queued",
		"Ranked jobs waiting in the one queue, less those assigned, unfundable or unassignable.", scopeProfile)
	d.assigned = measure.NewGaugeDesc("jobs_assigned_unstarted",
		"Jobs GitHub assigned to the scale set that no runner has taken yet.", scopeProfile)
	d.unassignable = measure.NewGaugeDesc("jobs_unassignable",
		"Queued jobs left out of the queue because GitHub left a funded slot unused for two polls.", scopeProfile)
	d.unfundable = measure.NewGaugeDesc("jobs_unfundable",
		"Queued jobs left out of the queue because the pool cannot run them now, by reason.",
		[]string{scopeLabel, profileLabel, "reason"})
	d.reservedCPUs = measure.NewGaugeDesc("reserved_cpus", "CPUs live runners reserve.", nil)
	d.reservedMemory = measure.NewGaugeDesc("reserved_memory_bytes", "Memory live runners reserve.", nil)
	d.headWait = measure.NewGaugeDesc("queue_head_wait_seconds",
		"How long the head of the one queue has waited; repo scopes only.", nil)
	all := []*measure.Desc{d.runners, d.queued, d.assigned, d.unassignable, d.unfundable, d.reservedCPUs, d.reservedMemory, d.headWait}
	if disk {
		d.reservedDisk = measure.NewGaugeDesc("reserved_disk_bytes", "Disk live runners reserve.", nil)
		all = append(all, d.reservedDisk)
	}
	return measure.NewCollector(collect, all...)
}

func (m *Metrics) collect(_ context.Context, e *measure.Emitter) {
	src := m.src.Load()
	if src == nil {
		return
	}
	snap := (*src)()
	d := &m.d
	e.Emit(d.reservedCPUs, float64(snap.Reserved.MilliCPUs)/1000)
	e.Emit(d.reservedMemory, float64(snap.Reserved.Memory))
	if d.reservedDisk != nil {
		e.Emit(d.reservedDisk, float64(snap.Reserved.Disk))
	}
	var wait float64
	if !snap.HeadSince.IsZero() {
		wait = max(0, unixSeconds(m.now())-unixSeconds(snap.HeadSince))
	}
	e.Emit(d.headWait, wait)
	for i := range snap.Counts {
		c := &snap.Counts[i]
		s, p := c.Scope, c.Profile
		e.Emit(d.runners, float64(c.Starting), s, p, "starting")
		e.Emit(d.runners, float64(c.Idle), s, p, "idle")
		e.Emit(d.runners, float64(c.Busy), s, p, "busy")
		e.Emit(d.queued, float64(c.Queued), s, p)
		e.Emit(d.assigned, float64(c.AssignedUnstarted), s, p)
		e.Emit(d.unassignable, float64(c.Unassignable), s, p)
		e.Emit(d.unfundable, float64(c.NotAllowed), s, p, "not_allowed")
		e.Emit(d.unfundable, float64(c.NotServing), s, p, "not_serving")
		e.Emit(d.unfundable, float64(c.ImageMissing), s, p, "image_missing")
	}
}

// passEvery passes make one record: about 2.5 minutes of the reconciler's
// 30-second loop, less when die events add passes. The LogQL bundle's
// 10-minute ranges rely on that gap.
const passEvery = 5

// Pass records one reconciler pass ending now, ok when it succeeded, and
// writes the pass record on every fifth. The record reduces the series the
// PromQL rules read, so each LogQL twin sees what its rule sees. Its message
// and field names are a contract with alerts/logql.yaml, bound by
// TestAlertParityLogQLReadsTheRecords.
func (m *Metrics) Pass(ctx context.Context, ok bool) {
	now := m.now()
	if ok {
		m.reconcile.Set(unixSeconds(now))
	}
	m.passMu.Lock()
	defer m.passMu.Unlock()
	m.passes++
	if m.passes%passEvery != 0 {
		return
	}
	// measure logs a dropped sample itself, and the families hold the rest.
	fams, _ := m.reg.Gather(ctx)
	t := reduce(fams, unixSeconds(now))
	m.log.Info("pass record",
		"poll_age_seconds", t.pollAge,
		"scopes_not_serving", t.notServing,
		"starts", t.starts-m.last.starts,
		"start_errors", t.startErrors-m.last.startErrors,
		"assigned_unstarted", t.assigned,
		"head_wait_seconds", t.headWait,
		"queue_read_age_seconds", t.readAge,
		"oom_exits", t.oom-m.last.oom,
		"reconcile_age_seconds", t.reconcileAge,
		"image_check_age_seconds", t.imageAge,
	)
	m.last = t
}

// bootEvery is about the pass records' gap when no die event adds a pass, so
// the LogQL deadman's 10-minute window reads one record or the other through
// a boot and after it.
const bootEvery = passEvery * 30 * time.Second

// Boot writes the start record at once, before any wait, then a boot record
// carrying boot_seconds every bootEvery until ctx ends; Booted writes the end
// line. The messages are a contract with alerts/logql.yaml, bound by
// TestAlertParityLogQLReadsTheRecords and TestLogQLBootRules.
func (m *Metrics) Boot(ctx context.Context) {
	m.log.Info("manade is starting")
	t := time.NewTicker(bootEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.log.Info("manade is still starting", "boot_seconds", m.bootSeconds())
		}
	}
}

// Booted records that boot has finished: manade_booting reads 0, and the end
// line carries the boot's length. Call it once Boot has returned, so no boot
// record follows the end line.
func (m *Metrics) Booted() {
	m.booting.Set(0)
	m.log.Info("manade finished starting", "boot_seconds", m.bootSeconds())
}

func (m *Metrics) bootSeconds() int64 { return int64(m.now().Sub(m.started).Seconds()) }

// totals is the record's reduction of the page: the oldest age of each
// freshness family, and the cumulative starts and OOM exits.
type totals struct {
	pollAge, notServing, assigned, headWait, readAge int64
	reconcileAge, imageAge                           int64
	starts, startErrors, oom                         int64
}

func reduce(fams []measure.Family, now float64) totals {
	var t totals
	age := func(ts float64) int64 { return int64(now - ts) }
	for _, f := range fams {
		for _, s := range f.Samples {
			v := s.Value
			switch strings.TrimPrefix(f.Name, "manade_") {
			case "poll_last_success_timestamp_seconds":
				t.pollAge = max(t.pollAge, age(v))
			case "queue_read_last_success_timestamp_seconds":
				t.readAge = max(t.readAge, age(v))
			case "image_checked_timestamp_seconds":
				t.imageAge = max(t.imageAge, age(v))
			case "reconcile_last_success_timestamp_seconds":
				t.reconcileAge = age(v)
			case "scope_serving":
				t.notServing += int64(1 - v)
			case "jobs_assigned_unstarted":
				t.assigned += int64(v)
			case "queue_head_wait_seconds":
				t.headWait = int64(v)
			case "runner_starts_total":
				if lastLabel(s) == "ok" {
					t.starts += int64(v)
				} else {
					t.startErrors += int64(v)
				}
			case "runner_exits_total":
				if lastLabel(s) == string(ExitOOM) {
					t.oom += int64(v)
				}
			}
		}
	}
	return t
}

func lastLabel(s measure.Sample) string { return s.Labels[len(s.Labels)-1].Value }
