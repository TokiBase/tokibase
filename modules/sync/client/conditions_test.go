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

func loopFixture(t *testing.T, cond Conditions) (*Client, *fakeSched, *countingRT) {
	t.Helper()
	id, err := proto.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	rt := &countingRT{}
	fs := newFakeSched()
	c, err := New(Options{
		Identity: id, HubURL: "http://127.0.0.1:1", Insecure: true, Cert: "cert", HubID: "hubtest",
		HubPub: base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)), HTTP: &http.Client{Transport: rt},
		Sched: fs, Now: fs.Now, NoPoke: true, Interval: 30 * time.Second, Debounce: 2 * time.Second,
		Rand: func() float64 { return 0.5 }, // jitter factor exactly 1
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetConditions(cond)
	return c, fs, rt
}

func startLoop(t *testing.T, c *Client, fs *fakeSched) {
	t.Helper()
	c.Start(context.Background())
	t.Cleanup(func() { _ = c.Stop(context.Background()) })
	fs.Advance(0) // the loop's first timer is due at once
}

// attempts waits until the loop has settled and returns the request count.
func attempts(rt *countingRT) int64 {
	time.Sleep(60 * time.Millisecond)
	return rt.n.Load()
}

func TestLoopOfflineMakesNoAttempts(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: false})
	startLoop(t, c, fs)
	fs.Advance(time.Hour)
	if n := attempts(rt); n != 0 {
		t.Fatalf("offline: %d requests", n)
	}
	if r := <-c.SyncNow(); !errors.Is(r.Err, ErrOffline) {
		t.Fatalf("SyncNow offline: %v", r.Err)
	}
	if attempts(rt) != 0 {
		t.Fatal("SyncNow offline must not touch the network")
	}
	// back online: a cycle starts at once
	c.SetConditions(Conditions{Online: true})
	fs.Advance(0)
	if n := attempts(rt); n == 0 {
		t.Fatal("going online must trigger a cycle")
	}
}

func TestLoopBackoffDoublesWithInjectedClock(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true})
	startLoop(t, c, fs)
	n0 := attempts(rt) // first failure at t=0, next attempt after 1 s
	if n0 == 0 {
		t.Fatal("no first attempt")
	}
	per := n0 // requests per failed cycle
	fs.Advance(900 * time.Millisecond)
	if attempts(rt) != n0 {
		t.Fatal("retried before the 1 s backoff")
	}
	fs.Advance(200 * time.Millisecond)
	if attempts(rt) != 2*per {
		t.Fatalf("no retry after 1 s: %d requests", rt.n.Load())
	}
	fs.Advance(1800 * time.Millisecond) // second failure: 2 s (retry due at t=3.0 s)
	if attempts(rt) != 2*per {
		t.Fatal("retried before the 2 s backoff")
	}
	fs.Advance(300 * time.Millisecond)
	if attempts(rt) != 3*per {
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

func TestLoopBackgroundRunsOneBoundedCycle(t *testing.T) {
	c, fs, rt := loopFixture(t, Conditions{Online: true, Background: true})
	startLoop(t, c, fs)
	n0 := attempts(rt)
	if n0 == 0 {
		t.Fatal("the background slot must run one cycle")
	}
	if !c.Status().BackgroundDone {
		t.Fatal("status must say the slot's cycle ran")
	}
	fs.Advance(30 * time.Minute)
	c.Kick()
	if attempts(rt) != n0 {
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
	if attempts(rt) <= n1 {
		t.Fatal("a new background slot must run a cycle")
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
