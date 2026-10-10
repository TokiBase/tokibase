//go:build !no_sync

package client

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/modules/sync/proto"
)

func TestPlan(t *testing.T) {
	const base = 30 * time.Second
	cases := []struct {
		name     string
		cond     Conditions
		explicit bool
		want     Plan
	}{
		{"online", Conditions{Online: true}, false, Plan{Attempt: true, Pull: true, PullLimit: 500, Interval: base}},
		{"offline", Conditions{}, false, Plan{Attempt: false, Pull: true, PullLimit: 500, Interval: base}},
		{"low power", Conditions{Online: true, LowPower: true}, false, Plan{Attempt: true, Pull: true, PullLimit: 500, Interval: SlowInterval}},
		{"metered", Conditions{Online: true, Metered: true}, false, Plan{Attempt: true, Pull: false, PullLimit: 100, Interval: SlowInterval}},
		{"metered syncnow", Conditions{Online: true, Metered: true}, true, Plan{Attempt: true, Pull: true, PullLimit: 100, Interval: SlowInterval}},
		{"background", Conditions{Online: true, Background: true}, false, Plan{Attempt: true, Pull: true, PullLimit: 500, Interval: base, Budget: 20 * time.Second}},
		{"all", Conditions{Online: true, Metered: true, LowPower: true, Background: true}, false, Plan{Attempt: true, Pull: false, PullLimit: 100, Interval: SlowInterval, Budget: 20 * time.Second}},
	}
	for _, c := range cases {
		if got := c.cond.Plan(base, 500, c.explicit); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	// a configured page below the cap stays; a long configured interval is kept
	if p := (Conditions{Online: true, Metered: true}).Plan(time.Hour, 50, true); p.PullLimit != 50 || p.Interval != time.Hour {
		t.Errorf("small page / long interval: %+v", p)
	}
}

func TestBackoffJitterBounds(t *testing.T) {
	for n, base := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: 5 * time.Minute, 40: 5 * time.Minute} {
		lo, hi := Backoff(n, 0, 0), Backoff(n, 0.999999, 0)
		if lo != time.Duration(float64(base)*0.8) || hi < time.Duration(float64(base)*1.19) || hi > time.Duration(float64(base)*1.2) {
			t.Errorf("n=%d: [%v, %v] for base %v", n, lo, hi, base)
		}
	}
	if d := Backoff(1, 0.5, 90*time.Second); d != 90*time.Second {
		t.Errorf("Retry-After is a lower bound, got %v", d)
	}
}

// countingRT answers every request with a hub error and counts them.
type countingRT struct{ n atomic.Int64 }

func (r *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.n.Add(1)
	return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"status":503,"message":"down","data":{"code":"unavailable"}}`)), Request: req}, nil
}

func loopFixture(t *testing.T, cond Conditions, mod ...func(*Options)) (*Client, *fakeSched, *countingRT) {
	t.Helper()
	return loopFixtureRT(t, cond, &countingRT{}, mod...)
}

func loopFixtureRT(t *testing.T, cond Conditions, rt http.RoundTripper, mod ...func(*Options)) (*Client, *fakeSched, *countingRT) {
	t.Helper()
	id, err := proto.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeSched()
	o := Options{
		Identity: id, HubURL: "http://127.0.0.1:1", Insecure: true, Cert: "cert", HubID: "hubtest",
		HubPub: base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)), HTTP: &http.Client{Transport: rt},
		Sched: fs, Now: fs.Now, NoPoke: true, Interval: 30 * time.Second, Debounce: 2 * time.Second,
		Rand: func() float64 { return 0.5 }, // jitter factor exactly 1
	}
	for _, m := range mod {
		m(&o)
	}
	c, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	fs.loopPasses = c.LoopPasses
	c.SetConditions(cond)
	crt, _ := rt.(*countingRT)
	return c, fs, crt
}

func startLoop(t *testing.T, c *Client, fs *fakeSched) {
	t.Helper()
	c.Start(context.Background())
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	fs.Advance(0) // the loop's first timer is due at once
	waitPasses(t, c, fs, 2)
}

// waitPasses blocks until the loop finished `want` wake-ups and has nothing
// queued: a condition wait, so an assertion that "nothing happened" is made
// only after the loop really looked at the stimulus.
func waitPasses(t *testing.T, c *Client, fs *fakeSched, want int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if c.LoopPasses() >= want && len(c.loop.kick) == 0 && len(c.loop.reqs) == 0 && !fs.pending() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop did not settle: passes %d (want %d), kick %d, reqs %d", c.LoopPasses(), want, len(c.loop.kick), len(c.loop.reqs))
		}
		time.Sleep(time.Millisecond)
	}
}

// eventually polls cond (a condition wait, not a fixed sleep).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestLoopOfflineMakesNoAttempts(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: false})
	startLoop(t, c, fs)
	fs.Advance(time.Hour)
	if n := rt.n.Load(); n != 0 {
		t.Fatalf("offline: %d requests", n)
	}
	if r := <-c.SyncNow(); !errors.Is(r.Err, ErrOffline) {
		t.Fatalf("SyncNow offline: %v", r.Err)
	}
	if rt.n.Load() != 0 {
		t.Fatal("SyncNow offline must not touch the network")
	}
	// back online: a cycle starts at once
	p := c.LoopPasses()
	c.SetConditions(Conditions{Online: true})
	waitPasses(t, c, fs, p+1)
	if n := rt.n.Load(); n == 0 {
		t.Fatal("going online must trigger a cycle")
	}
}

func TestLoopBackoffDoublesWithInjectedClock(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true})
	startLoop(t, c, fs)
	n0 := rt.n.Load() // first failure at t=0, next attempt after 1 s
	if n0 == 0 {
		t.Fatal("no first attempt")
	}
	per := n0 // requests per failed cycle
	fs.Advance(900 * time.Millisecond)
	if rt.n.Load() != n0 {
		t.Fatal("retried before the 1 s backoff")
	}
	fs.Advance(200 * time.Millisecond)
	if rt.n.Load() != 2*per {
		t.Fatalf("no retry after 1 s: %d requests", rt.n.Load())
	}
	fs.Advance(1800 * time.Millisecond) // second failure: 2 s (retry due at t=3.0 s)
	if rt.n.Load() != 2*per {
		t.Fatal("retried before the 2 s backoff")
	}
	fs.Advance(300 * time.Millisecond)
	if rt.n.Load() != 3*per {
		t.Fatalf("no retry after 2 s: %d requests", rt.n.Load())
	}
	st := c.Status()
	if st.Failures != 3 || st.LastError == "" || !st.NextAttempt.After(fs.Now()) {
		t.Fatalf("status %+v", st)
	}
	// SyncNow ignores the backoff
	before := rt.n.Load()
	<-c.SyncNow()
	if rt.n.Load() <= before {
		t.Fatal("SyncNow must run during a backoff")
	}
}

// Going back online breaks the backoff at once: the failures were earned under
// conditions the host now says are gone.
func TestLoopOnlineTransitionBreaksBackoff(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true})
	startLoop(t, c, fs)
	per := rt.n.Load()
	fs.Advance(1100 * time.Millisecond)
	fs.Advance(1500 * time.Millisecond) // 2 failures, the next retry is 2 s away
	if rt.n.Load() != 2*per || c.Status().Failures != 2 {
		t.Fatalf("setup: %d requests, %+v", rt.n.Load(), c.Status())
	}
	p := c.LoopPasses()
	c.SetConditions(Conditions{Online: false})
	c.SetConditions(Conditions{Online: true}) // no clock movement at all
	waitPasses(t, c, fs, p+1)
	if rt.n.Load() != 3*per {
		t.Fatalf("the online kick was swallowed by the backoff: %d requests", rt.n.Load())
	}
}

func TestLoopBackgroundRunsOneBoundedCycle(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true, Background: true})
	startLoop(t, c, fs)
	n0 := rt.n.Load()
	if n0 == 0 {
		t.Fatal("the background slot must run one cycle")
	}
	if !c.Status().BackgroundDone {
		t.Fatal("status must say the slot's cycle ran")
	}
	fs.Advance(30 * time.Minute)
	p := c.LoopPasses()
	c.Kick()
	waitPasses(t, c, fs, p+1)
	if rt.n.Load() != n0 {
		t.Fatalf("a background slot stops after its cycle (%d -> %d requests)", n0, rt.n.Load())
	}
	// SyncNow is explicit: it runs once more
	<-c.SyncNow()
	if rt.n.Load() <= n0 {
		t.Fatal("SyncNow in a background slot must run")
	}
	// a new slot gets a new cycle
	n1 := rt.n.Load()
	c.SetConditions(Conditions{Online: true})
	c.SetConditions(Conditions{Online: true, Background: true})
	eventually(t, "a new background slot runs a cycle", func() bool { return rt.n.Load() > n1 && c.Status().BackgroundDone })
}

// gateRT blocks the first request until release is closed and fails every
// request with a 503, so that a cycle can be held in the middle.
type gateRT struct {
	n       atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (g *gateRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if g.n.Add(1) == 1 {
		close(g.entered)
		select {
		case <-g.release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"status":503,"message":"down","data":{"code":"unavailable"}}`)), Request: req}, nil
}

// A cycle that started in an older background slot must not mark the new slot done.
func TestLoopStaleSlotDoesNotMarkNewSlotDone(t *testing.T) {
	g := &gateRT{entered: make(chan struct{}), release: make(chan struct{})}
	c, fs, _ := loopFixtureRT(t, Conditions{Online: true, Background: true}, g)
	c.Start(context.Background())
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	fs.Advance(0)
	<-g.entered // cycle of slot 1 is in flight
	p := c.LoopPasses()
	c.SetConditions(Conditions{Online: true})
	c.SetConditions(Conditions{Online: true, Background: true}) // slot 2 begins
	close(g.release)
	waitPasses(t, c, fs, p+2) // end of the cycle of slot 1, then the kick of slot 2
	if n := g.n.Load(); n != 2 {
		t.Fatalf("the new slot must get its own cycle: %d requests", n)
	}
}

// hangRT never answers: the request ends with the context.
type hangRT struct{ n atomic.Int64 }

func (h *hangRT) RoundTrip(req *http.Request) (*http.Response, error) {
	h.n.Add(1)
	<-req.Context().Done()
	return nil, req.Context().Err()
}

// The budget of a background slot (20 s in production, short here) ends a cycle
// against a hub that hangs. That is not a success: last_ok stays empty, the
// failure is counted and SyncNow reports it.
func TestLoopBackgroundBudgetCutWithoutProgressIsAFailure(t *testing.T) {
	h := &hangRT{}
	c, fs, _ := loopFixtureRT(t, Conditions{Online: true, Background: true}, h, func(o *Options) {
		o.BackgroundBudget = 50 * time.Millisecond
	})
	var errEvents, partial atomic.Int64
	c.Subscribe(func(ev Event) {
		switch ev.Type {
		case EventError:
			errEvents.Add(1)
		case EventPartial:
			partial.Add(1)
		}
	})
	startLoop(t, c, fs)
	st := c.Status()
	if !st.LastOK.IsZero() || st.Failures != 1 || !strings.Contains(st.LastError, "without progress") || !st.BackgroundDone {
		t.Fatalf("a cut slot without progress must be a failure: %+v", st)
	}
	r := <-c.SyncNow()
	if r.Err == nil || !errors.Is(r.Err, context.DeadlineExceeded) || !r.Partial {
		t.Fatalf("SyncNow in a cut slot: %+v", r)
	}
	if st := c.Status(); !st.LastOK.IsZero() || st.Failures != 2 {
		t.Fatalf("status after SyncNow: %+v", st)
	}
	// one error event per cut cycle (the no-progress failure), none for the deadline itself
	if partial.Load() != 2 || errEvents.Load() != 2 {
		t.Fatalf("events: partial %d error %d", partial.Load(), errEvents.Load())
	}
}

// Stop answers the SyncNow callers that are still queued.
func TestStopAnswersPendingSyncNow(t *testing.T) {
	h := &hangRT{}
	c, fs, _ := loopFixtureRT(t, Conditions{Online: true}, h)
	c.Start(context.Background())
	fs.Advance(0)
	eventually(t, "the first cycle is in flight", func() bool { return h.n.Load() >= 1 })
	// the loop is stuck in that cycle: these requests stay queued
	queued := []<-chan Result{c.SyncNow(), c.SyncNow(), c.SyncNow()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop must not hang on pending requests: %v", err)
	}
	for i, ch := range queued {
		select {
		case r := <-ch:
			if r.Err == nil {
				t.Fatalf("request %d answered without an error", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d never answered", i)
		}
	}
	if r := <-c.SyncNow(); !errors.Is(r.Err, ErrStopped) {
		t.Fatalf("SyncNow after Stop: %v", r.Err)
	}
}

func TestNotifyWriteDebounce(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true})
	// a long backoff would hide the kick: make the first cycle succeed in "not running"
	// terms by not starting the loop and counting kicks instead
	_ = rt
	for i := 0; i < 5; i++ {
		c.NotifyWrite() // five writes in a row collapse into one kick
	}
	select {
	case <-c.loop.kick:
		t.Fatal("kicked before the debounce interval")
	default:
	}
	fs.Advance(1500 * time.Millisecond)
	select {
	case <-c.loop.kick:
		t.Fatal("kicked before 2 s")
	default:
	}
	fs.Advance(600 * time.Millisecond)
	select {
	case <-c.loop.kick:
	default:
		t.Fatal("no kick after the 2 s debounce")
	}
	select {
	case <-c.loop.kick:
		t.Fatal("a burst of writes must give a single kick")
	default:
	}
}
