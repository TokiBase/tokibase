//go:build !no_sync

package sync

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// compactEverything makes the hub forget its whole log (low_water = head).
func compactEverything(t *testing.T, h *hubEnv) {
	t.Helper()
	t.Setenv(EnvMinKeep, "1ms")
	backdateChanges(t, h.app, time.Hour)
	if _, err := h.m.Compact(ctxb); err != nil {
		t.Fatal(err)
	}
	if h.m.lowWater() != h.m.headSeq() || h.m.lowWater() == 0 {
		t.Fatalf("low_water %d head %d", h.m.lowWater(), h.m.headSeq())
	}
}

func cursorOf(t *testing.T, s *spokeEnv) *client.Cursor {
	t.Helper()
	cur, err := client.LoadCursor(s.app)
	if err != nil || cur == nil {
		t.Fatalf("cursor %v %v", cur, err)
	}
	return cur
}

func TestSnapshotBootstrapsANewNodeAfterCompaction(t *testing.T) {
	t.Setenv(client.EnvSnapshotPage, "20")
	h, a, b := hubFixture(t)
	for i := 0; i < 70; i++ {
		h.create(t, map[string]any{"title": "rec " + strconv.Itoa(i), "qty": i, "tags": []string{"a"}})
	}
	gone := h.create(t, map[string]any{"title": "deleted"})
	if err := h.app.Delete(gone); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	compactEverything(t, h)

	c := newItemsSpoke(t, h, "gate-3")
	// the plain cycle only reports it (P56-2); the loop's cycleBoot would bootstrap
	if r := c.c.RunOnce(ctxb); r.Err != client.ErrRebootstrap {
		t.Fatalf("RunOnce: %v", r.Err)
	}
	if cur := cursorOf(t, c.spokeEnv); cur.State != client.StateRebootstrapRequired {
		t.Fatalf("state %q", cur.State)
	}
	if err := c.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	cur := cursorOf(t, c.spokeEnv)
	if cur.State != client.StateIdle || cur.SnapshotID != "" || cur.SnapshotAfter != "" || cur.PullAfter != h.m.headSeq() {
		t.Fatalf("cursor after bootstrap %+v (head %d)", cur, h.m.headSeq())
	}
	if k := tombKind(t, c.app, h.items.Id, gone.Id); k != "delete" {
		t.Fatalf("the delete tombstone must arrive with the snapshot, got %q", k)
	}
	c.sync(t)
	requireConverged(t, h, a, b, c)
	// the hub reactivated the node and counts it as pulled
	if st := nodeStatusOf(t, h, "gate-3"); st != NodeActive {
		t.Fatalf("status %q", st)
	}
	// life goes on
	h.create(t, map[string]any{"title": "after"})
	c.sync(t)
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b, c)
}

// failAfter fails every request to the snapshot pages after n of them.
type failAfter struct {
	n     int32
	count atomic.Int32
	pages atomic.Int32
}

func (f *failAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, proto.PathSnapshot) {
		f.pages.Add(1)
		if f.count.Add(1) > f.n {
			return nil, errors.New("network down")
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestSnapshotResumesAfterANetworkLoss(t *testing.T) {
	t.Setenv(client.EnvSnapshotPage, "10")
	h, a, b := hubFixture(t)
	for i := 0; i < 55; i++ {
		h.create(t, map[string]any{"title": "rec " + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	compactEverything(t, h)

	s := newItemsSpoke(t, h, "gate-3")
	flaky := &failAfter{n: 2}
	s.c = s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
		o.HTTP = &http.Client{Transport: flaky}
	})
	if err := s.c.Bootstrap(ctxb); err == nil {
		t.Fatal("the bootstrap must fail when the network goes down")
	}
	cur := cursorOf(t, s.spokeEnv)
	if cur.State != client.StateBootstrapping || cur.SnapshotID == "" || !strings.Contains(cur.SnapshotAfter, "/") {
		t.Fatalf("cursor %+v", cur)
	}
	have := countRows(t, s.app, "SELECT COUNT(*) FROM items", nil)
	if have == 0 || have >= 55 {
		t.Fatalf("a partial snapshot expected, %d rows", have)
	}
	// the network is back: a new client resumes at the saved position
	good := &failAfter{n: 1 << 20}
	s.c = s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
		o.HTTP = &http.Client{Transport: good}
	})
	if err := s.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	if got := int(good.pages.Load()); got > 5 {
		t.Fatalf("a resumed snapshot must not start over: %d page requests", got)
	}
	s.sync(t)
	requireConverged(t, h, a, s)
}

func TestSnapshotIDExpiryAndBinding(t *testing.T) {
	h, a, b := hubFixture(t)
	now := time.Now()
	id, _, err := h.m.mintSnapshotID("nodeA", 7, now)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := h.m.parseSnapshotID(id, "nodeA"); !ok || c.Seq != 7 {
		t.Fatal("a fresh id must verify")
	}
	if _, ok := h.m.parseSnapshotID(id, "nodeB"); ok {
		t.Fatal("an id is bound to its node")
	}
	if _, ok := h.m.parseSnapshotID(id+"0", "nodeA"); ok {
		t.Fatal("a tampered id must fail")
	}
	h.m.now = func() time.Time { return now.Add(SnapshotTTL + time.Minute) }
	if _, ok := h.m.parseSnapshotID(id, "nodeA"); ok {
		t.Fatal("an id expires after 24 h")
	}
	h.m.now = time.Now
	// over HTTP: a bad id is 410 sync_snapshot_expired
	tok := a.token(t)
	req, _ := http.NewRequest("GET", h.srv.URL+proto.PathSnapshot+"?id=bad&collection="+h.items.Id, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatalf("status %d", res.StatusCode)
	}
	_ = b
}

func TestSnapshotPartitionAndNoTombstoneLeak(t *testing.T) {
	t.Setenv(client.EnvSnapshotPage, "5")
	h := newTicketHub(t)
	var inB []string
	for i := 0; i < 12; i++ {
		h.ticket(t, "a"+strconv.Itoa(i), "A")
		inB = append(inB, h.ticket(t, "b"+strconv.Itoa(i), "B").Id)
	}
	// a delete in the other partition must not reach branch A (no id, clock or origin)
	if err := h.app.Delete(h.ticket(t, "gone", "B")); err != nil {
		t.Fatal(err)
	}
	n := newTicketSpoke(t, h, "gate-a", "A")
	if err := n.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, n.app, "SELECT COUNT(*) FROM tickets", nil); got != 12 {
		t.Fatalf("branch A must hold its 12 tickets, got %d", got)
	}
	if got := countRows(t, n.app, "SELECT COUNT(*) FROM tickets WHERE branch='B'", nil); got != 0 {
		t.Fatal("no ticket of branch B may travel")
	}
	if got := countRows(t, n.app, "SELECT COUNT(*) FROM _sync_tombstones", nil); got != 0 {
		t.Fatalf("no tombstone of a partitioned collection may travel, got %d", got)
	}
	for _, id := range inB {
		if n.has(id) != nil {
			t.Fatal("leak")
		}
	}
}

func TestBootstrapRebasesLocalChangesAndOrphansOldOnes(t *testing.T) {
	t.Setenv(client.EnvRetention, "1h")
	h, a, b := hubFixture(t)
	r := h.create(t, map[string]any{"title": "shared", "qty": 1})
	old := h.create(t, map[string]any{"title": "will be old"})
	a.sync(t)
	b.sync(t)

	// offline work on a: an update, a counter increment, a create, and an edit that is "too old"
	ra, _ := a.app.FindRecordById("items", r.Id)
	ra.Set("title", "edited offline")
	ra.Set("qty+", 5)
	if err := a.app.Save(ra); err != nil {
		t.Fatal(err)
	}
	n := a.create(t, map[string]any{"title": "created offline"})
	oa, _ := a.app.FindRecordById("items", old.Id)
	oa.Set("title", "ancient edit")
	if err := a.app.Save(oa); err != nil {
		t.Fatal(err)
	}
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _changes SET created={:t} WHERE record={:r} AND status='local'").
		Bind(dbx.Params{"t": time.Now().UTC().Add(-3 * time.Hour).Format("2006-01-02 15:04:05.000Z"), "r": old.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	// meanwhile the hub moves on
	hr := hubItem(t, h, r.Id)
	hr.Set("note", "hub note")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}

	if err := client.ScheduleRebootstrap(a.app, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	// the local edit survives the replacement of the data ...
	got, _ := a.app.FindRecordById("items", r.Id)
	if got.GetString("title") != "edited offline" || got.GetFloat("qty") != 6 {
		t.Fatalf("rebased record %v", got.FieldsData())
	}
	// ... as a NEW local change; the parked rows became fillers
	if cnt := countRows(t, a.app, "SELECT COUNT(*) FROM _changes WHERE status='rebase'", nil); cnt != 0 {
		t.Fatalf("%d parked rows left", cnt)
	}
	if cnt := countRows(t, a.app, "SELECT COUNT(*) FROM _changes WHERE code='rebased' AND status='local'", nil); cnt != 3 {
		t.Fatalf("want 3 fillers, got %d", cnt)
	}
	// the too-old edit is an orphaned conflict and was not re-applied
	if cnt := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_conflicts WHERE kind='orphaned' AND status='open' AND record={:r}", dbx.Params{"r": old.Id}); cnt != 1 {
		t.Fatalf("orphaned conflicts: %d", cnt)
	}
	oa, _ = a.app.FindRecordById("items", old.Id)
	if oa.GetString("title") != "will be old" {
		t.Fatalf("the orphaned edit must not apply: %q", oa.GetString("title"))
	}
	// pushing works (the fillers keep the hub's sequence contiguous) and everyone converges
	a.sync(t)
	b.sync(t)
	a.sync(t)
	requireConverged(t, h, a, b)
	hr = hubItem(t, h, r.Id)
	if hr.GetString("title") != "edited offline" || hr.GetFloat("qty") != 6 || hr.GetString("note") != "hub note" {
		t.Fatalf("hub record %v", hr.FieldsData())
	}
	if _, err := h.app.FindRecordById("items", n.Id); err != nil {
		t.Fatal("the offline create must reach the hub")
	}
}

func TestHubMarksANodeForRebootstrapAndReactivatesIt(t *testing.T) {
	h, a, b := hubFixture(t)
	h.create(t, map[string]any{"title": "x"})
	a.sync(t)
	b.sync(t)
	if _, err := MarkRebootstrap(h.app, "gate-2"); err != nil {
		t.Fatal(err)
	}
	hs, err := b.c.Handshake(ctxb)
	if err != nil || !hs.Rebootstrap {
		t.Fatalf("handshake %+v %v", hs, err)
	}
	if r := b.c.RunOnce(ctxb); r.Err != client.ErrRebootstrap {
		t.Fatalf("RunOnce %v", r.Err)
	}
	if err := b.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	if st := nodeStatusOf(t, h, "gate-2"); st != NodeActive {
		t.Fatalf("status %q", st)
	}
	if nodePulled(t, h, "gate-2") != cursorOf(t, b.spokeEnv).PullAfter {
		t.Fatalf("pulled_seq %d, cursor %d", nodePulled(t, h, "gate-2"), cursorOf(t, b.spokeEnv).PullAfter)
	}
	b.sync(t)
	requireConverged(t, h, a, b)
	if _, err := MarkRebootstrap(h.app, "nobody"); err == nil {
		t.Fatal("an unknown node must be refused")
	}
}

func TestHubEpochChangesOnRestoreMarkerPromoteAndRegressedHead(t *testing.T) {
	h := newHub(t)
	e0 := h.m.Epoch()
	if err := h.m.Init(); err != nil || h.m.Epoch() != e0 {
		t.Fatalf("a plain restart keeps the epoch: %v %q", err, h.m.Epoch())
	}
	// restore: the hook leaves a marker that survives the swap and the next boot renews the epoch
	var excluded bool
	ev := &kernel.BackupEvent{App: h.app, Context: ctxb, Name: "b.zip"}
	err := h.app.OnBackupRestore().Trigger(ev, func(e *kernel.BackupEvent) error {
		excluded = false
		for _, x := range e.Exclude {
			excluded = excluded || x == RestoreMarker
		}
		if _, err := os.Stat(filepath.Join(h.app.DataDir(), RestoreMarker)); err != nil {
			t.Fatalf("marker missing: %v", err)
		}
		return nil
	})
	if err != nil || !excluded {
		t.Fatalf("restore hook: %v excluded=%v", err, excluded)
	}
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	e1 := h.m.Epoch()
	if e1 == e0 {
		t.Fatal("restore must change the epoch")
	}
	if _, err := os.Stat(filepath.Join(h.app.DataDir(), RestoreMarker)); err == nil {
		t.Fatal("the marker must be consumed")
	}
	// a failed restore leaves no marker
	err = h.app.OnBackupRestore().Trigger(&kernel.BackupEvent{App: h.app, Context: ctxb, Name: "b.zip"}, func(*kernel.BackupEvent) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("error expected")
	}
	if _, serr := os.Stat(filepath.Join(h.app.DataDir(), RestoreMarker)); serr == nil {
		t.Fatal("no marker after a failed restore")
	}
	// promote: a new .toki-promoted.json renews the epoch once
	if err := os.WriteFile(filepath.Join(h.app.DataDir(), promotedMarker), []byte(`{"promoted_at":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	e2 := h.m.Epoch()
	if e2 == e1 {
		t.Fatal("promote must change the epoch")
	}
	if err := h.m.Init(); err != nil || h.m.Epoch() != e2 {
		t.Fatal("the same promote marker must not renew the epoch again")
	}
	// the head is below the highest head ever seen
	if _, err := h.app.NonconcurrentDB().NewQuery("INSERT INTO _sync_state (key,value) VALUES ({:k},'999999') ON CONFLICT(key) DO UPDATE SET value='999999'").
		Bind(dbx.Params{"k": keyMaxSeqSeen}).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Init(); err != nil || h.m.Epoch() == e2 {
		t.Fatalf("a regressed head must renew the epoch: %v", err)
	}
}

func TestSpokeFollowsAHubEpochChange(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 5; i++ {
		h.create(t, map[string]any{"title": "n" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	before := cursorOf(t, a.spokeEnv)
	if before.HubEpoch != h.m.Epoch() || before.PullAfter == 0 {
		t.Fatalf("cursor %+v", before)
	}
	// the spoke applied hub seqs that the hub no longer has (restore to an older state)
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET pull_after=pull_after+40").Execute(); err != nil {
		t.Fatal(err)
	}
	head := h.m.headSeq()
	if err := os.WriteFile(filepath.Join(h.app.DataDir(), RestoreMarker), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	newEpoch := h.m.Epoch()
	// a client that already knows the hub gets the new epoch with the next handshake
	h.create(t, map[string]any{"title": "after restore"})
	a.c.Kick()
	a.sync(t)
	cur := cursorOf(t, a.spokeEnv)
	if cur.HubEpoch != newEpoch {
		t.Fatalf("epoch %q != %q", cur.HubEpoch, newEpoch)
	}
	if cur.PullAfter > h.m.headSeq() || cur.PullAfter < head {
		t.Fatalf("pull_after %d must be back at the hub's head (%d..%d)", cur.PullAfter, head, h.m.headSeq())
	}
	b.sync(t)
	a.sync(t)
	requireConverged(t, h, a, b)
}

func TestFillerAdvancesThePushSequence(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := a.create(t, map[string]any{"title": "one"})
	a.sync(t)
	// a filler in place of a discarded change, followed by a real one
	tok := a.token(t)
	nid := a.m.NodeID()
	st, resp, eb := rawPush(t, h, tok, pushReq(
		proto.PushChange{ID: nid + ":2", HLC: nowHLC(0, 1).String(), Collection: "items", Record: "x", Op: proto.OpFiller, Patch: []byte("{}")},
		pc(nid, 3, nowHLC(0, 2), 0, "items", r.Id, OpUpdate, map[string]any{"title": "two"}),
	))
	if st != 200 || resp.AckedThrough != 3 || len(resp.Results) != 2 {
		t.Fatalf("%d %+v %+v", st, resp, eb)
	}
	if hubItem(t, h, r.Id).GetString("title") != "two" {
		t.Fatal("the change after the filler must apply")
	}
}

func TestAutoHealRebootstrapsAfterTwoDigestMismatches(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 6; i++ {
		h.create(t, map[string]any{"title": "r" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
	// the metadata of one record drifts (a raw write the capture never saw)
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _sync_meta SET hash=randomblob(32) WHERE rowid=(SELECT MIN(rowid) FROM _sync_meta WHERE collection={:c})").
		Bind(dbx.Params{"c": h.items.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	if _, _, mm := digestOf(t, a.app, "items"); len(mm) == 0 {
		t.Fatal("the corruption must be visible")
	}
	a.c = a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Interval = 30 * time.Millisecond
		o.DigestInterval = time.Millisecond
		o.AutoHeal = true
		o.NoPoke = true
	})
	a.c.Start(ctxb)
	defer a.c.Stop(ctxb)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, _, mm := digestOf(t, a.app, "items"); len(mm) == 0 && cursorOf(t, a.spokeEnv).State == client.StateIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the auto-heal did not repair the node: %+v", cursorOf(t, a.spokeEnv))
		}
		time.Sleep(50 * time.Millisecond)
	}
	b.sync(t)
	requireConverged(t, h, a, b)
}
