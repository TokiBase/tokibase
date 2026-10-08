//go:build !no_sync

package client

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Loop tunables (docs/SYNC_DESIGN.md §6.2).
const (
	EnvInterval     = "TOKI_SYNC_INTERVAL"
	EnvPoke         = "TOKI_SYNC_POKE"
	DefaultInterval = 30 * time.Second
	SlowInterval    = 5 * time.Minute
	DefaultDebounce = 2 * time.Second
	DefaultPage     = 500
	MeteredPage     = 100
	BackoffMin      = time.Second
	BackoffMax      = 5 * time.Minute
	jitter          = 0.2
)

type syncReq struct{ res chan Result }

// loopState is the state of the loop inside Client.
type loopState struct {
	mu       stdsync.Mutex
	running  bool
	cancel   context.CancelFunc
	done     chan struct{}
	paused   bool
	cond     Conditions
	failures int
	next     time.Time
	state    string
	lastErr  string
	lastOK   time.Time
	applyErr int64
	// hashMis counts hash_mismatch occurrences, hashStreak those in a row
	hashMis    int64
	hashStreak int
	mismatch   []string

	// booting is set while a snapshot bootstrap runs (the handshake then does not
	// stop the session with ErrRebootstrap); snapPage is the halved page size.
	booting  bool
	snapPage int
	// digestAt is the time of the last auto-heal digest check, digestStreak the
	// number of mismatching checks in a row.
	digestAt     time.Time
	digestStreak int
	// heal is StatusHealExhausted once the auto-heal limit was reached.
	heal string

	kick     chan struct{}
	reqs     chan syncReq
	events   chan Event
	wmu      stdsync.Mutex
	wTimer   bool
	pushFrom int64
	needHS   bool
	page     int
}

func (c *Client) initLoop() {
	c.loop.kick = make(chan struct{}, 1)
	c.loop.reqs = make(chan syncReq, 16)
	c.loop.events = make(chan Event, 256)
	c.loop.cond = Conditions{Online: true}
	c.loop.state = "idle"
	c.loop.needHS = true
}

// Backoff is the retry delay after the n-th consecutive failure (n >= 1):
// 1 s doubling up to 5 min, with ±20% jitter (rnd in [0,1)). A Retry-After
// from the hub is a lower bound.
func Backoff(n int, rnd float64, retryAfter time.Duration) time.Duration {
	d := BackoffMin
	for i := 1; i < n && d < BackoffMax; i++ {
		d *= 2
	}
	d = min(d, BackoffMax)
	d = time.Duration(float64(d) * (1 - jitter + 2*jitter*rnd))
	return max(d, retryAfter)
}

func (c *Client) rnd() float64 {
	if c.o.Rand != nil {
		return c.o.Rand()
	}
	return rand.Float64()
}

func (c *Client) interval() time.Duration {
	c.loop.mu.Lock()
	cond := c.loop.cond
	c.loop.mu.Unlock()
	if cond.LowPower || cond.Metered {
		return SlowInterval
	}
	if c.o.Interval > 0 {
		return c.o.Interval
	}
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(EnvInterval))); err == nil && d > 0 {
		return d
	}
	return DefaultInterval
}

func (c *Client) pageSize() int {
	c.loop.mu.Lock()
	defer c.loop.mu.Unlock()
	n := c.o.Page
	if n <= 0 {
		n = DefaultPage
	}
	if c.loop.page > 0 && c.loop.page < n {
		n = c.loop.page // halved after a 413
	}
	return n
}

func (c *Client) pullLimit() int {
	c.loop.mu.Lock()
	metered := c.loop.cond.Metered
	c.loop.mu.Unlock()
	n := c.pageSize()
	if metered {
		n = min(n, MeteredPage)
	}
	return n
}

func (c *Client) emit(ev Event) {
	ev.Time = time.Now()
	select {
	case c.loop.events <- ev:
	default: // slow reader: drop
	}
}

// Events returns the event channel (buffered; events are dropped when full).
func (c *Client) Events() <-chan Event { return c.loop.events }

// Start runs the loop until ctx is done or Stop is called. It returns at once.
func (c *Client) Start(ctx context.Context) {
	c.loop.mu.Lock()
	if c.loop.running {
		c.loop.mu.Unlock()
		return
	}
	lctx, cancel := context.WithCancel(ctx)
	c.loop.running, c.loop.cancel, c.loop.done = true, cancel, make(chan struct{})
	done := c.loop.done
	c.loop.mu.Unlock()
	go func() {
		defer func() {
			c.loop.mu.Lock()
			c.loop.running = false
			c.loop.mu.Unlock()
			close(done)
		}()
		c.run(lctx)
	}()
	if !c.o.NoPoke && strings.TrimSpace(os.Getenv(EnvPoke)) != "0" {
		go c.pokeLoop(lctx)
	}
}

// Stop ends the loop and waits for the current page to finish (or ctx).
func (c *Client) Stop(ctx context.Context) error {
	c.loop.mu.Lock()
	cancel, done, running := c.loop.cancel, c.loop.done, c.loop.running
	c.loop.mu.Unlock()
	if !running {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pause stops the attempts until Resume.
func (c *Client) Pause() {
	c.loop.mu.Lock()
	c.loop.paused = true
	c.loop.mu.Unlock()
	c.setCursorState(StatePaused, StateIdle)
}

// setCursorState moves `_sync_cursors.state` from one state to another (best effort).
func (c *Client) setCursorState(to, from string) {
	if c.o.App == nil {
		return
	}
	_, _ = c.o.App.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET state={:t} WHERE state={:f}").
		Bind(dbx.Params{"t": to, "f": from}).Execute()
}

// Resume undoes Pause and triggers a cycle.
func (c *Client) Resume() {
	c.loop.mu.Lock()
	c.loop.paused = false
	c.loop.mu.Unlock()
	c.setCursorState(StateIdle, StatePaused)
	c.Kick()
}

// SetConditions updates the device conditions. Going online triggers a cycle.
func (c *Client) SetConditions(cond Conditions) {
	c.loop.mu.Lock()
	was := c.loop.cond.Online
	c.loop.cond = cond
	c.loop.mu.Unlock()
	if cond.Online && !was {
		c.Kick()
	}
}

// Kick asks for a cycle (poke, local write). It is ignored while the loop is
// backing off after failures; SyncNow is not.
func (c *Client) Kick() {
	select {
	case c.loop.kick <- struct{}{}:
	default:
	}
}

// NotifyWrite is called after a local write: it kicks the loop once after the
// debounce interval.
func (c *Client) NotifyWrite() {
	c.loop.wmu.Lock()
	if c.loop.wTimer {
		c.loop.wmu.Unlock()
		return
	}
	c.loop.wTimer = true
	c.loop.wmu.Unlock()
	d := c.o.Debounce
	if d <= 0 {
		d = DefaultDebounce
	}
	time.AfterFunc(d, func() {
		c.loop.wmu.Lock()
		c.loop.wTimer = false
		c.loop.wmu.Unlock()
		c.Kick()
	})
}

// SyncNow runs a cycle as soon as possible (also while backing off) and
// delivers its result. While offline or paused the result carries ErrOffline
// or ErrPaused. When the loop is not running the channel gets ErrStopped.
func (c *Client) SyncNow() <-chan Result {
	out := make(chan Result, 1)
	c.loop.mu.Lock()
	running := c.loop.running
	c.loop.mu.Unlock()
	if !running {
		out <- Result{Err: ErrStopped}
		return out
	}
	select {
	case c.loop.reqs <- syncReq{res: out}:
	default:
		out <- Result{Err: errors.New("sync: too many pending SyncNow requests")}
	}
	return out
}

// Status returns a snapshot.
func (c *Client) Status() Status {
	c.loop.mu.Lock()
	st := Status{
		State: c.loop.state, Online: c.loop.cond.Online, Paused: c.loop.paused, Running: c.loop.running,
		LastOK: c.loop.lastOK, LastError: c.loop.lastErr, Failures: c.loop.failures, NextAttempt: c.loop.next,
		ApplyErrors: c.loop.applyErr, HashMismatches: c.loop.hashMis, HashStreak: c.loop.hashStreak, DigestMismatch: append([]string(nil), c.loop.mismatch...), Heal: c.loop.heal,
	}
	c.loop.mu.Unlock()
	st.OffsetMs = c.Offset().Milliseconds()
	if c.o.App != nil {
		_ = c.o.App.DB().NewQuery("SELECT COUNT(*) FROM _changes WHERE node={:n} AND status IN ('local','pushed')").
			Bind(dbx.Params{"n": c.nodeID}).Row(&st.Pending)
		if cur, _ := LoadCursor(c.o.App); cur != nil {
			st.PullAfter, st.AckedOrigin = cur.PullAfter, cur.AckedOrigin
		}
	}
	return st
}

func (c *Client) setState(s string) {
	c.loop.mu.Lock()
	c.loop.state = s
	c.loop.mu.Unlock()
}

// run is the loop goroutine.
func (c *Client) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		var reqs []syncReq
		explicit := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.loop.kick:
		case r := <-c.loop.reqs:
			reqs, explicit = append(reqs, r), true
		}
	drain:
		for {
			select {
			case r := <-c.loop.reqs:
				reqs, explicit = append(reqs, r), true
			default:
				break drain
			}
		}

		c.loop.mu.Lock()
		paused, online := c.loop.paused, c.loop.cond.Online
		backoffUntil := c.loop.next
		c.loop.mu.Unlock()
		deliver := func(r Result) {
			for _, q := range reqs {
				q.res <- r
			}
		}
		switch {
		case paused:
			deliver(Result{Err: ErrPaused})
			resetTimer(timer, c.interval())
			continue
		case !online:
			deliver(Result{Err: ErrOffline})
			resetTimer(timer, c.interval())
			continue
		case !explicit && time.Now().Before(backoffUntil):
			resetTimer(timer, time.Until(backoffUntil))
			continue
		}

		res := c.cycleBoot(ctx)
		deliver(res)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(res.Err, ErrRevoked) || errors.Is(res.Err, ErrRebootstrap) {
			return // nothing a retry can fix: the status says why (revoked / rebootstrap_required)
		}
		var wait time.Duration
		c.loop.mu.Lock()
		if res.Err != nil {
			c.loop.failures++
			var ra time.Duration
			var he *Error
			if errors.As(res.Err, &he) {
				ra = he.RetryAfter
			}
			wait = Backoff(c.loop.failures, c.rnd(), ra)
			c.loop.next = time.Now().Add(wait)
			c.loop.lastErr = res.Err.Error()
		} else {
			c.loop.failures, c.loop.next, c.loop.lastErr = 0, time.Time{}, ""
			c.loop.lastOK = time.Now()
		}
		c.loop.mu.Unlock()
		if res.Err == nil {
			wait = c.interval()
		}
		resetTimer(timer, wait)
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// cycle is one full sync: session -> push -> pull -> ack.
func (c *Client) cycle(ctx context.Context) (res Result) {
	defer func() {
		if res.Err != nil {
			c.emit(Event{Type: EventError, Message: res.Err.Error()})
			c.recordError(res.Err)
			var he *Error
			if errors.As(res.Err, &he) && he.Code == proto.CodeNodeRevoked {
				res.Err = ErrRevoked
				c.setState("revoked")
				c.emit(Event{Type: EventRevoked})
			}
		} else {
			c.emit(Event{Type: EventSynced})
			c.setState("idle")
		}
	}()
	if c.bootstrapPending() {
		res.Err = ErrRebootstrap
		return
	}
	c.setState("syncing")
	if err := c.ensureSession(ctx); err != nil {
		res.Err = err
		return
	}
	c.reservePass(ctx) // PR8: top up the reserved ranges (never fails the cycle)
	if err := c.pushAll(ctx, &res); err != nil {
		res.Err = err
		return
	}
	if err := c.pullAll(ctx, &res); err != nil {
		res.Err = err
		return
	}
	return
}

// RunOnce runs one full cycle (session, push, pull, ack) in the caller's
// goroutine, without the loop. It must not run concurrently with a started
// loop.
func (c *Client) RunOnce(ctx context.Context) Result { return c.cycle(ctx) }

// PullOnce runs only the session and the pull side of a cycle.
func (c *Client) PullOnce(ctx context.Context) Result {
	var res Result
	if c.bootstrapPending() {
		res.Err = ErrRebootstrap
		return res
	}
	if err := c.ensureSession(ctx); err != nil {
		res.Err = err
		return res
	}
	res.Err = c.pullAll(ctx, &res)
	return res
}

// bootstrapPending reports whether the cursor says a bootstrap is in progress or
// required while none runs in this process: a plain cycle must not push or pull
// then (log entries would land on half-emptied collections and the cursor would
// move past them, P7-10). The caller bootstraps (Bootstrap) or gives up.
func (c *Client) bootstrapPending() bool {
	if c.o.App == nil || c.isBootstrapping() {
		return false
	}
	cur, err := LoadCursor(c.o.App)
	return err == nil && cur != nil && (cur.State == StateBootstrapping || cur.State == StateRebootstrapRequired)
}

// cycleBoot is one loop iteration: a cycle, preceded by the snapshot bootstrap
// when one is in progress (crash, lost connection) or requested, and followed by
// one when the hub, a 410 or the auto-heal asks for it.
func (c *Client) cycleBoot(ctx context.Context) Result {
	if c.o.NoAutoBootstrap || c.o.Backend == nil || c.o.App == nil {
		return c.cycle(ctx)
	}
	if cur, _ := LoadCursor(c.o.App); cur != nil && (cur.State == StateBootstrapping || cur.State == StateRebootstrapRequired) {
		if err := c.Bootstrap(ctx); err != nil {
			c.recordError(err)
			return Result{Err: err}
		}
	}
	res := c.cycle(ctx)
	for i := 0; i < 2 && errors.Is(res.Err, ErrRebootstrap) && ctx.Err() == nil; i++ {
		if err := c.Bootstrap(ctx); err != nil {
			c.recordError(err)
			res.Err = err
			return res
		}
		res = c.cycle(ctx)
	}
	if errors.Is(res.Err, ErrRebootstrap) {
		res.Err = errors.New("sync: the hub still requires a re-bootstrap right after a snapshot")
	}
	if res.Err == nil && ctx.Err() == nil && c.healDue(ctx) {
		c.Kick() // the next cycle starts with the bootstrap
	}
	return res
}

// healDue implements TOKI_SYNC_AUTO_HEAL: two hash mismatches in a row, or two
// digest checks in a row that the hub reports as different, schedule a
// re-bootstrap. The digest check runs at most every DigestInterval and only
// without pending local changes (they would differ from the hub by definition).
func (c *Client) healDue(ctx context.Context) bool {
	if !c.autoHeal() {
		return false
	}
	c.loop.mu.Lock()
	hash := c.loop.hashStreak
	c.loop.mu.Unlock()
	if hash >= 2 {
		return c.scheduleHeal("auto-heal: hash mismatch twice in a row")
	}
	db, ok := c.o.Backend.(DigestBackend)
	if !ok {
		return false
	}
	interval := c.o.DigestInterval
	if interval <= 0 {
		if d, ok := parseDur(os.Getenv(EnvDigestInterval)); ok {
			interval = d
		} else {
			interval = DefaultDigestInterval
		}
	}
	c.loop.mu.Lock()
	due := time.Since(c.loop.digestAt) >= interval
	c.loop.mu.Unlock()
	if !due || c.Status().Pending > 0 {
		return false
	}
	c.loop.mu.Lock()
	c.loop.digestAt = time.Now()
	c.loop.mu.Unlock()
	digests, err := db.MetaDigests()
	cur, _ := LoadCursor(c.o.App)
	if err != nil || cur == nil {
		return false
	}
	ar, err := c.Ack(ctx, cur.PullAfter, digests)
	if err != nil || !ar.DigestChecked {
		return false // the hub did not compare (the node was behind): nothing learned
	}
	c.loop.mu.Lock()
	if len(ar.DigestMismatch) == 0 {
		c.loop.digestStreak = 0
		c.loop.mismatch = nil
		c.loop.mu.Unlock()
		return false
	}
	c.loop.digestStreak++
	c.loop.mismatch = ar.DigestMismatch
	streak := c.loop.digestStreak
	c.loop.mu.Unlock()
	c.emit(Event{Type: EventDigestMismatch, Message: strings.Join(ar.DigestMismatch, ",")})
	if streak >= 2 {
		return c.scheduleHeal("auto-heal: digest mismatch twice in a row (" + strings.Join(ar.DigestMismatch, ",") + ")")
	}
	return false
}

// Auto-heal limit: a mismatch that a fresh snapshot does not fix (data the hub
// holds outside the sync metadata, a field this node lacks) would otherwise
// re-download everything every few minutes for ever.
const (
	maxHealsPerWindow = 2
	healWindow        = 24 * time.Hour
	stateKeyHeals     = "heal_log"

	// StatusHealExhausted is Status.Heal when the limit stopped the auto-heal.
	StatusHealExhausted = "heal_exhausted"
)

// healBudget returns the unix times of the heals in the window and whether
// another one is allowed.
func (c *Client) healBudget() ([]int64, bool) {
	var all, kept []int64
	if v, ok := stateGet(c.o.App.DB(), stateKeyHeals); ok {
		_ = json.Unmarshal([]byte(v), &all)
	}
	cut := c.wallNow().Add(-healWindow).Unix()
	for _, t := range all {
		if t > cut {
			kept = append(kept, t)
		}
	}
	return kept, len(kept) < maxHealsPerWindow
}

func (c *Client) scheduleHeal(reason string) bool {
	heals, ok := c.healBudget()
	if !ok {
		msg := "auto-heal stopped (" + StatusHealExhausted + "): " + strconv.Itoa(len(heals)) + " heals in 24 h did not cure \"" + reason + "\"; fix the cause or run `toki sync rebootstrap`"
		c.loop.mu.Lock()
		first := c.loop.heal != StatusHealExhausted
		c.loop.heal = StatusHealExhausted
		c.loop.hashStreak, c.loop.digestStreak = 0, 0
		c.loop.mu.Unlock()
		if first {
			c.recordError(errors.New(msg))
			if c.o.Logger != nil {
				c.o.Logger.Error("sync: " + msg)
			}
			c.emit(Event{Type: EventError, Message: msg})
		}
		return false
	}
	if err := ScheduleRebootstrap(c.o.App, reason); err != nil {
		return false
	}
	_ = stateSet(c.o.App.NonconcurrentDB(), stateKeyHeals, func() string {
		b, _ := json.Marshal(append(heals, c.wallNow().Unix()))
		return string(b)
	}())
	c.loop.mu.Lock()
	c.loop.heal = ""
	c.loop.hashStreak, c.loop.digestStreak = 0, 0
	c.loop.mu.Unlock()
	if c.o.Logger != nil {
		c.o.Logger.Warn("sync: " + reason + "; re-bootstrapping")
	}
	return true
}
