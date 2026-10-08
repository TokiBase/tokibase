//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

func nextOseq(t *testing.T, h *hubEnv, name string) int64 {
	t.Helper()
	var n int64
	if err := h.app.DB().NewQuery("SELECT COALESCE(pushed_origin_seq,0) FROM _sync_nodes WHERE name={:n}").Bind(dbx.Params{"n": name}).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n + 1
}

func tombKind(t *testing.T, app core.App, col, rec string) string {
	t.Helper()
	return tombstoneKind(app.DB(), col, rec)
}

// ---- purge -------------------------------------------------------------

func TestPurgeErasesAndPropagates(t *testing.T) {
	h, a, b := hubFixture(t)
	var audited []string
	SetAuditSink(func(action, _, _ string, d map[string]any) { audited = append(audited, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	r := a.create(t, map[string]any{"title": "personal data", "qty": 2})
	a.sync(t)
	rr, _ := a.app.FindRecordById("items", r.Id)
	rr.Set("title", "more personal data")
	if err := a.app.Save(rr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if b.has(r.Id) == nil {
		t.Fatal("b should hold the record before the purge")
	}
	// keep the local pending change of b: it must lose its data too
	br, _ := b.app.FindRecordById("items", r.Id)
	br.Set("title", "b edit not pushed yet")
	if err := b.app.Save(br); err != nil {
		t.Fatal(err)
	}

	res, err := h.m.Purge("items", r.Id, "gdpr request 42", "cli", true)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deleted || res.Already || res.Seq == 0 || res.Blanked < 2 {
		t.Fatalf("result %+v", res)
	}
	if _, err := h.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the record must be gone from the hub")
	}
	if k := tombKind(t, h.app, h.items.Id, r.Id); k != "legal" {
		t.Fatalf("tombstone kind %q", k)
	}
	// erasure: no patch or hash survives in the log, except the reason of the p row
	rows := changeRows(t, h.app, "record={:r}", dbx.Params{"r": r.Id})
	sawP := false
	for _, c := range rows {
		if c.Op == OpPurge {
			sawP = true
			if c.Status != StatusApplied || !strings.Contains(c.Patch, "gdpr request 42") {
				t.Fatalf("p row %+v", c)
			}
			continue
		}
		if c.Patch != "{}" || len(c.Hash) != 0 {
			t.Fatalf("row %d (%s) still carries data: patch=%s hash=%x", c.Seq, c.Op, c.Patch, c.Hash)
		}
	}
	if !sawP || len(rows) < 3 {
		t.Fatalf("rows %+v", rows)
	}
	// the legal tombstone is immutable (DB triggers)
	for _, q := range []string{
		"UPDATE _sync_tombstones SET kind='delete' WHERE record='" + r.Id + "'",
		"UPDATE _sync_tombstones SET reason='x' WHERE record='" + r.Id + "'",
		"DELETE FROM _sync_tombstones WHERE record='" + r.Id + "'",
	} {
		if _, err := h.app.NonconcurrentDB().NewQuery(q).Execute(); err == nil || !strings.Contains(err.Error(), "permanent") {
			t.Fatalf("%s must be refused by the trigger, got %v", q, err)
		}
	}
	// compaction never prunes it, even with retention 1 ns past
	if _, err := h.app.NonconcurrentDB().NewQuery("UPDATE _sync_tombstones SET created='2000-01-01 00:00:00.000Z' WHERE kind='delete'").Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Compact(ctxb); err != nil {
		t.Fatal(err)
	}
	if k := tombKind(t, h.app, h.items.Id, r.Id); k != "legal" {
		t.Fatal("a legal tombstone is never pruned")
	}

	// the erasure reaches the spokes
	a.sync(t)
	b.sync(t)
	for name, s := range map[string]*itemsSpoke{"a": a, "b": b} {
		if s.has(r.Id) != nil {
			t.Fatalf("%s: the record must be erased", name)
		}
		if k := tombKind(t, s.app, h.items.Id, r.Id); k != "legal" {
			t.Fatalf("%s: tombstone kind %q, want legal", name, k)
		}
		if _, err := s.app.NonconcurrentDB().NewQuery("DELETE FROM _sync_tombstones WHERE kind='legal'").Execute(); err == nil {
			t.Fatalf("%s: the legal tombstone must be permanent on the spoke too", name)
		}
		for _, c := range changeRows(t, s.app, "record={:r}", dbx.Params{"r": r.Id}) {
			if c.Patch != "{}" || len(c.Hash) != 0 {
				t.Fatalf("%s: local log row keeps data: %+v", name, c)
			}
		}
	}

	// b's refused push must not have put data back into the hub log
	for _, c := range changeRows(t, h.app, "record={:r} AND op!='p'", dbx.Params{"r": r.Id}) {
		if c.Patch != "{}" || len(c.Hash) != 0 {
			t.Fatalf("hub row %d (%s/%s) holds data after the purge: %s", c.Seq, c.Op, c.Status, c.Patch)
		}
	}

	// pushes of c and u for the purged id are refused with legal_tombstone
	tok := a.token(t)
	node := a.m.NodeID()
	o := nextOseq(t, h, "gate-1")
	st, pr, eb := rawPush(t, h, tok, pushReq(
		pc(node, o, nowHLC(-3000, 1), 0, h.items.Id, r.Id, "c", map[string]any{"title": "resurrect"}),
		pc(node, o+1, nowHLC(-3000, 2), 0, h.items.Id, r.Id, "u", map[string]any{"title": "resurrect"}),
	))
	if st != 200 {
		t.Fatalf("%d %+v", st, eb)
	}
	for _, x := range pr.Results {
		if x.Status != proto.ResRejected || x.Code != proto.CodeLegalTombstone {
			t.Fatalf("results %+v", pr.Results)
		}
	}
	if _, err := h.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the record must stay gone")
	}
	// a node cannot push a purge
	o = nextOseq(t, h, "gate-1")
	_, pr, _ = rawPush(t, h, tok, pushReq(pc(node, o, nowHLC(-3000, 3), 0, h.items.Id, "other0000000001", "p", map[string]any{})))
	if pr.Results[0].Status != proto.ResRejected {
		t.Fatalf("a node must not push a purge: %+v", pr.Results)
	}
	// local create with the purged id fails on a spoke
	col := a.coll()
	again := core.NewRecord(col)
	again.Id = r.Id
	again.Set("title", "again")
	if err := a.app.Save(again); err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("a purged id can not be created again, got %v", err)
	}

	// idempotent
	res2, err := h.m.Purge("items", r.Id, "again", "cli", true)
	if err != nil || !res2.Already || res2.Seq != 0 {
		t.Fatalf("second purge: %+v %v", res2, err)
	}
	found := false
	for _, x := range audited {
		found = found || x == AuditPurge
	}
	if !found {
		t.Fatalf("audit actions %v", audited)
	}
}

func TestPurgeUpgradesDeleteTombstoneAndValidates(t *testing.T) {
	h, _, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "x"})
	if err := h.app.Delete(r); err != nil {
		t.Fatal(err)
	}
	if k := tombKind(t, h.app, h.items.Id, r.Id); k != "delete" {
		t.Fatalf("kind %q", k)
	}
	res, err := h.m.Purge("items", r.Id, "erase", "cli", true)
	if err != nil || res.Deleted || res.Already {
		t.Fatalf("%+v %v", res, err)
	}
	if k := tombKind(t, h.app, h.items.Id, r.Id); k != "legal" {
		t.Fatalf("a delete tombstone is upgraded to legal, got %q", k)
	}
	// an id that never existed can be purged in advance
	if _, err := h.m.Purge("items", "neverexisted001", "erase", "cli", true); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][3]string{{"items", r.Id, ""}, {"items", "", "x"}, {"nope", r.Id, "x"}} {
		if _, err := h.m.Purge(bad[0], bad[1], bad[2], "cli", true); err == nil {
			t.Fatalf("purge %v must fail", bad)
		}
	}
	// purge is hub-local: a collection without policy can be purged (old log rows
	// may still hold patches); a system collection can not
	c := core.NewBaseCollection("plain")
	c.Fields.Add(&core.TextField{Name: "x"})
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Purge("plain", "someid000000001", "x", "cli", true); err != nil {
		t.Fatalf("a collection without policy can be purged: %v", err)
	}
	if _, err := h.m.Purge(PoliciesCollection, "someid000000001", "x", "cli", true); err == nil {
		t.Fatal("a system collection can not be purged")
	}
}

func TestPurgeEndpointAndCommand(t *testing.T) {
	t.Setenv(EnvRole, "hub")
	h, _, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "x"})
	body := `{"collection":"items","record":"` + r.Id + `","reason":"court order","legal":true}`
	if code, _ := h.do(t, nil, "POST", proto.PathPurge, body); code == 200 {
		t.Fatal("anonymous purge must be refused")
	}
	if code, _ := h.do(t, h.usr, "POST", proto.PathPurge, body); code == 200 {
		t.Fatal("a normal user must not purge")
	}
	if code, _ := h.do(t, h.su, "POST", proto.PathPurge, `{"collection":"items","record":"`+r.Id+`","reason":"x"}`); code != 400 {
		t.Fatalf("without legal:true the request is refused, got %d", code)
	}
	if code, out := h.do(t, h.su, "POST", proto.PathPurge, `{"collection":"items","record":"`+r.Id+`","legal":true}`); code != 400 {
		t.Fatalf("without a reason the request is refused, got %d %s", code, out)
	}
	code, out := h.do(t, h.su, "POST", proto.PathPurge, body)
	if code != 200 {
		t.Fatalf("purge: %d %s", code, out)
	}
	var res PurgeResult
	_ = json.Unmarshal(out, &res)
	if !res.Deleted || res.Seq == 0 {
		t.Fatalf("%s", out)
	}
	if _, err := h.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("record must be gone")
	}

	// CLI
	r2 := h.create(t, map[string]any{"title": "y"})
	if _, err := runCmd(t, purgeCommand(h.app), "items", r2.Id, "--reason", "x"); err == nil {
		t.Fatal("--legal is required")
	}
	if out, err := runCmd(t, purgeCommand(h.app), "items", r2.Id, "--legal", "--reason", "cli test"); err != nil || !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("%v %s", err, out)
	}
	if k := tombKind(t, h.app, h.items.Id, r2.Id); k != "legal" {
		t.Fatalf("kind %q", k)
	}
}

// ---- compaction --------------------------------------------------------

func backdateChanges(t *testing.T, app core.App, age time.Duration) {
	t.Helper()
	ts := time.Now().UTC().Add(-age).Format("2006-01-02 15:04:05.000Z")
	if _, err := app.NonconcurrentDB().NewQuery("UPDATE _changes SET created={:t}").Bind(dbx.Params{"t": ts}).Execute(); err != nil {
		t.Fatal(err)
	}
}

func nodePulled(t *testing.T, h *hubEnv, name string) int64 {
	t.Helper()
	var n int64
	if err := h.app.DB().NewQuery("SELECT COALESCE(pulled_seq,0) FROM _sync_nodes WHERE name={:n}").Bind(dbx.Params{"n": name}).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func minChangeSeq(t *testing.T, app core.App) int64 {
	t.Helper()
	var n int64
	_ = app.DB().NewQuery("SELECT COALESCE(MIN(seq),0) FROM _changes").Row(&n)
	return n
}

func TestCompactionKeepsTheUnackedMinimum(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 3; i++ {
		h.create(t, map[string]any{"title": "early " + strconv.Itoa(i)})
	}
	a.create(t, map[string]any{"title": "from a"})
	a.sync(t)
	b.sync(t)
	// b falls behind: more hub writes after its last pull
	h.create(t, map[string]any{"title": "late 1"})
	h.create(t, map[string]any{"title": "late 2"})
	a.sync(t)
	pulledB := nodePulled(t, h, "gate-2")
	head := h.m.headSeq()
	if pulledB == 0 || pulledB >= head {
		t.Fatalf("setup: b pulled %d, head %d", pulledB, head)
	}

	// everything is younger than TOKI_SYNC_MIN_KEEP: nothing goes
	rep, err := h.m.Compact(ctxb)
	if err != nil || rep.ChangesDeleted != 0 || rep.LowWater != 0 {
		t.Fatalf("MIN_KEEP must protect fresh rows: %+v %v", rep, err)
	}

	backdateChanges(t, h.app, 48*time.Hour)
	rep, err = h.m.Compact(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Safe != pulledB || rep.ChangesDeleted == 0 || rep.LowWater != pulledB {
		t.Fatalf("report %+v (b pulled %d)", rep, pulledB)
	}
	if min := minChangeSeq(t, h.app); min != pulledB+1 {
		t.Fatalf("the rows b has not pulled must stay: oldest seq %d, want %d", min, pulledB+1)
	}
	if h.m.headSeq() != head {
		t.Fatal("the head seq must not move")
	}
	// the laggard resumes without a snapshot and everybody converges
	b.sync(t)
	a.sync(t)
	requireConverged(t, h, a, b)
	// everything is pulled by everybody now: the next run takes the rest, the head stays
	rep, err = h.m.Compact(ctxb)
	if err != nil || rep.LowWater != head || h.m.headSeq() != head {
		t.Fatalf("second run: %+v %v (head %d)", rep, err, head)
	}
	// low_water survives a restart and is reported by the handshake; a node AT low_water is fine
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	hs, err := a.c.Handshake(ctxb)
	if err != nil || hs.LowWater != head || hs.Rebootstrap {
		t.Fatalf("handshake %+v %v", hs, err)
	}
	h.create(t, map[string]any{"title": "after compaction"})
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
	pulledB = head

	// a node that joins now would start below low_water: it is told to re-bootstrap
	c := newItemsSpoke(t, h, "gate-3")
	r := c.c.RunOnce(ctxb)
	if r.Err == nil || r.Err != client.ErrRebootstrap {
		t.Fatalf("a new node below low_water must get ErrRebootstrap, got %v", r.Err)
	}
	if st := c.c.Status(); st.State != "rebootstrap_required" {
		t.Fatalf("status %+v", st)
	}
	cur, _ := client.LoadCursor(c.app)
	if cur.State != "rebootstrap_required" || !strings.Contains(cur.LastError, "re-bootstrap") {
		t.Fatalf("cursor %+v", cur)
	}
	// the direct pull answers 410
	tok := c.c.Token()
	st, _ := rawPull(t, h, tok, "after=0")
	if st != 410 {
		t.Fatalf("pull below low_water: %d", st)
	}
	// at low_water it still works
	if st, _ = rawPull(t, h, tok, "after="+strconv.FormatInt(pulledB, 10)); st != 200 {
		t.Fatalf("pull at low_water: %d", st)
	}
}

func TestCompactionMarksStaleNodesAndTheyGet410(t *testing.T) {
	h, a, b := hubFixture(t)
	h.create(t, map[string]any{"title": "one"})
	a.sync(t)
	b.sync(t)
	// b disappears for longer than the retention; the hub keeps writing
	if _, err := h.app.NonconcurrentDB().NewQuery("UPDATE _sync_nodes SET last_seen={:t} WHERE name='gate-2'").
		Bind(dbx.Params{"t": time.Now().UTC().Add(-100 * 24 * time.Hour).Format("2006-01-02 15:04:05.000Z")}).Execute(); err != nil {
		t.Fatal(err)
	}
	h.create(t, map[string]any{"title": "two"})
	h.create(t, map[string]any{"title": "three"})
	a.sync(t)
	backdateChanges(t, h.app, 48*time.Hour)

	rep, err := h.m.Compact(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StaleNodes != 1 {
		t.Fatalf("report %+v", rep)
	}
	if st := nodeStatusOf(t, h, "gate-2"); st != NodeStale {
		t.Fatalf("b is %q", st)
	}
	if st := nodeStatusOf(t, h, "gate-1"); st != NodeActive {
		t.Fatalf("a is %q", st)
	}
	// the stale node no longer holds the minimum back
	if rep.Safe != nodePulled(t, h, "gate-1") || rep.ChangesDeleted == 0 {
		t.Fatalf("a stale node must leave the minimum: %+v", rep)
	}
	// b: handshake says re-bootstrap, pull answers 410 whatever its cursor
	hs, err := b.c.Handshake(ctxb)
	if err != nil || !hs.Rebootstrap || hs.LowWater != rep.LowWater {
		t.Fatalf("handshake %+v %v", hs, err)
	}
	tok := b.c.Token()
	curB, _ := client.LoadCursor(b.app)
	if st, _ := rawPull(t, h, tok, "after="+strconv.FormatInt(curB.PullAfter, 10)); st != 410 {
		t.Fatalf("a stale node's pull: %d", st)
	}
	res := b.c.RunOnce(ctxb)
	if res.Err != client.ErrRebootstrap {
		t.Fatalf("RunOnce: %v", res.Err)
	}
	if st := b.c.Status(); st.State != "rebootstrap_required" {
		t.Fatalf("status %+v", st)
	}
	// a stays healthy
	h.create(t, map[string]any{"title": "four"})
	a.sync(t)
}

func nodeStatusOf(t *testing.T, h *hubEnv, name string) string {
	t.Helper()
	var s string
	if err := h.app.DB().NewQuery("SELECT status FROM _sync_nodes WHERE name={:n}").Bind(dbx.Params{"n": name}).Row(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCompactionRetentionDropsEvenUnackedRows(t *testing.T) {
	t.Setenv(EnvRetention, "1d")
	h, a, _ := hubFixture(t) // gate-2 never pulls
	h.create(t, map[string]any{"title": "one"})
	h.create(t, map[string]any{"title": "two"})
	a.sync(t)
	backdateChanges(t, h.app, 36*time.Hour)
	rep, err := h.m.Compact(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StaleNodes != 0 { // it was seen recently, it is just slow
		t.Fatalf("%+v", rep)
	}
	if rep.Safe != 0 {
		t.Fatalf("a node that pulled nothing holds the minimum at 0: %+v", rep)
	}
	if rep.ChangesDeleted < 2 || rep.LowWater < 2 {
		t.Fatalf("rows older than the retention go regardless: %+v", rep)
	}
	// the slow node is below low_water and must re-bootstrap
	hub2 := nodePulled(t, h, "gate-2")
	if hub2 >= rep.LowWater {
		t.Fatalf("setup: gate-2 pulled %d, low water %d", hub2, rep.LowWater)
	}
}

func TestCompactionNeverTouchesParkedRows(t *testing.T) {
	t.Setenv(EnvRetention, "1d")
	h, a, _ := hubFixture(t)
	a.sync(t)
	// a parked change (waits for an admin): never compacted, even past the retention
	if _, err := h.m.insertHubRow(h.app.NonconcurrentDB(), &hubRow{node: "nparked", oseq: 1, hlc: 1, col: h.items.Id, rec: "parked000000001", op: "u", patch: `{"title":"x"}`, status: StatusParked, code: "actor_revoked"}); err != nil {
		t.Fatal(err)
	}
	h.create(t, map[string]any{"title": "later"})
	backdateChanges(t, h.app, 72*time.Hour)
	if _, err := h.m.Compact(ctxb); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE status='parked'", nil); n != 1 {
		t.Fatalf("parked rows: %d", n)
	}
}

func TestCompactionPrunesTombstonesAndConflicts(t *testing.T) {
	h, _, _ := hubFixture(t)
	old := time.Now().UTC().Add(-100 * 24 * time.Hour).Format("2006-01-02 15:04:05.000Z")
	db := h.app.NonconcurrentDB()
	put := func(rec, kind, created string) {
		t.Helper()
		if _, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created) VALUES ({:c},{:r},{:k},1,'n','','',{:t})`).
			Bind(dbx.Params{"c": h.items.Id, "r": rec, "k": kind, "t": created}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	fresh := time.Now().UTC().Format("2006-01-02 15:04:05.000Z")
	put("olddelete000001", "delete", old)
	put("newdelete000001", "delete", fresh)
	put("oldlegal0000001", "legal", old)

	cc, _ := h.app.FindCollectionByNameOrId(ConflictsCollection)
	mk := func(status, resolved string) string {
		r := core.NewRecord(cc)
		r.Set("collection", "items")
		r.Set("record", "r")
		r.Set("kind", "orphaned")
		r.Set("resolution", "reverted")
		r.Set("status", status)
		if err := h.app.Save(r); err != nil {
			t.Fatal(err)
		}
		if resolved != "" {
			if _, err := db.NewQuery("UPDATE _sync_conflicts SET resolved_at={:t} WHERE id={:id}").Bind(dbx.Params{"t": resolved, "id": r.Id}).Execute(); err != nil {
				t.Fatal(err)
			}
		}
		return r.Id
	}
	oldRes := mk("resolved", time.Now().UTC().Add(-91*24*time.Hour).Format("2006-01-02 15:04:05.000Z"))
	newRes := mk("resolved", time.Now().UTC().Add(-10*24*time.Hour).Format("2006-01-02 15:04:05.000Z"))
	open := mk("open", "")

	rep, err := h.m.Compact(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if rep.TombstonesDeleted != 1 || rep.ConflictsDeleted != 1 {
		t.Fatalf("report %+v", rep)
	}
	if k := tombKind(t, h.app, h.items.Id, "olddelete000001"); k != "" {
		t.Fatal("an old delete tombstone is pruned")
	}
	if tombKind(t, h.app, h.items.Id, "newdelete000001") != "delete" || tombKind(t, h.app, h.items.Id, "oldlegal0000001") != "legal" {
		t.Fatal("young delete tombstones and legal tombstones stay")
	}
	for id, want := range map[string]bool{oldRes: false, newRes: true, open: true} {
		_, err := h.app.FindRecordById(ConflictsCollection, id)
		if (err == nil) != want {
			t.Fatalf("conflict %s present=%v want %v", id, err == nil, want)
		}
	}
}

func TestSpokeKeepPrunesAckedRows(t *testing.T) {
	h, a, _ := hubFixture(t)
	for i := 0; i < 3; i++ {
		a.create(t, map[string]any{"title": "row " + strconv.Itoa(i)})
	}
	a.sync(t) // all acked
	a.create(t, map[string]any{"title": "pending"})
	backdateChanges(t, a.app, 48*time.Hour)

	rep, err := a.m.Compact(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Role != "spoke" || rep.SpokeRowsDeleted != 3 {
		t.Fatalf("report %+v", rep)
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes WHERE status='local'", nil); n != 1 {
		t.Fatalf("the unacked row must stay, %d local rows", n)
	}
	// it still pushes; a fresh acked row is younger than SPOKE_KEEP and stays
	a.sync(t)
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM items WHERE title='pending'", nil); n != 1 {
		t.Fatal("pending row not pushed")
	}
	a.create(t, map[string]any{"title": "young"})
	a.sync(t)
	rep, _ = a.m.Compact(ctxb) // row 4 (acked, 48 h old) goes, row 5 (acked now) stays
	if rep.SpokeRowsDeleted != 1 {
		t.Fatalf("%+v", rep)
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes WHERE status='acked'", nil); n != 1 {
		t.Fatalf("%d acked rows left", n)
	}
	if rep, _ = a.m.Compact(ctxb); rep.SpokeRowsDeleted != 0 {
		t.Fatalf("%+v", rep)
	}
	t.Setenv(EnvSpokeKeep, "1h")
	backdateChanges(t, a.app, 2*time.Hour)
	if rep, _ = a.m.Compact(ctxb); rep.SpokeRowsDeleted != 1 {
		t.Fatalf("TOKI_SYNC_SPOKE_KEEP=1h: %+v", rep)
	}
}

// fakeQueue records what the module enqueues.
type fakeQueue struct {
	keys     []string
	handlers map[string]kernel.JobHandler
}

func (q *fakeQueue) Enqueue(_ context.Context, kind string, _ any, opts ...kernel.EnqueueOption) (string, error) {
	o := kernel.ResolveEnqueueOptions(opts...)
	q.keys = append(q.keys, kind+"|"+o.CronKey)
	return "job1", nil
}
func (q *fakeQueue) Register(kind string, h kernel.JobHandler) {
	if q.handlers == nil {
		q.handlers = map[string]kernel.JobHandler{}
	}
	q.handlers[kind] = h
}
func (q *fakeQueue) Stats(context.Context) (kernel.JobStats, error) { return kernel.JobStats{}, nil }

func TestCompactionCronUsesTheJobQueue(t *testing.T) {
	h := newHub(t)
	q := &fakeQueue{}
	kernel.SetJobs(h.app, q) // replays the handler that the module registered early
	if q.handlers[CompactKind] == nil {
		t.Fatal("the module must register the sync.compact job handler")
	}
	h.m.enqueueCompact(time.Date(2026, 10, 8, 10, 30, 0, 0, time.UTC))
	h.m.enqueueCompact(time.Date(2026, 10, 8, 11, 0, 0, 0, time.UTC))
	want := []string{"sync.compact|sync.compact:2026100810", "sync.compact|sync.compact:2026100811"}
	if len(q.keys) != 2 || q.keys[0] != want[0] || q.keys[1] != want[1] {
		t.Fatalf("enqueued %v, want %v", q.keys, want)
	}
	if err := q.handlers[CompactKind](ctxb, h.app, &kernel.Job{}); err != nil {
		t.Fatalf("the job handler runs a compaction: %v", err)
	}
	if jobs := h.app.Cron().Jobs(); len(jobs) == 0 {
		t.Fatal("an hourly cron entry must exist")
	} else {
		found := false
		for _, j := range jobs {
			found = found || (j.Id() == "__tokiSyncCompact" && j.Expression() == "0 * * * *")
		}
		if !found {
			t.Fatal("cron entry __tokiSyncCompact with `0 * * * *` not found")
		}
	}
}

func TestCompactionWithoutJobQueueRunsInline(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.create(t, map[string]any{"title": "x"})
	a.sync(t)
	backdateChanges(t, h.app, 48*time.Hour)
	// two nodes: gate-2 never pulled, so nothing is deleted; but it must not fail
	h.m.enqueueCompact(time.Now())
}

func TestHealthBlock(t *testing.T) {
	h, a, b := hubFixture(t)
	h.create(t, map[string]any{"title": "x"})
	a.sync(t)
	// superusers see the sync block, everybody else nothing
	code, body := h.do(t, h.su, "GET", "/api/health", "")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var resp struct {
		Data struct {
			Sync *HealthBlock `json:"sync"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Data.Sync == nil {
		t.Fatalf("no sync block: %s", body)
	}
	hb := resp.Data.Sync
	if hb.Role != "hub" || hb.ActiveNodes != 2 || hb.Head != h.m.headSeq() || hb.Pending < 1 || hb.StaleNodes != 0 || hb.LowWater != 0 {
		t.Fatalf("block %+v", hb)
	}
	_, body = h.do(t, nil, "GET", "/api/health", "")
	if strings.Contains(string(body), "sync") {
		t.Fatalf("guests must not see the block: %s", body)
	}
	_, body = h.do(t, h.usr, "GET", "/api/health", "")
	if strings.Contains(string(body), `"sync"`) {
		t.Fatalf("normal users must not see the block: %s", body)
	}
	// spoke: pending = unacknowledged local changes
	b.create(t, map[string]any{"title": "pending on b"})
	if hbs := b.m.Health(); hbs == nil || hbs.Role != "spoke" || hbs.Pending != 1 {
		t.Fatalf("spoke block %+v", hbs)
	}
	// stale count
	_, _ = h.app.NonconcurrentDB().NewQuery("UPDATE _sync_nodes SET status='stale' WHERE name='gate-2'").Execute()
	if hb := h.m.Health(); hb.StaleNodes != 1 || hb.ActiveNodes != 1 {
		t.Fatalf("%+v", hb)
	}
}
