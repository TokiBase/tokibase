//go:build !no_sync

package sync

import (
	"net/http"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// skewClock makes the module and its client believe it is d later than the hub.
func skewClock(s *spokeEnv, d time.Duration) func() time.Time {
	now := func() time.Time { return time.Now().Add(d) }
	s.m.now = now
	if err := s.m.Init(); err != nil { // rebuild the HLC clock on the skewed wall clock
		panic(err)
	}
	return now
}

func TestPushIsRefusedWhileTheClockIsOutOfToleranceThenRecovers(t *testing.T) {
	t.Setenv(EnvMaxDrift, "1m")
	h := newHub(t)
	h.policy(t, "items", DirBoth, nil, nil)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	now := skewClock(s, 3*time.Minute) // inside the signature window, outside the 1 minute tolerance
	cl := s.client(t, h, func(o *client.Options) { o.Now = now; o.Backend = backend{s.m}; o.Interval = time.Hour })

	hs, err := cl.Handshake(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if hs.Clock.Ok || hs.Clock.OffsetMs > -170000 || hs.Clock.OffsetMs < -190000 {
		t.Fatalf("clock verdict: %+v", hs.Clock)
	}
	// the push of a node whose last handshake was out of tolerance is refused
	tok := cl.Token()
	req := pushReq(pc(s.m.NodeID(), 1, nowHLC(-10, 0), 0, h.items.Id, "rec000000000001", "c", map[string]any{"title": "x"}))
	req.SchemaVersion = hubSchemaVersion(h)
	st, _, eb := rawPush(t, h, tok, req)
	if st != http.StatusConflict || eb.Data["code"] != proto.CodeClockDrift {
		t.Fatalf("push while out of tolerance: %d %+v", st, eb)
	}
	// the metric is in the health block
	hb := h.m.Health()
	if hb.MaxSkewMs < 170000 || len(hb.NodeSkew) != 1 || hb.NodeSkew[0].Name != "gate-1" {
		t.Fatalf("health: %+v", hb)
	}
	// a handshake with the corrected clock lifts it
	hs, err = cl.Handshake(ctxb)
	if err != nil || !hs.Clock.Ok {
		t.Fatalf("second handshake: %+v %v", hs, err)
	}
	if st, _, eb := rawPush(t, h, cl.Token(), req); st != 200 {
		t.Fatalf("push after the corrected handshake: %d %+v", st, eb)
	}
}

func TestSpokeWithAClockTwoDaysAheadIsCorrectedAndRestamped(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter}, nil)
	a := newItemsSpoke(t, h, "gate-1")
	a.sync(t) // the policies
	s := a.spokeEnv
	now := skewClock(s, 48*time.Hour)
	a.c = s.client(t, h, func(o *client.Options) { o.Now = now; o.Backend = backend{s.m}; o.Interval = time.Hour })

	// three edits, stamped by the wrong clock
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, a.create(t, map[string]any{"title": "t" + string(rune('0'+i))}).Id)
	}
	rows := changeRows(t, s.app, "node={:n}", dbx.Params{"n": s.m.NodeID()})
	if len(rows) != 3 || hlc.HLC(rows[0].HLC).Physical().Before(time.Now().Add(47*time.Hour)) {
		t.Fatalf("the changes should carry the skewed clock: %+v", rows)
	}
	// the next session corrects the offset, re-stamps and pushes (no future_hlc)
	res := a.sync(t)
	if res.Pushed != 3 || res.Rejected != 0 {
		t.Fatalf("result %+v", res)
	}
	for _, id := range ids {
		hubItem(t, h, id)
	}
	hrows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": s.m.NodeID()})
	if len(hrows) != 3 {
		t.Fatalf("hub rows %d", len(hrows))
	}
	var prev int64
	for i, r := range hrows {
		p := hlc.HLC(r.HLC).Physical()
		if d := time.Since(p); d > time.Minute || d < -time.Minute {
			t.Fatalf("row %d: re-stamped hlc %v is not now", i, p)
		}
		if r.HLC <= prev {
			t.Fatalf("origin order must be kept: %d after %d", r.HLC, prev)
		}
		prev = r.HLC
	}
	// the record clocks and the persisted floor follow
	var maxMeta int64
	_ = s.app.DB().NewQuery("SELECT MAX(hlc) FROM _sync_meta").Row(&maxMeta)
	if hlc.HLC(maxMeta).Physical().After(time.Now().Add(time.Minute)) {
		t.Fatalf("_sync_meta still carries the old stamps: %v", hlc.HLC(maxMeta).Physical())
	}
	if fl, _ := hlc.LoadFloor(dbState{db: s.app.NonconcurrentDB()}); fl.Physical().After(time.Now().Add(time.Minute)) {
		t.Fatalf("the persisted floor was not lowered: %v", fl.Physical())
	}
	if s.m.Clock().Last().Physical().After(time.Now().Add(time.Minute)) {
		t.Fatal("the clock was not lowered")
	}
	// new writes use the corrected clock
	r := a.create(t, map[string]any{"title": "after"})
	if row := changeRows(t, s.app, "record={:r}", dbx.Params{"r": r.Id}); len(row) != 1 || hlc.HLC(row[0].HLC).Physical().After(time.Now().Add(time.Minute)) {
		t.Fatalf("new change: %+v", row)
	}
	a.sync(t)
	requireConverged(t, h, a)
}

func TestFutureHLCIsRejectedPerChange(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	cid := h.items.Id
	mk := func(oseq int64, rec string, offMs int64) proto.PushResult {
		req := pushReq(pc(a.m.NodeID(), oseq, nowHLC(offMs, 0), 0, cid, rec, "c", map[string]any{"title": rec}))
		req.SchemaVersion = hubSchemaVersion(h)
		st, ok, eb := rawPush(t, h, tok, req)
		if st != 200 {
			t.Fatalf("push: %d %+v", st, eb)
		}
		return ok.Results[0]
	}
	if r := mk(1, "rec000000000001", 4*60*1000); r.Status != proto.ResApplied {
		t.Fatalf("4 minutes ahead is inside the tolerance: %+v", r)
	}
	if r := mk(2, "rec000000000002", 10*60*1000); r.Status != proto.ResRejected || r.Code != proto.CodeFutureHLC {
		t.Fatalf("10 minutes ahead: %+v", r)
	}
	// a forged HLC does not move the hub clock
	if h.m.Clock().Last().Physical().After(time.Now().Add(6 * time.Minute)) {
		t.Fatal("the hub clock followed a forged hlc")
	}
}
