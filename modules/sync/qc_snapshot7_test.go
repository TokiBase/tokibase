//go:build !no_sync

package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/router"
)

// QC of PR7 (review P7-1 .. P7-15): snapshot scope parity with the pull, epoch
// handling (§3.3), the bootstrap write guard, bounded auto-heal, compaction
// against a snapshot in progress, restore detection.

func execSQL(t *testing.T, h *hubEnv, q string, p dbx.Params) {
	t.Helper()
	if _, err := h.app.NonconcurrentDB().NewQuery(q).Bind(p).Execute(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// simulateRestore turns the hub back to `head`: the log above it is gone, the
// restore marker is written and the hub boots again (new epoch).
func simulateRestore(t *testing.T, h *hubEnv, head int64) {
	t.Helper()
	execSQL(t, h, "DELETE FROM _changes WHERE seq > {:h}", dbx.Params{"h": head})
	execSQL(t, h, "UPDATE sqlite_sequence SET seq={:h} WHERE name='_changes'", dbx.Params{"h": head})
	execSQL(t, h, "UPDATE _sync_nodes SET pulled_seq=MIN(COALESCE(pulled_seq,0),{:h})", dbx.Params{"h": head})
	restartWithMarker(t, h)
}

func restartWithMarker(t *testing.T, h *hubEnv) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.app.DataDir(), RestoreMarker), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
}

// hubCall is a raw authenticated call to the hub.
func hubCall(t *testing.T, h *hubEnv, tok, method, path string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, h.srv.URL+path, rd)
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

// ---- P7-1 ----------------------------------------------------------------

func TestSnapshotHonoursTheGlobalViewRuleDefault(t *testing.T) {
	t.Setenv(EnvPullViewRule, "1")
	t.Setenv(client.EnvSnapshotPage, "3")
	h, _, _ := actorHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("title != 'secret'"))
	var pub []string
	for i := 0; i < 5; i++ {
		h.create(t, map[string]any{"title": "secret"})
		pub = append(pub, h.create(t, map[string]any{"title": "public " + strconv.Itoa(i)}).Id)
	}
	// deleted records leave tombstones (id, clock, origin) that a node must not get
	// for a collection whose rows it may only partly see
	if err := h.app.Delete(h.create(t, map[string]any{"title": "secret"})); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Delete(h.create(t, map[string]any{"title": "gone"})); err != nil {
		t.Fatal(err)
	}
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: "gate-3", Profile: "edge", Actor: "users/" + h.usr.Id})
	if err != nil {
		t.Fatal(err)
	}
	n := newItemsSpokeWith(t, h, "gate-3", code)
	if err := n.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	if got := countWhere(t, n.app, "items", "title='secret'", nil); got != 0 {
		t.Fatalf("the snapshot sent %d rows the view rule hides from the node", got)
	}
	if got := countWhere(t, n.app, "items", "1=1", nil); got != len(pub) {
		t.Fatalf("the node must hold its %d visible rows, got %d", len(pub), got)
	}
	if got := countWhere(t, n.app, "_sync_tombstones", "1=1", nil); got != 0 {
		t.Fatalf("no tombstone of a view-rule collection may travel, got %d", got)
	}
}

func TestAckDigestSkipsViewRuleCollectionsOfTheEnvDefault(t *testing.T) {
	t.Setenv(EnvPullViewRule, "1")
	h, a, _ := actorHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("qty < 100"))
	h.create(t, map[string]any{"title": "visible", "qty": 5})
	h.create(t, map[string]any{"title": "hidden", "qty": 500})
	a.sync(t)
	digests, err := backend{a.m}.MetaDigests()
	if err != nil {
		t.Fatal(err)
	}
	ar, err := a.c.Ack(ctxb, cursorOf(t, a.spokeEnv).PullAfter, digests)
	if err != nil {
		t.Fatal(err)
	}
	if !ar.DigestChecked || len(ar.DigestMismatch) != 0 {
		t.Fatalf("a node that holds a subset must not be reported as different: %+v", ar)
	}
}

// ---- P7-2 / P7-7 -----------------------------------------------------------

func TestEpochChangeKeepsTheCheapPathWhenTheCursorIsSafe(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 4; i++ {
		h.create(t, map[string]any{"title": "n" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	e0 := cursorOf(t, a.spokeEnv).HubEpoch
	restartWithMarker(t, h) // the head did not move: the restored hub has everything a pulled
	a.c.ForceHandshake()
	hs, err := a.c.Handshake(ctxb)
	if err != nil || hs.Rebootstrap || hs.HubEpoch == e0 {
		t.Fatalf("handshake %+v %v", hs, err)
	}
	a.c.ForceHandshake()
	a.sync(t)
	if cur := cursorOf(t, a.spokeEnv); cur.HubEpoch != h.m.Epoch() || cur.State != client.StateIdle {
		t.Fatalf("cursor %+v", cur)
	}
	b.c.ForceHandshake()
	b.sync(t)
	requireConverged(t, h, a, b)
}

func TestHubWritesLostByARestoreAreReconciledByARebootstrap(t *testing.T) { // scenario A
	h, a, b := hubFixture(t)
	for i := 0; i < 3; i++ {
		h.create(t, map[string]any{"title": "kept " + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	head := h.m.headSeq()
	lost := h.create(t, map[string]any{"title": "written on the hub after the backup"})
	a.sync(t)
	b.sync(t)
	if a.has(lost.Id) == nil {
		t.Fatal("the spoke must have pulled the record")
	}
	// a change of a spoke that the hub acknowledged, then lost with the restore, is not lost for good
	mine := a.create(t, map[string]any{"title": "written on a spoke after the backup"})
	a.sync(t)
	b.sync(t)
	execSQL(t, h, "DELETE FROM items WHERE id={:i}", dbx.Params{"i": mine.Id})
	execSQL(t, h, "DELETE FROM _sync_meta WHERE record={:i}", dbx.Params{"i": mine.Id})
	execSQL(t, h, "UPDATE _sync_nodes SET pushed_origin_seq=pushed_origin_seq-1 WHERE name='gate-1'", nil)
	// the restore drops the record and its log entry ...
	execSQL(t, h, "DELETE FROM items WHERE id={:i}", dbx.Params{"i": lost.Id})
	execSQL(t, h, "DELETE FROM _sync_meta WHERE record={:i}", dbx.Params{"i": lost.Id})
	simulateRestore(t, h, head)
	// ... and the hub moves on past the cursors of the spokes
	for i := 0; i < 4; i++ {
		h.create(t, map[string]any{"title": "after restore " + strconv.Itoa(i)})
	}
	for _, s := range []*itemsSpoke{a, b} {
		s.c.ForceHandshake()
		if r := s.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
			t.Fatalf("a spoke that holds lost hub history must re-bootstrap, got %v", r.Err)
		}
		if err := s.c.Bootstrap(ctxb); err != nil {
			t.Fatal(err)
		}
		s.sync(t)
		if s.has(lost.Id) != nil {
			t.Fatal("the ghost record survived the re-bootstrap")
		}
	}
	a.sync(t)
	b.sync(t)
	if _, err := h.app.FindRecordById("items", mine.Id); err != nil {
		t.Fatal("the spoke's own change must be sent again after the re-bootstrap")
	}
	requireConverged(t, h, a, b)
}

func TestDoubleEpochChangeDoesNotSkipARange(t *testing.T) { // scenario C
	h, a, b := hubFixture(t)
	for i := 0; i < 3; i++ {
		h.create(t, map[string]any{"title": "n" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	h0 := h.m.headSeq()
	e0 := h.m.Epoch()
	restartWithMarker(t, h) // E1, epoch_seq = h0
	e1 := h.m.Epoch()
	for i := 0; i < 30; i++ {
		h.create(t, map[string]any{"title": "e1 " + strconv.Itoa(i)})
	}
	h1 := h.m.headSeq()
	restartWithMarker(t, h) // E2, epoch_seq = h1
	e2 := h.m.Epoch()
	for _, c := range []struct {
		epoch string
		after int64
		want  bool
	}{
		{e0, h0, false}, {e0, 0, false}, {e0, h0 + 5, true}, // inside E1's range: E1 seq h0+1.. differ from E0's
		{e1, h1, false}, {e1, h1 + 1, true},
		{e2, h1 + 100, false}, {"", h1 + 100, false}, {"unknown", 0, true},
	} {
		if got := h.m.epochRequiresRebootstrap(c.epoch, c.after); got != c.want {
			t.Fatalf("epoch %s after %d: rebootstrap=%v, want %v", c.epoch, c.after, got, c.want)
		}
	}
	// a client that is two epochs behind with a cursor inside the first range is sent away
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET pull_after={:p}").Bind(dbx.Params{"p": h0 + 5}).Execute(); err != nil {
		t.Fatal(err)
	}
	a.c.ForceHandshake()
	if r := a.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
		t.Fatalf("RunOnce %v", r.Err)
	}
	if err := a.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	// b stayed at h0: nothing it pulled was lost, it follows through both epochs
	b.c.ForceHandshake()
	b.sync(t)
	a.sync(t)
	if cur := cursorOf(t, b.spokeEnv); cur.HubEpoch != e2 {
		t.Fatalf("epoch %q != %q", cur.HubEpoch, e2)
	}
	requireConverged(t, h, a, b)
}

func TestRestoreOlderThanSpokeKeepRebootstrapsInsteadOfKeepingGhosts(t *testing.T) { // scenario B
	for _, compacted := range []bool{false, true} {
		t.Run("compacted="+strconv.FormatBool(compacted), func(t *testing.T) {
			h, a, b := hubFixture(t)
			h.create(t, map[string]any{"title": "base"})
			a.sync(t)
			b.sync(t)
			head := h.m.headSeq()
			c1 := a.create(t, map[string]any{"title": "written on the spoke"})
			a.sync(t) // pushed and acked
			b.sync(t)
			if compacted {
				t.Setenv(EnvSpokeKeep, "1ms")
				backdateChanges(t, a.app, 48*time.Hour)
				if _, err := a.m.Compact(ctxb); err != nil {
					t.Fatal(err)
				}
				if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes WHERE node={:n}", dbx.Params{"n": a.m.NodeID()}); n != 0 {
					t.Fatalf("the acked rows must be compacted, %d left", n)
				}
			}
			// the hub is restored from a backup that does not have the spoke's change;
			// the spoke never pulled beyond that backup
			execSQL(t, h, "DELETE FROM items WHERE id={:i}", dbx.Params{"i": c1.Id})
			execSQL(t, h, "DELETE FROM _sync_meta WHERE record={:i}", dbx.Params{"i": c1.Id})
			execSQL(t, h, "UPDATE _sync_nodes SET pushed_origin_seq=0 WHERE name='gate-1'", nil)
			simulateRestore(t, h, head)
			// a only pushed (its cursor is inside the backup), b pulled the lost record
			if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _sync_cursors SET pull_after=MIN(pull_after,{:p})").Bind(dbx.Params{"p": head}).Execute(); err != nil {
				t.Fatal(err)
			}
			a.c.ForceHandshake()
			b.c.ForceHandshake()
			if !compacted {
				a.sync(t) // the acked change is still kept: it is sent again
				if _, err := h.app.FindRecordById("items", c1.Id); err != nil {
					t.Fatal("the change must be re-pushed to the restored hub")
				}
			} else {
				if r := a.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
					t.Fatalf("a change the hub lost and the spoke can not resend must lead to a re-bootstrap, got %v", r.Err)
				}
				if err := a.c.Bootstrap(ctxb); err != nil {
					t.Fatal(err)
				}
			}
			// b holds a record from after the backup: it re-bootstraps
			if r := b.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
				t.Fatalf("b: %v", r.Err)
			}
			if err := b.c.Bootstrap(ctxb); err != nil {
				t.Fatal(err)
			}
			a.sync(t)
			b.sync(t)
			if compacted && (a.has(c1.Id) != nil || b.has(c1.Id) != nil) {
				t.Fatal("no node may keep a record the hub does not have")
			}
			requireConverged(t, h, a, b)
		})
	}
}

func TestMaxSeqSeenLivesOutsideTheDatabaseAndFollowsPushes(t *testing.T) {
	h, a, _ := hubFixture(t)
	for i := 0; i < 6; i++ {
		h.create(t, map[string]any{"title": "n" + strconv.Itoa(i)}) // hub-local writes: noteHead is not called
	}
	a.create(t, map[string]any{"title": "pushed"})
	a.sync(t) // a push and a pull note the head
	head := h.m.headSeq()
	var seen int64
	if err := h.app.DB().NewQuery("SELECT CAST(value AS INTEGER) FROM _sync_state WHERE key='max_seq_seen'").Row(&seen); err != nil || seen != head {
		t.Fatalf("max_seq_seen %d, head %d (%v)", seen, head, err)
	}
	var side sidecar
	b, err := os.ReadFile(filepath.Join(h.app.DataDir(), stateSidecar))
	if err != nil || json.Unmarshal(b, &side) != nil || side.MaxSeqSeen != head {
		t.Fatalf("sidecar %s %v (head %d)", b, err, head)
	}
	// data.db is swapped for an older copy behind the module's back: the database knows
	// nothing of the later head, the file next to it does
	execSQL(t, h, "DELETE FROM _changes WHERE seq > {:h}", dbx.Params{"h": head - 4})
	execSQL(t, h, "UPDATE sqlite_sequence SET seq={:h} WHERE name='_changes'", dbx.Params{"h": head - 4})
	execSQL(t, h, "UPDATE _sync_state SET value={:h} WHERE key='max_seq_seen'", dbx.Params{"h": head - 4})
	e := h.m.Epoch()
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	if h.m.Epoch() == e {
		t.Fatal("a head below the sidecar's high-water mark must renew the epoch")
	}
}

func TestEpochHistoryIsKeptAcrossRestarts(t *testing.T) {
	h := newHub(t)
	e0 := h.m.Epoch()
	restartWithMarker(t, h)
	e1 := h.m.Epoch()
	if err := h.m.Init(); err != nil {
		t.Fatal(err)
	}
	if len(h.m.hub.epochHist) != 2 || h.m.hub.epochHist[0].Epoch != e0 || h.m.hub.epochHist[1].Epoch != e1 {
		t.Fatalf("history %+v", h.m.hub.epochHist)
	}
}

// ---- P7-3 ------------------------------------------------------------------

func TestLocalWritesAreRefusedWhileBootstrapping(t *testing.T) {
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
		t.Fatal("the bootstrap must stop when the network goes down")
	}
	if cur := cursorOf(t, s.spokeEnv); cur.State != client.StateBootstrapping {
		t.Fatalf("state %q", cur.State)
	}
	col := s.coll()
	rec := core.NewRecord(col)
	rec.Set("title", "typed during the bootstrap")
	err := s.app.Save(rec)
	var ae *router.ApiError
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || !strings.Contains(ae.Message, CodeBootstrapping) {
		t.Fatalf("a local write must get 503 sync_bootstrapping, got %v", err)
	}
	if n := countRows(t, s.app, "SELECT COUNT(*) FROM items WHERE title='typed during the bootstrap'", nil); n != 0 {
		t.Fatal("the refused write left a row")
	}
	// reads serve what is there, unless the operator asks for the strict mode
	get := func() int {
		w := httptest.NewRecorder()
		s.handler(t).ServeHTTP(w, httptest.NewRequest("GET", "/api/collections/items/records", nil))
		return w.Code
	}
	if c := get(); c != http.StatusOK {
		t.Fatalf("reads are served by default, got %d", c)
	}
	t.Setenv(EnvBootstrapBlockReads, "1")
	if c := get(); c != http.StatusServiceUnavailable {
		t.Fatalf("TOKI_SYNC_BOOTSTRAP_BLOCK_READS=1 must refuse reads, got %d", c)
	}
	t.Setenv(EnvBootstrapBlockReads, "")

	s.c = s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
	})
	if err := s.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	s.create(t, map[string]any{"title": "after the bootstrap"})
	s.sync(t)
	a.sync(t)
	b.sync(t)
	s.sync(t)
	requireConverged(t, h, a, b, s)
}

func TestRebaseKeepsTheOriginalHLCOfAnEditNewerThanTheHub(t *testing.T) {
	h, a, b := hubFixture(t)
	r := h.create(t, map[string]any{"title": "t0"})
	a.sync(t)
	b.sync(t)
	hr := hubItem(t, h, r.Id)
	hr.Set("total", 7)
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	ra, _ := a.app.FindRecordById("items", r.Id)
	ra.Set("title", "offline edit")
	if err := a.app.Save(ra); err != nil {
		t.Fatal(err)
	}
	var orig int64
	if err := a.app.DB().NewQuery("SELECT hlc FROM _changes WHERE record={:r} AND status='local'").Bind(dbx.Params{"r": r.Id}).Row(&orig); err != nil {
		t.Fatal(err)
	}
	if err := client.ScheduleRebootstrap(a.app, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	got, _ := a.app.FindRecordById("items", r.Id)
	if got.GetString("title") != "offline edit" || got.GetFloat("total") != 7 {
		t.Fatalf("the edit is newer than the hub's: it applies, got %v", got.FieldsData())
	}
	var replayed, meta int64
	if err := a.app.DB().NewQuery("SELECT hlc FROM _changes WHERE record={:r} AND status='local' AND code!='rebased'").Bind(dbx.Params{"r": r.Id}).Row(&replayed); err != nil {
		t.Fatal(err)
	}
	if err := a.app.DB().NewQuery("SELECT hlc FROM _sync_meta WHERE record={:r}").Bind(dbx.Params{"r": r.Id}).Row(&meta); err != nil {
		t.Fatal(err)
	}
	if replayed != orig || meta != orig {
		t.Fatalf("the replay must keep the original HLC %d: change %d, record clock %d", orig, replayed, meta)
	}
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
	if hubItem(t, h, r.Id).GetString("title") != "offline edit" {
		t.Fatal("the edit must reach the hub")
	}
}

func TestRebaseDoesNotOverrideANewerHubEdit(t *testing.T) {
	h, a, b := hubFixture(t)
	r := h.create(t, map[string]any{"title": "t0", "qty": 1})
	a.sync(t)
	b.sync(t)
	ra, _ := a.app.FindRecordById("items", r.Id)
	ra.Set("title", "offline edit")
	ra.Set("qty+", 5)
	if err := a.app.Save(ra); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	hr := hubItem(t, h, r.Id)
	hr.Set("title", "newer hub edit")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	if err := client.ScheduleRebootstrap(a.app, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	got, _ := a.app.FindRecordById("items", r.Id)
	if got.GetString("title") != "newer hub edit" || got.GetFloat("qty") != 6 {
		t.Fatalf("the newer hub title must stay, the commutative counter applies: %v", got.FieldsData())
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_conflicts WHERE kind='orphaned' AND status='open' AND record={:r}", dbx.Params{"r": r.Id}); n != 1 {
		t.Fatalf("the lost title edit must be kept as a conflict, got %d", n)
	}
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
	if hr = hubItem(t, h, r.Id); hr.GetString("title") != "newer hub edit" || hr.GetFloat("qty") != 6 {
		t.Fatalf("hub %v", hr.FieldsData())
	}
}

// ---- P7-4 ------------------------------------------------------------------

func TestDigestIgnoresHubRowsWithoutMeta(t *testing.T) {
	h, _, _ := hubFixture(t)
	for i := 0; i < 5; i++ {
		h.create(t, map[string]any{"title": "r" + strconv.Itoa(i)})
	}
	old := h.create(t, map[string]any{"title": "from before sync was enabled"})
	execSQL(t, h, "DELETE FROM _sync_meta WHERE record={:i}", dbx.Params{"i": old.Id})
	n := newItemsSpoke(t, h, "gate-3")
	if err := n.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	if n.has(old.Id) == nil {
		t.Fatal("the row must arrive")
	}
	digests, err := backend{n.m}.MetaDigests()
	if err != nil {
		t.Fatal(err)
	}
	ar, err := n.c.Ack(ctxb, cursorOf(t, n.spokeEnv).PullAfter, digests)
	if err != nil || !ar.DigestChecked || len(ar.DigestMismatch) != 0 {
		t.Fatalf("a clean bootstrap must not look different: %+v %v", ar, err)
	}
}

func TestAutoHealStopsAfterTwoHealsInADay(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 4; i++ {
		h.create(t, map[string]any{"title": "r" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE _sync_meta SET hash=randomblob(32) WHERE collection={:c} AND record=(SELECT MIN(record) FROM _sync_meta WHERE collection={:c})").
		Bind(dbx.Params{"c": h.items.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	seed, _ := json.Marshal([]int64{now - 3600, now - 60})
	if _, err := a.app.NonconcurrentDB().NewQuery("INSERT INTO _sync_state (key, value) VALUES ('heal_log', {:v})").Bind(dbx.Params{"v": string(seed)}).Execute(); err != nil {
		t.Fatal(err)
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
	for a.c.Status().Heal != client.StatusHealExhausted {
		if time.Now().After(deadline) {
			t.Fatalf("the heal limit was not reported: %+v", a.c.Status())
		}
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	cur := cursorOf(t, a.spokeEnv)
	if cur.State != client.StateIdle || !strings.Contains(cur.LastError, client.StatusHealExhausted) {
		t.Fatalf("no re-bootstrap may follow the limit: %+v", cur)
	}
	if _, _, mm := digestOf(t, a.app, "items"); len(mm) == 0 {
		t.Fatal("the corruption must still be there: nothing healed it")
	}
}

// ---- P7-5 / P7-8 ------------------------------------------------------------

func TestCompactionKeepsTheLogOfASnapshotInProgress(t *testing.T) {
	h, a, b := hubFixture(t)
	for i := 0; i < 20; i++ {
		h.create(t, map[string]any{"title": "r" + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t)
	compactEverything(t, h)

	s := newItemsSpoke(t, h, "gate-3")
	tok := s.token(t)
	code, raw := hubCall(t, h, tok, "POST", proto.PathSnapshot, map[string]any{})
	var start proto.SnapshotStart
	if code != 200 || json.Unmarshal(raw, &start) != nil {
		t.Fatalf("snapshot start %d %s", code, raw)
	}
	for i := 0; i < 6; i++ {
		h.create(t, map[string]any{"title": "while the snapshot runs " + strconv.Itoa(i)})
	}
	a.sync(t)
	b.sync(t) // every active node has pulled everything: without the pin it would all go
	// the node is silent for longer than the retention, and the min-keep is over
	execSQL(t, h, "UPDATE _sync_nodes SET last_seen={:t} WHERE name='gate-3'", dbx.Params{"t": time.Now().UTC().Add(-200 * 24 * time.Hour).Format("2006-01-02 15:04:05.000Z")})
	t.Setenv(EnvMinKeep, "1ms")
	backdateChanges(t, h.app, 48*time.Hour)
	if _, err := h.m.Compact(ctxb); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE seq >= {:s}", dbx.Params{"s": start.StartSeq}); n < 6 {
		t.Fatalf("the log after start_seq must stay while the snapshot runs, %d rows left", n)
	}
	if st := nodeStatusOf(t, h, "gate-3"); st == NodeStale {
		t.Fatal("a node with a snapshot in progress must not be marked stale")
	}
	// the ack ends it; the log is free again
	code, raw = hubCall(t, h, tok, "POST", proto.PathAck, proto.AckRequest{PulledThrough: start.StartSeq, SnapshotID: start.SnapshotID})
	if code != 200 {
		t.Fatalf("ack %d %s", code, raw)
	}
	if st := nodeStatusOf(t, h, "gate-3"); st != NodeActive || nodePulled(t, h, "gate-3") != start.StartSeq {
		t.Fatalf("the ack must reactivate the node at start_seq: %q %d", st, nodePulled(t, h, "gate-3"))
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _sync_state WHERE key LIKE 'snap:%'", nil); n != 0 {
		t.Fatalf("the pin must be gone after the ack, %d left", n)
	}
}

func TestAckOfAnInvalidOrVoidSnapshotIdIsRefused(t *testing.T) {
	h, a, b := hubFixture(t)
	h.create(t, map[string]any{"title": "x"})
	a.sync(t)
	b.sync(t)
	tok := b.token(t)
	code, raw := hubCall(t, h, tok, "POST", proto.PathAck, proto.AckRequest{PulledThrough: 0, SnapshotID: "nope.nope"})
	if code != http.StatusGone || !strings.Contains(string(raw), proto.CodeSnapshotExpired) {
		t.Fatalf("a bogus id must get 410: %d %s", code, raw)
	}
	code, raw = hubCall(t, h, tok, "POST", proto.PathSnapshot, map[string]any{})
	var start proto.SnapshotStart
	if code != 200 || json.Unmarshal(raw, &start) != nil {
		t.Fatalf("start %d %s", code, raw)
	}
	// the operator marks the node again: the id issued before is void
	if _, err := MarkRebootstrap(h.app, "gate-2"); err != nil {
		t.Fatal(err)
	}
	code, raw = hubCall(t, h, tok, "POST", proto.PathAck, proto.AckRequest{PulledThrough: start.StartSeq, SnapshotID: start.SnapshotID})
	if code != http.StatusGone {
		t.Fatalf("a void id must get 410: %d %s", code, raw)
	}
	if st := nodeStatusOf(t, h, "gate-2"); st != NodeRebootstrap {
		t.Fatalf("the operator's mark must stay, status %q", st)
	}
}

// ---- P7-6 ------------------------------------------------------------------

func TestSnapshotPageScanIsCapped(t *testing.T) {
	t.Setenv(EnvPullViewRule, "1")
	t.Setenv(client.EnvSnapshotPage, "50")
	old := snapshotScanCap
	snapshotScanCap = 7
	t.Cleanup(func() { snapshotScanCap = old })
	h, _, _ := actorHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("title != 'secret'"))
	for i := 0; i < 40; i++ {
		h.create(t, map[string]any{"title": "secret"})
	}
	var vis []string
	for i := 0; i < 3; i++ {
		vis = append(vis, h.create(t, map[string]any{"title": "public " + strconv.Itoa(i)}).Id)
	}
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: "gate-3", Profile: "edge", Actor: "users/" + h.usr.Id})
	if err != nil {
		t.Fatal(err)
	}
	n := newItemsSpokeWith(t, h, "gate-3", code)
	pages := 0
	n.c = n.client(t, h, func(o *client.Options) {
		o.Backend = backend{n.m}
		o.Interval = time.Hour
		o.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, proto.PathSnapshot) {
				pages++
			}
			return http.DefaultTransport.RoundTrip(r)
		})}
	})
	if err := n.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	for _, id := range vis {
		if n.has(id) == nil {
			t.Fatal("a visible record is missing")
		}
	}
	if countWhere(t, n.app, "items", "1=1", nil) != 3 {
		t.Fatal("only the visible rows may arrive")
	}
	if pages < 6 {
		t.Fatalf("43 rows with a cap of 7 per request need several pages, got %d requests", pages)
	}
}

// ---- P7-9 / P7-14 -------------------------------------------------------------

func TestSnapshotFieldClocksOnlyForFieldsThatTravel(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, nil, []string{"note"})
	if _, err := SetPolicy(h.app, "items", PolicyChange{Strategy: strp(StratFieldMerge)}); err != nil {
		t.Fatal(err)
	}
	r := h.create(t, map[string]any{"title": "t", "note": "private"})
	execSQL(t, h, "UPDATE _sync_meta SET fields={:f} WHERE record={:i}",
		dbx.Params{"f": `{"title":"` + nowHLC(0, 1).String() + `","note":"` + nowHLC(0, 2).String() + `"}`, "i": r.Id})
	s := newItemsSpoke(t, h, "gate-3")
	tok := s.token(t)
	_, raw := hubCall(t, h, tok, "POST", proto.PathSnapshot, map[string]any{})
	var start proto.SnapshotStart
	if err := json.Unmarshal(raw, &start); err != nil {
		t.Fatal(err)
	}
	code, raw := hubCall(t, h, tok, "GET", proto.PathSnapshot+"?id="+start.SnapshotID+"&collection="+h.items.Id, nil)
	var page proto.SnapshotPage
	if code != 200 || json.Unmarshal(raw, &page) != nil || len(page.Records) == 0 {
		t.Fatalf("page %d %s", code, raw)
	}
	for _, rec := range page.Records {
		if _, leak := rec.Fields["note"]; leak {
			t.Fatalf("the clock of an excluded field leaked: %v", rec.Fields)
		}
	}
}

func TestPushDegradesTheFillerForAHubWithoutTheCapability(t *testing.T) {
	var mu sync.Mutex
	var ops []string
	strip := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil && r.URL.Path == proto.PathPush {
			body, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		res, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			return res, err
		}
		if r.URL.Path == proto.PathHandshake {
			raw, _ := io.ReadAll(res.Body)
			var m map[string]any
			if json.Unmarshal(raw, &m) == nil {
				delete(m, "caps") // an older hub
				raw, _ = json.Marshal(m)
			}
			res.Body = io.NopCloser(bytes.NewReader(raw))
			res.ContentLength = int64(len(raw))
			res.Header.Del("Content-Length")
		}
		if len(body) > 0 {
			var pr proto.PushRequest
			if json.Unmarshal(body, &pr) == nil {
				mu.Lock()
				for _, c := range pr.Changes {
					ops = append(ops, c.Op+":"+string(c.Patch))
				}
				mu.Unlock()
			}
		}
		return res, nil
	})
	h, a, b := hubFixture(t)
	r := h.create(t, map[string]any{"title": "t0"})
	a.sync(t)
	b.sync(t)
	ra, _ := a.app.FindRecordById("items", r.Id)
	ra.Set("title", "offline")
	if err := a.app.Save(ra); err != nil {
		t.Fatal(err)
	}
	a.c = a.client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Interval = time.Hour
		o.HTTP = &http.Client{Transport: strip}
	})
	if err := client.ScheduleRebootstrap(a.app, "test"); err != nil {
		t.Fatal(err)
	}
	if err := a.c.Bootstrap(ctxb); err != nil {
		t.Fatal(err)
	}
	_ = a.c.RunOnce(ctxb)
	mu.Lock()
	defer mu.Unlock()
	for _, o := range ops {
		if strings.HasPrefix(o, proto.OpFiller+":") {
			t.Fatalf("op %q sent to a hub that does not advertise it: %v", proto.OpFiller, ops)
		}
	}
	if len(ops) == 0 {
		t.Fatal("the rebased change was not pushed")
	}
}

// ---- P7-10 / P7-15 -----------------------------------------------------------

func TestPlainCycleRefusesWhileABootstrapIsPending(t *testing.T) {
	h, a, b := hubFixture(t)
	h.create(t, map[string]any{"title": "x"})
	a.sync(t)
	b.sync(t)
	if err := client.ScheduleRebootstrap(a.app, "test"); err != nil {
		t.Fatal(err)
	}
	before := cursorOf(t, a.spokeEnv).PullAfter
	h.create(t, map[string]any{"title": "y"})
	if r := a.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
		t.Fatalf("RunOnce %v", r.Err)
	}
	if r := a.c.PullOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
		t.Fatalf("PullOnce %v", r.Err)
	}
	if cursorOf(t, a.spokeEnv).PullAfter != before {
		t.Fatal("no log entry may be applied while a bootstrap is pending")
	}
}

func TestFillerBeyondThePushedSequenceIsRefused(t *testing.T) {
	h, a, _ := hubFixture(t)
	a.create(t, map[string]any{"title": "one"})
	a.sync(t)
	tok := a.token(t)
	nid := a.m.NodeID()
	st, _, eb := rawPush(t, h, tok, pushReq(
		proto.PushChange{ID: nid + ":9", HLC: nowHLC(0, 1).String(), Collection: "items", Record: "x", Op: proto.OpFiller, Patch: []byte("{}")},
	))
	if st != http.StatusConflict {
		t.Fatalf("a filler that skips a sequence must be refused (push gap), got %d %+v", st, eb)
	}
}
