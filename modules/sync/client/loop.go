//go:build !no_sync

package client

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
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
}

// Resume undoes Pause and triggers a cycle.
func (c *Client) Resume() {
	c.loop.mu.Lock()
	c.loop.paused = false
	c.loop.mu.Unlock()
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
		ApplyErrors: c.loop.applyErr, HashMismatches: c.loop.hashMis, HashStreak: c.loop.hashStreak, DigestMismatch: append([]string(nil), c.loop.mismatch...),
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

		res := c.cycle(ctx)
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
	c.setState("syncing")
	if err := c.ensureSession(ctx); err != nil {
		res.Err = err
		return
	}
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
	if err := c.ensureSession(ctx); err != nil {
		res.Err = err
		return res
	}
	res.Err = c.pullAll(ctx, &res)
	return res
}
