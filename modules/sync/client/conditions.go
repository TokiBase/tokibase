//go:build !no_sync

package client

import (
	"strconv"
	"time"
)

// Device conditions of the loop (docs/SYNC_DESIGN.md §6.2) and the scheduler
// seam that lets tests drive the loop with a fake clock.

// BackgroundBudget is the length of the one cycle an OS granted background slot
// (iOS BGAppRefreshTask, Android WorkManager) may use.
const BackgroundBudget = 20 * time.Second

// Plan is what the loop does under a set of conditions.
type Plan struct {
	// Attempt is false while the device is offline: no attempt at all.
	Attempt bool
	// Pull is false for an automatic cycle on a metered link (push only).
	Pull bool
	// PullLimit is the number of changes per pull page.
	PullLimit int
	// Interval is the idle interval between two automatic cycles.
	Interval time.Duration
	// Budget bounds the single cycle of a background slot (0: unbounded). After
	// it the loop stops until the conditions change or SyncNow is called.
	Budget time.Duration
}

// Plan derives the behavior from the conditions. base is the configured idle
// interval, page the configured page size, explicit is true for SyncNow (an
// explicit request also pulls on a metered link, with the capped page).
func (c Conditions) Plan(base time.Duration, page int, explicit bool) Plan {
	if base <= 0 {
		base = DefaultInterval
	}
	if page <= 0 {
		page = DefaultPage
	}
	p := Plan{Attempt: c.Online, Pull: true, PullLimit: page, Interval: base}
	if c.LowPower || c.Metered {
		p.Interval = max(base, SlowInterval)
	}
	if c.Metered {
		p.Pull = explicit
		p.PullLimit = min(page, MeteredPage)
	}
	if c.Background {
		p.Budget = BackgroundBudget
	}
	return p
}

// Conditions returns the current device conditions.
func (c *Client) Conditions() Conditions {
	c.loop.mu.Lock()
	defer c.loop.mu.Unlock()
	return c.loop.cond
}

// Scheduler is the clock of the loop: tests replace it to run hours of sync in
// milliseconds. The default uses the time package.
type Scheduler interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the part of *time.Timer the loop uses. C is nil for AfterFunc timers.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(d time.Duration) bool
}

type realSched struct{}

func (realSched) Now() time.Time { return time.Now() }
func (realSched) NewTimer(d time.Duration) Timer {
	return realTimer{time.NewTimer(d)}
}
func (realSched) AfterFunc(d time.Duration, f func()) Timer {
	return realTimer{time.AfterFunc(d, f)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

func (c Conditions) String() string {
	b := func(v bool) string { return strconv.FormatBool(v) }
	return "online=" + b(c.Online) + " metered=" + b(c.Metered) + " low_power=" + b(c.LowPower) + " background=" + b(c.Background)
}
