//go:build !no_sync

package sync

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// hookRT forwards to the real transport and lets a test intercept requests.
type hookRT struct {
	mu    stdsync.Mutex
	paths []string
	hook  func(req *http.Request) (*http.Response, error)
}

func (h *hookRT) RoundTrip(req *http.Request) (*http.Response, error) {
	h.mu.Lock()
	h.paths = append(h.paths, req.URL.Path+"?"+req.URL.RawQuery)
	hook := h.hook
	h.mu.Unlock()
	if hook != nil {
		if res, err := hook(req); res != nil || err != nil {
			return res, err
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (h *hookRT) count(prefix string) (n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return
}

func (h *hookRT) limits() (out []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.paths {
		if strings.HasPrefix(p, proto.PathPull+"?") {
			for _, kv := range strings.Split(strings.SplitN(p, "?", 2)[1], "&") {
				if v, ok := strings.CutPrefix(kv, "limit="); ok {
					n, _ := strconv.Atoi(v)
					out = append(out, n)
				}
			}
		}
	}
	return
}

func tooLarge(req *http.Request) *http.Response {
	return &http.Response{StatusCode: 413, Header: http.Header{"Content-Type": {"application/json"}}, Request: req,
		Body: io.NopCloser(strings.NewReader(`{"status":413,"message":"too large","data":{"code":"` + proto.CodeResponseTooLarge + `"}}`))}
}

func waitCond(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A metered link must not download a snapshot because the handshake of an
// automatic cycle says so (here: the node is more schema versions behind than
// the hub keeps bundles). The download waits for SyncNow or an unmetered moment.
func TestMeteredLoopDefersRebootstrapUntilAskedFor(t *testing.T) {
	t.Setenv(EnvMaxBundles, "3")
	h, a, _ := hubFixture(t)
	for i := 0; i < 5; i++ {
		addTextField(t, h.app, "items", "f"+string(rune('a'+i)))
	}
	rt := &hookRT{}
	fs := newFsched()
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Sched, o.Now = fs, fs.Now
		o.NoPoke, o.Interval = true, 30*time.Second
		o.Rand = func() float64 { return 0.5 }
		o.HTTP = &http.Client{Transport: rt}
	})
	fs.loopPasses = cl.LoopPasses
	var deferred atomic.Int64
	cl.Subscribe(func(ev client.Event) {
		if ev.Type == client.EventRebootstrap && strings.Contains(ev.Message, "deferred") {
			deferred.Add(1)
		}
	})
	cl.SetConditions(client.Conditions{Online: true, Metered: true})
	cl.Start(ctxb)
	t.Cleanup(func() { _ = cl.Stop(ctxb) })
	fs.Advance(0)
	waitLoopPass(t, cl, 2)

	if n := rt.count(proto.PathSnapshot); n != 0 {
		t.Fatalf("a metered automatic cycle requested a snapshot (%d requests)", n)
	}
	if cur := cursorOf(t, a.spokeEnv); cur.State != client.StateRebootstrapRequired {
		t.Fatalf("cursor state %q, want %q", cur.State, client.StateRebootstrapRequired)
	}
	st := cl.Status()
	if !st.PullDeferred || st.Failures != 0 || !st.LastOK.IsZero() || deferred.Load() != 1 {
		t.Fatalf("deferral must be visible but is neither failure nor success: %+v, events %d", st, deferred.Load())
	}
	// later metered cycles keep waiting
	fs.Advance(30 * time.Minute)
	if n := rt.count(proto.PathSnapshot); n != 0 {
		t.Fatalf("a later metered cycle requested a snapshot (%d)", n)
	}
	// SyncNow is the explicit request: it bootstraps
	if r := <-cl.SyncNow(); r.Err != nil {
		t.Fatalf("SyncNow: %+v", r)
	}
	if rt.count(proto.PathSnapshot) == 0 {
		t.Fatal("SyncNow on a metered link must bootstrap")
	}
	if cur := cursorOf(t, a.spokeEnv); cur.State != client.StateIdle {
		t.Fatalf("cursor state after the bootstrap: %q", cur.State)
	}
	if st := cl.Status(); st.PullDeferred {
		t.Fatalf("pull_deferred must clear after a pulling cycle: %+v", st)
	}
}

// The unmetered transition does the deferred bootstrap without SyncNow.
func TestUnmeteredTransitionRunsTheDeferredRebootstrap(t *testing.T) {
	t.Setenv(EnvMaxBundles, "3")
	h, a, _ := hubFixture(t)
	for i := 0; i < 5; i++ {
		addTextField(t, h.app, "items", "f"+string(rune('a'+i)))
	}
	rt := &hookRT{}
	fs := newFsched()
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Sched, o.Now = fs, fs.Now
		o.NoPoke, o.Interval = true, 30*time.Second
		o.HTTP = &http.Client{Transport: rt}
	})
	fs.loopPasses = cl.LoopPasses
	cl.SetConditions(client.Conditions{Online: true, Metered: true})
	cl.Start(ctxb)
	t.Cleanup(func() { _ = cl.Stop(ctxb) })
	fs.Advance(0)
	waitLoopPass(t, cl, 2)
	if rt.count(proto.PathSnapshot) != 0 {
		t.Fatal("snapshot on a metered link")
	}
	cl.SetConditions(client.Conditions{Online: true})
	waitCond(t, "bootstrap after leaving the metered link", func() bool {
		return rt.count(proto.PathSnapshot) > 0 && cursorOf(t, a.spokeEnv).State == client.StateIdle
	})
}

// The 20 s slot ends a cycle after it pushed: that is progress, not success and
// not a failure; and the hang must not produce an error event or a green result.
func TestBackgroundBudgetCutAfterProgress(t *testing.T) {
	h, a, _ := hubFixture(t)
	rt := &hookRT{hook: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == proto.PathPull { // the hub accepted the push, then went silent
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return nil, nil
	}}
	fs := newFsched()
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Sched, o.Now = fs, fs.Now
		o.NoPoke, o.Interval = true, 30*time.Second
		o.HTTP = &http.Client{Transport: rt}
		o.BackgroundBudget = 3 * time.Second
	})
	fs.loopPasses = cl.LoopPasses
	var errEv, partialEv atomic.Int64
	cl.Subscribe(func(ev client.Event) {
		switch ev.Type {
		case client.EventError:
			errEv.Add(1)
		case client.EventPartial:
			partialEv.Add(1)
		}
	})
	local := a.create(t, map[string]any{"title": "pushed before the cut"})
	cl.SetConditions(client.Conditions{Online: true, Background: true})
	cl.Start(ctxb)
	t.Cleanup(func() { _ = cl.Stop(ctxb) })
	fs.Advance(0)
	waitCond(t, "the slot cycle ends", func() bool { return cl.Status().BackgroundDone })
	if _, err := h.app.FindRecordById("items", local.Id); err != nil {
		t.Fatalf("the push before the cut must have landed: %v", err)
	}
	st := cl.Status()
	if !st.LastOK.IsZero() || st.LastPartial.IsZero() || st.Failures != 0 || st.LastError != "" {
		t.Fatalf("a cut slot with progress is neither success nor failure: %+v", st)
	}
	if errEv.Load() != 0 || partialEv.Load() != 1 {
		t.Fatalf("events: error %d partial %d", errEv.Load(), partialEv.Load())
	}
	if cur := cursorOf(t, a.spokeEnv); strings.Contains(cur.LastError, "deadline") {
		t.Fatalf("the slot deadline must not be stored as last_error: %q", cur.LastError)
	}
	// SyncNow in the slot: nothing left to push, the pull hangs: no progress, an error
	r := <-cl.SyncNow()
	if r.Err == nil || !errors.Is(r.Err, context.DeadlineExceeded) || !r.Partial {
		t.Fatalf("SyncNow without progress must fail: %+v", r)
	}
	if st := cl.Status(); !st.LastOK.IsZero() || st.Failures != 1 {
		t.Fatalf("status: %+v", st)
	}
}

// A metered SyncNow pulls with pages of at most 100 although the configured page is larger.
func TestMeteredSyncNowUsesPageLimit100(t *testing.T) {
	h, a, _ := hubFixture(t)
	rt := &hookRT{}
	fs := newFsched()
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Sched, o.Now = fs, fs.Now
		o.NoPoke, o.Interval, o.Page = true, 30*time.Second, 500
		o.HTTP = &http.Client{Transport: rt}
	})
	fs.loopPasses = cl.LoopPasses
	h.create(t, map[string]any{"title": "x"})
	cl.SetConditions(client.Conditions{Online: true, Metered: true})
	cl.Start(ctxb)
	t.Cleanup(func() { _ = cl.Stop(ctxb) })
	fs.Advance(0)
	waitLoopPass(t, cl, 2)
	if n := len(rt.limits()); n != 0 {
		t.Fatalf("an automatic metered cycle pulled: %v", rt.limits())
	}
	if r := <-cl.SyncNow(); r.Err != nil || r.Pulled == 0 {
		t.Fatalf("SyncNow: %+v", r)
	}
	ls := rt.limits()
	if len(ls) == 0 {
		t.Fatal("no pull request")
	}
	for _, l := range ls {
		if l != 100 {
			t.Fatalf("metered pull limits %v, want 100", ls)
		}
	}
}

// A 413 halves the page; after a run of good pages it grows back (it used to stay small for good).
func TestPageSizeRecoversAfter413(t *testing.T) {
	h, a, _ := hubFixture(t)
	rt := &hookRT{hook: func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == proto.PathPull && req.URL.Query().Get("limit") != "1" {
			return tooLarge(req), nil
		}
		return nil, nil
	}}
	cl := a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Page = 4
		o.HTTP = &http.Client{Transport: rt}
	})
	for i := 0; i < 14; i++ {
		h.create(t, map[string]any{"title": "n" + strconv.Itoa(i)})
	}
	r := cl.RunOnce(ctxb)
	if r.Err != nil || r.Pulled < 14 {
		t.Fatalf("pull: %+v", r)
	}
	ls := rt.limits()
	// 4 (413), 2 (413), then pages of 1; after 8 good pages the page doubles to 2
	if len(ls) < 12 || ls[0] != 4 || ls[1] != 2 || ls[2] != 1 {
		t.Fatalf("limits %v", ls)
	}
	grew := false
	for _, l := range ls[3:] {
		if l > 1 {
			grew = true
		}
	}
	if !grew {
		t.Fatalf("the page never grew back: %v", ls)
	}
}

// SetConditions racing with StartLoop must not be lost.
func TestSetConditionsDuringStartLoopIsNotLost(t *testing.T) {
	t.Setenv("TOKI_SYNC_POKE", "0")
	t.Setenv("TOKI_SYNC_INSECURE", "1")
	_, a, _ := hubFixture(t)
	final := client.Conditions{Online: false, Metered: true}
	for round := 0; round < 25; round++ {
		var wg stdsync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := a.m.StartLoop(ctxb); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			a.m.SetConditions(client.Conditions{Online: true})
			a.m.SetConditions(client.Conditions{Online: false})
			a.m.SetConditions(final)
		}()
		wg.Wait()
		cl := a.m.Client()
		if cl == nil {
			t.Fatal("no client after StartLoop")
		}
		if got := cl.Conditions(); got != final {
			t.Fatalf("round %d: the loop holds %+v, the host last said %+v", round, got, final)
		}
		if err := a.m.StopLoop(ctxb); err != nil {
			t.Fatal(err)
		}
	}
}
