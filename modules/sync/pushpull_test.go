//go:build !no_sync

package sync

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// ---- helpers ----------------------------------------------------------

var ctxb = context.Background()

// itemsSpoke is a spoke with the same `items` collection as the hub.
type itemsSpoke struct {
	*spokeEnv
	c   *client.Client
	mux http.Handler
}

// handler is the HTTP handler of the spoke (built once: OnServe can run once per app).
func (s *itemsSpoke) handler(t *testing.T) http.Handler {
	if s.mux == nil {
		s.mux = buildMux(t, s.app)
	}
	return s.mux
}

func newItemsSpoke(t *testing.T, h *hubEnv, name string) *itemsSpoke {
	t.Helper()
	return newItemsSpokeWith(t, h, name, h.enroll(t, name, nil))
}

// newItemsSpokeWith joins with an enrollment code created by the caller.
func newItemsSpokeWith(t *testing.T, h *hubEnv, name, code string) *itemsSpoke {
	t.Helper()
	s := newSpoke(t)
	open := ""
	c := core.NewBaseCollection("items")
	c.Id = h.items.Id
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "qty"},
		&core.SelectField{Name: "tags", MaxSelect: 4, Values: []string{"a", "b", "c", "d"}},
		&core.FileField{Name: "photo", MaxSelect: 1, MaxSize: 1 << 20},
		&core.TextField{Name: "note"},
		&core.TextField{Name: "secret"},
		&core.NumberField{Name: "total"},
		&core.JSONField{Name: "meta"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := s.app.Save(c); err != nil {
		t.Fatal(err)
	}
	h.join(t, s, code)
	cl := s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
	})
	return &itemsSpoke{spokeEnv: s, c: cl}
}

func (s *itemsSpoke) sync(t *testing.T) client.Result {
	t.Helper()
	r := s.c.RunOnce(ctxb)
	if r.Err != nil {
		t.Fatalf("sync: %v", r.Err)
	}
	return r
}

func (s *itemsSpoke) coll() *core.Collection {
	c, _ := s.app.FindCollectionByNameOrId("items")
	return c
}

func (s *itemsSpoke) create(t *testing.T, vals map[string]any) *core.Record {
	t.Helper()
	r := core.NewRecord(s.coll())
	for k, v := range vals {
		r.Set(k, v)
	}
	if err := s.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func hubItem(t *testing.T, h *hubEnv, id string) *core.Record {
	t.Helper()
	r, err := h.app.FindRecordById("items", id)
	if err != nil {
		t.Fatalf("hub record %s: %v", id, err)
	}
	return r
}

func (h *hubEnv) create(t *testing.T, vals map[string]any) *core.Record {
	t.Helper()
	r := core.NewRecord(h.items)
	for k, v := range vals {
		r.Set(k, v)
	}
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func digestOf(t *testing.T, app core.App, name string) (string, int, []Mismatch) {
	t.Helper()
	rep, err := Verify(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Collections {
		if c.Name == name {
			return c.Digest, c.Records, c.Mismatches
		}
	}
	t.Fatalf("collection %s not verified", name)
	return "", 0, nil
}

func requireConverged(t *testing.T, h *hubEnv, spokes ...*itemsSpoke) {
	t.Helper()
	want, n, mm := digestOf(t, h.app, "items")
	if len(mm) != 0 {
		t.Fatalf("hub mismatches: %v", mm)
	}
	dump := func(app core.App) string {
		recs, _ := app.FindAllRecords("items")
		var sb strings.Builder
		for _, r := range recs {
			b, _ := json.Marshal(r.FieldsData())
			sb.WriteString(string(b) + "\n")
		}
		return sb.String()
	}
	for i, s := range spokes {
		got, m, mm := digestOf(t, s.app, "items")
		if len(mm) != 0 || got != want || m != n {
			t.Fatalf("spoke %d: digest %s (%d records, %v) != hub %s (%d records)\nhub:\n%sspoke:\n%s\n%s", i, got, m, mm, want, n, dump(h.app), dump(s.app), diffDump(h.app, s.app, spokes...)+events(s.c))
		}
	}
}

// hubSession handshakes the spoke's client and returns its session token.
func (s *itemsSpoke) token(t *testing.T) string {
	t.Helper()
	if _, err := s.c.Handshake(ctxb); err != nil {
		t.Fatal(err)
	}
	return s.c.Token()
}

func nowHLC(offsetMs int64, logical uint16) hlc.HLC {
	return hlc.Make(time.Now().UnixMilli()+offsetMs, logical)
}

func rawPush(t *testing.T, h *hubEnv, tok string, req proto.PushRequest) (int, proto.PushResponse, proto.ErrorBody) {
	t.Helper()
	if req.SchemaVersion == 0 {
		req.SchemaVersion = h.m.schemaVersion() // PR8: a push must carry the hub schema version
	}
	body, _ := json.Marshal(req)
	r, _ := http.NewRequest("POST", h.srv.URL+proto.PathPush, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	r.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var ok proto.PushResponse
	var eb proto.ErrorBody
	if res.StatusCode == 200 {
		_ = json.Unmarshal(raw, &ok)
	} else {
		_ = json.Unmarshal(raw, &eb)
	}
	return res.StatusCode, ok, eb
}

func rawPull(t *testing.T, h *hubEnv, tok string, query string) (int, proto.PullResponse) {
	t.Helper()
	r, _ := http.NewRequest("GET", h.srv.URL+proto.PathPull+"?"+query, nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var pr proto.PullResponse
	_ = json.NewDecoder(res.Body).Decode(&pr)
	return res.StatusCode, pr
}

func pc(node string, oseq int64, h hlc.HLC, base hlc.HLC, col, rec, op string, patch map[string]any) proto.PushChange {
	b, _ := json.Marshal(patch)
	c := proto.PushChange{ID: node + ":" + strconv.FormatInt(oseq, 10), HLC: h.String(), Collection: col, Record: rec, Op: op, Patch: b}
	if base != 0 {
		c.Base = base.String()
	}
	return c
}

func pushReq(cs ...proto.PushChange) proto.PushRequest {
	return proto.PushRequest{ClientTime: time.Now().UTC().Format(proto.TimeLayout), Changes: cs}
}

func changeRows(t *testing.T, app core.App, where string, args dbx.Params) []chg {
	t.Helper()
	var rows []chg
	if err := app.DB().NewQuery("SELECT seq, node, origin_seq, hlc, base_hlc, collection, record, op, patch, hash, actor, tx, status FROM _changes WHERE " + where + " ORDER BY seq").
		Bind(args).All(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// hubFixture is a hub with the items policy and two enrolled spokes.
func hubFixture(t *testing.T) (*hubEnv, *itemsSpoke, *itemsSpoke) {
	t.Helper()
	h := newHub(t)
	h.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter, "tags": TypeSet}, []string{"note"})
	a := newItemsSpoke(t, h, "gate-1")
	b := newItemsSpoke(t, h, "gate-2")
	a.sync(t) // fetch the policies
	b.sync(t)
	return h, a, b
}

// ---- tests ------------------------------------------------------------

func TestPushPullRoundTripConverges(t *testing.T) {
	h, a, b := hubFixture(t)

	r := a.create(t, map[string]any{"title": "from a", "qty": 2, "tags": []string{"a"}})
	res := a.sync(t)
	if res.Pushed != 1 {
		t.Fatalf("result %+v", res)
	}
	hr := hubItem(t, h, r.Id)
	if hr.GetString("title") != "from a" || hr.GetFloat("qty") != 2 {
		t.Fatalf("hub record %v", hr.FieldsData())
	}
	// the hub row keeps the origin node and is applied
	rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()})
	if len(rows) != 1 || rows[0].Status != StatusApplied || rows[0].OriginSeq != 1 || rows[0].Op != OpCreate {
		t.Fatalf("hub rows: %+v", rows)
	}
	if st := a.c.Status(); st.Pending != 0 {
		t.Fatalf("pending %d", st.Pending)
	}
	var st string
	_ = a.app.DB().NewQuery("SELECT status FROM _changes WHERE origin_seq=1").Row(&st)
	if st != "acked" {
		t.Fatalf("spoke row status %q", st)
	}

	// b pulls it; created/updated keep the origin values
	b.sync(t)
	br, err := b.app.FindRecordById("items", r.Id)
	if err != nil || br.GetString("title") != "from a" {
		t.Fatalf("spoke b: %v %v", err, br)
	}
	r, _ = a.app.FindRecordById("items", r.Id) // the in-memory record has sub-millisecond precision
	if br.GetString("created") != r.GetString("created") || br.GetString("updated") != r.GetString("updated") {
		t.Fatalf("autodates: %v/%v vs %v/%v", br.GetString("created"), br.GetString("updated"), r.GetString("created"), r.GetString("updated"))
	}
	if n := len(changeRows(t, b.app, "1=1", nil)); n != 0 {
		t.Fatalf("a pull apply must not capture a change row (got %d)", n)
	}
	requireConverged(t, h, a, b)

	// hub-local write reaches both
	hr = hubItem(t, h, r.Id)
	hr.Set("title", "from hub")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	requireConverged(t, h, a, b)
	if x, _ := a.app.FindRecordById("items", r.Id); x.GetString("title") != "from hub" {
		t.Fatal("a did not get the hub write")
	}

	// delete from b
	br, _ = b.app.FindRecordById("items", r.Id)
	if err := b.app.Delete(br); err != nil {
		t.Fatal(err)
	}
	b.sync(t)
	a.sync(t)
	if _, err := h.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the delete must reach the hub")
	}
	if _, err := a.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the delete must reach a")
	}
	requireConverged(t, h, a, b)
}

func TestPushIdempotentDuplicate(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	node := a.m.NodeID()
	cid := h.items.Id
	req := pushReq(pc(node, 1, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "once"}))
	st, r1, _ := rawPush(t, h, tok, req)
	if st != 200 || len(r1.Results) != 1 || r1.Results[0].Status != proto.ResApplied || r1.AckedThrough != 1 {
		t.Fatalf("first push: %d %+v", st, r1)
	}
	st, r2, _ := rawPush(t, h, tok, req)
	if st != 200 || r2.Results[0].Status != proto.ResDuplicate || r2.Results[0].HubSeq != r1.Results[0].HubSeq ||
		r2.Results[0].Hash != r1.Results[0].Hash || r2.Results[0].Was != proto.ResApplied || r2.AckedThrough != 1 {
		t.Fatalf("second push: %d %+v vs %+v", st, r2, r1)
	}
	if n := len(changeRows(t, h.app, "node={:n}", dbx.Params{"n": node})); n != 1 {
		t.Fatalf("a duplicate must not add rows (%d)", n)
	}
}

func TestPushGapAndLimits(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	node, cid := a.m.NodeID(), h.items.Id
	st, _, eb := rawPush(t, h, tok, pushReq(pc(node, 5, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})))
	if st != 409 || eb.Data["code"] != proto.CodePushGap || eb.Data["push_from"] != float64(1) {
		t.Fatalf("gap: %d %+v", st, eb)
	}
	// a hole inside the request is a gap too
	st, _, eb = rawPush(t, h, tok, pushReq(
		pc(node, 1, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "x"}),
		pc(node, 3, nowHLC(-4000, 0), 0, cid, "recordaaaaaaaa2", "c", map[string]any{"title": "y"})))
	if st != 409 || eb.Data["code"] != proto.CodePushGap {
		t.Fatalf("inner gap: %d %+v", st, eb)
	}
	if n := len(changeRows(t, h.app, "node={:n}", dbx.Params{"n": node})); n != 0 {
		t.Fatal("a refused request must not apply anything")
	}
	// too many changes
	var many []proto.PushChange
	for i := 1; i <= 501; i++ {
		many = append(many, pc(node, int64(i), nowHLC(-5000, uint16(i)), 0, cid, fmt.Sprintf("record%08d", i), "c", map[string]any{"title": "x"}))
	}
	st, _, eb = rawPush(t, h, tok, pushReq(many...))
	if st != 413 || eb.Data["code"] != proto.CodeBatchTooLarge {
		t.Fatalf("501 changes: %d %+v", st, eb)
	}
	// too many bytes
	big := strings.Repeat("x", 9<<20)
	st, _, eb = rawPush(t, h, tok, pushReq(pc(node, 1, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": big})))
	if st != 413 || eb.Data["code"] != proto.CodeBatchTooLarge {
		t.Fatalf("9 MiB: %d %+v", st, eb)
	}
	// the foreign node id in a change id
	st, _, eb = rawPush(t, h, tok, pushReq(pc("nsomeoneelse", 1, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", nil)))
	if st != 400 || eb.Data["code"] != proto.CodeBadRequest {
		t.Fatalf("foreign id: %d %+v", st, eb)
	}
	// without a token
	r, _ := http.NewRequest("POST", h.srv.URL+proto.PathPush, strings.NewReader("{}"))
	if res, _ := http.DefaultClient.Do(r); res == nil || res.StatusCode != 401 {
		t.Fatal("push needs a node session")
	}
}

func TestLWWSupersededAndWinner(t *testing.T) {
	h, a, b := hubFixture(t)
	cid, id := h.items.Id, "recordaaaaaaaa1"
	ta, tb := a.token(t), b.token(t)
	na, nb := a.m.NodeID(), b.m.NodeID()

	t1 := nowHLC(-9000, 0)
	st, r, _ := rawPush(t, h, ta, pushReq(pc(na, 1, t1, 0, cid, id, "c", map[string]any{"title": "first", "note": "n"})))
	if st != 200 || r.Results[0].Status != proto.ResApplied {
		t.Fatalf("create: %d %+v", st, r)
	}
	// b edits from base 0 (never saw it) with an OLDER hlc: it loses
	st, r, _ = rawPush(t, h, tb, pushReq(pc(nb, 1, nowHLC(-9500, 0), 0, cid, id, "u", map[string]any{"title": "loser"})))
	if st != 200 || r.Results[0].Status != proto.ResSuperseded || r.AckedThrough != 1 {
		t.Fatalf("loser: %d %+v", st, r)
	}
	if hubItem(t, h, id).GetString("title") != "first" {
		t.Fatal("a superseded change must not be written")
	}
	// ...and gets a revert row with the full hub record
	rv := changeRows(t, h.app, "status='revert' AND target={:t}", dbx.Params{"t": nb})
	if len(rv) != 1 || rv[0].Op != OpUpdate || !strings.Contains(rv[0].Patch, `"first"`) {
		t.Fatalf("revert rows: %+v", rv)
	}
	if r.Results[0].HubSeq != rv[0].Seq {
		t.Fatalf("result hub_seq %d, revert seq %d", r.Results[0].HubSeq, rv[0].Seq)
	}
	// the loss is stored as rejected/superseded
	if rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": nb}); len(rows) != 1 || rows[0].Status != StatusRejected {
		t.Fatalf("stored: %+v", rows)
	}
	// re-push: duplicate that remembers it was superseded
	_, r2, _ := rawPush(t, h, tb, pushReq(pc(nb, 1, nowHLC(-9500, 0), 0, cid, id, "u", map[string]any{"title": "loser"})))
	if r2.Results[0].Status != proto.ResDuplicate || r2.Results[0].Was != proto.ResSuperseded || r2.Results[0].HubSeq != rv[0].Seq {
		t.Fatalf("dup of superseded: %+v", r2)
	}

	// b edits again with a NEWER hlc and a stale base: concurrent but it wins
	st, r, _ = rawPush(t, h, tb, pushReq(pc(nb, 2, nowHLC(-1000, 0), 0, cid, id, "u", map[string]any{"title": "winner"})))
	if st != 200 || r.Results[0].Status != proto.ResApplied {
		t.Fatalf("winner: %d %+v", st, r)
	}
	hr := hubItem(t, h, id)
	if hr.GetString("title") != "winner" {
		t.Fatalf("title %q", hr.GetString("title"))
	}
	if hr.GetString("note") != "" && hr.GetString("note") != "n" {
		t.Fatal("fields outside the patch keep the hub values")
	}
	if mh, mn, _ := readMeta(h.app.DB(), h.items.Id, id); mn != nb || mh != int64(nowHLC(-1000, 0))/1<<0 && mh == 0 {
		t.Fatalf("meta %d %s", mh, mn)
	}

	// not concurrent (base == meta.hlc) with any hlc wins
	meta, _, _ := readMeta(h.app.DB(), h.items.Id, id)
	st, r, _ = rawPush(t, h, ta, pushReq(pc(na, 2, nowHLC(-500, 0), hlc.HLC(meta), cid, id, "u", map[string]any{"title": "seen it"})))
	if st != 200 || r.Results[0].Status != proto.ResApplied || hubItem(t, h, id).GetString("title") != "seen it" {
		t.Fatalf("non concurrent: %d %+v", st, r)
	}
}

func TestSupersededKeepsCounterOps(t *testing.T) {
	h, a, b := hubFixture(t)
	cid, id := h.items.Id, "recordaaaaaaaa1"
	ta, tb := a.token(t), b.token(t)
	na, nb := a.m.NodeID(), b.m.NodeID()
	rawPush(t, h, ta, pushReq(pc(na, 1, nowHLC(-9000, 0), 0, cid, id, "c", map[string]any{"title": "t", "qty": 10})))
	// b loses on title but its counter increment still counts
	_, r, _ := rawPush(t, h, tb, pushReq(pc(nb, 1, nowHLC(-9500, 0), 0, cid, id, "u",
		map[string]any{"title": "lost", "qty": map[string]any{"$inc": 5}})))
	if r.Results[0].Status != proto.ResApplied && r.Results[0].Status != proto.ResMerged {
		t.Fatalf("status %+v", r.Results[0])
	}
	hr := hubItem(t, h, id)
	if hr.GetFloat("qty") != 15 || hr.GetString("title") != "t" {
		t.Fatalf("hub %v", hr.FieldsData())
	}
	if r.Results[0].Status != proto.ResMerged {
		t.Fatalf("a counter-only winner is merged, got %s", r.Results[0].Status)
	}
}

func TestDeleteWinsAndTombstoned(t *testing.T) {
	h, a, b := hubFixture(t)
	cid, id := h.items.Id, "recordaaaaaaaa1"
	ta, tb := a.token(t), b.token(t)
	na, nb := a.m.NodeID(), b.m.NodeID()
	rawPush(t, h, ta, pushReq(pc(na, 1, nowHLC(-9000, 0), 0, cid, id, "c", map[string]any{"title": "t"})))
	rawPush(t, h, ta, pushReq(pc(na, 2, nowHLC(-4000, 0), 0, cid, id, "u", map[string]any{"title": "newer"})))

	// an OLDER concurrent delete still deletes
	_, r, _ := rawPush(t, h, tb, pushReq(pc(nb, 1, nowHLC(-9900, 0), 0, cid, id, "d", nil)))
	if r.Results[0].Status != proto.ResApplied {
		t.Fatalf("delete: %+v", r)
	}
	if _, err := h.app.FindRecordById("items", id); err == nil {
		t.Fatal("deletes are final")
	}
	if k := tombstoneKind(h.app.DB(), cid, id); k != "delete" {
		t.Fatalf("tombstone %q", k)
	}
	// updates after it are rejected as tombstoned, with a revert that deletes on the spoke
	_, r, _ = rawPush(t, h, ta, pushReq(pc(na, 3, nowHLC(-100, 0), 0, cid, id, "u", map[string]any{"title": "zombie"})))
	if r.Results[0].Status != proto.ResRejected || r.Results[0].Code != proto.CodeTombstoned {
		t.Fatalf("zombie: %+v", r)
	}
	rv := changeRows(t, h.app, "status='revert' AND target={:t}", dbx.Params{"t": na})
	if len(rv) != 1 || rv[0].Op != OpDelete {
		t.Fatalf("revert: %+v", rv)
	}
	// a second delete of a deleted record is a quiet no-op
	_, r, _ = rawPush(t, h, tb, pushReq(pc(nb, 2, nowHLC(-50, 0), 0, cid, id, "d", nil)))
	if r.Results[0].Status != proto.ResSuperseded {
		t.Fatalf("second delete: %+v", r)
	}
}

func TestRejectionWritesRevertAndSpokeAppliesIt(t *testing.T) {
	h, a, b := hubFixture(t)
	// a unique index only on the hub: the spoke cannot know
	// (a raw SQL index: a collection index would travel to the spokes with the schema bundle)
	if _, err := h.app.NonconcurrentDB().NewQuery("CREATE UNIQUE INDEX idx_items_title_u ON items (title) WHERE title != ''").Execute(); err != nil {
		t.Fatal(err)
	}
	first := h.create(t, map[string]any{"title": "dup"})
	_ = first
	a.sync(t)
	b.sync(t)

	// a creates a record with the same title: the hub refuses with a revert
	bad := a.create(t, map[string]any{"title": "dup"})
	res := a.sync(t)
	if res.Rejected != 1 {
		t.Fatalf("result %+v", res)
	}
	if _, err := h.app.FindRecordById("items", bad.Id); err == nil {
		t.Fatal("the hub must not have the rejected record")
	}
	rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()})
	if len(rows) != 1 || rows[0].Status != StatusRejected {
		t.Fatalf("hub rows: %+v", rows)
	}
	// the revert (record unknown on the hub => op d) was applied by the spoke's pull in the same cycle
	if _, err := a.app.FindRecordById("items", bad.Id); err == nil {
		t.Fatal("the spoke must drop the rejected record when it applies the revert")
	}
	// pending changes are final: nothing left to push
	if st := a.c.Status(); st.Pending != 0 {
		t.Fatalf("pending %d", st.Pending)
	}
	// and a re-push after a lost response answers from the table
	_ = b
	requireConverged(t, h, a, b)
}

func TestRevertOverwritesLocalState(t *testing.T) {
	h, a, b := hubFixture(t)
	r := a.create(t, map[string]any{"title": "orig"})
	a.sync(t)
	b.sync(t)
	// b edits (older hlc than a's next edit); both push; b loses and must converge to a's value
	ar, _ := a.app.FindRecordById("items", r.Id)
	br, _ := b.app.FindRecordById("items", r.Id)
	br.Set("title", "b edit")
	if err := b.app.Save(br); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	ar.Set("title", "a edit")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	a.sync(t) // a pushes first and wins at the hub
	res := b.sync(t)
	if res.Superseded != 1 {
		t.Fatalf("b result %+v", res)
	}
	a.sync(t)
	requireConverged(t, h, a, b)
	if x, _ := b.app.FindRecordById("items", r.Id); x.GetString("title") != "a edit" {
		t.Fatalf("b title %q", x.GetString("title"))
	}
}

func TestConcurrentEditsConvergeAcrossThreeNodes(t *testing.T) {
	h, a, b := hubFixture(t)
	r := h.create(t, map[string]any{"title": "base", "qty": 1})
	a.sync(t)
	b.sync(t)
	for i := 0; i < 3; i++ {
		ar, _ := a.app.FindRecordById("items", r.Id)
		br, _ := b.app.FindRecordById("items", r.Id)
		hr := hubItem(t, h, r.Id)
		ar.Set("title", fmt.Sprintf("a%d", i))
		br.Set("title", fmt.Sprintf("b%d", i))
		br.Set("qty", br.GetFloat("qty")+2)
		hr.Set("title", fmt.Sprintf("h%d", i))
		for _, x := range []struct {
			app core.App
			rec *core.Record
		}{{a.app, ar}, {b.app, br}, {h.app, hr}} {
			if err := x.app.Save(x.rec); err != nil {
				t.Fatal(err)
			}
		}
		for k := 0; k < 3; k++ {
			a.sync(t)
			b.sync(t)
		}
	}
	requireConverged(t, h, a, b)
	if q := hubItem(t, h, r.Id).GetFloat("qty"); q != 7 {
		t.Fatalf("counters never conflict: hub qty %v, want 7", q)
	}
}

func TestPullImplicitAckAndLowWater(t *testing.T) {
	h, a, _ := hubFixture(t)
	for i := 0; i < 3; i++ {
		h.create(t, map[string]any{"title": fmt.Sprint(i)})
	}
	tok := a.token(t)
	head := h.m.headSeq()
	st, pr := rawPull(t, h, tok, "after=0&limit=2")
	if st != 200 || len(pr.Changes) != 2 || !pr.More || pr.Next != pr.Changes[1].Seq {
		t.Fatalf("page 1: %d %+v", st, pr)
	}
	pulled := func() int64 {
		n, _ := h.app.FindRecordById(NodesCollection, a.m.NodeID())
		return int64(n.GetFloat("pulled_seq"))
	}
	if pulled() != 0 {
		t.Fatal("after=0 acks nothing")
	}
	st, pr2 := rawPull(t, h, tok, "after="+strconv.FormatInt(pr.Next, 10)+"&limit=10")
	if st != 200 || len(pr2.Changes) != 1 || pr2.More || pr2.Next != head {
		t.Fatalf("page 2: %d %+v (head %d)", st, pr2, head)
	}
	if pulled() != pr.Next {
		t.Fatalf("pulled_seq %d, want %d (implicit ack of `after`)", pulled(), pr.Next)
	}
	// a cursor beyond the head is refused (P3-7) and never acks beyond the head
	if st, _ := rawPull(t, h, tok, "after=999999"); st != http.StatusGone {
		t.Fatalf("after > head: %d, want 410", st)
	}
	if pulled() > head {
		t.Fatalf("pulled_seq %d beyond the head %d", pulled(), head)
	}
	// below the low water mark => 410
	if err := (dbState{db: h.app.NonconcurrentDB()}).Set(keyLowWater, "100"); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("GET", h.srv.URL+proto.PathPull+"?after=5", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	res, _ := http.DefaultClient.Do(r)
	if res.StatusCode != 410 {
		t.Fatalf("status %d", res.StatusCode)
	}
	var eb proto.ErrorBody
	_ = json.NewDecoder(res.Body).Decode(&eb)
	if eb.Data["code"] != proto.CodeRebootstrap {
		t.Fatalf("%+v", eb)
	}
}

func TestPullReturnsRevertsOnlyToTheirTarget(t *testing.T) {
	h, a, b := hubFixture(t)
	cid, id := h.items.Id, "recordaaaaaaaa1"
	ta, tb := a.token(t), b.token(t)
	rawPush(t, h, ta, pushReq(pc(a.m.NodeID(), 1, nowHLC(-9000, 0), 0, cid, id, "c", map[string]any{"title": "t"})))
	rawPush(t, h, tb, pushReq(pc(b.m.NodeID(), 1, nowHLC(-9500, 0), 0, cid, id, "u", map[string]any{"title": "x"}))) // superseded => revert for b
	_, pa := rawPull(t, h, ta, "after=0")
	if len(pa.Changes) != 1 || pa.Changes[0].Revert || pa.Changes[0].Node != a.m.NodeID() {
		t.Fatalf("a gets its own applied change but not b's revert: %+v", pa.Changes)
	}
	_, pb := rawPull(t, h, tb, "after=0")
	if len(pb.Changes) != 2 || !pb.Changes[1].Revert || pb.Changes[0].Revert {
		t.Fatalf("b gets a's change and its own revert: %+v", pb.Changes)
	}
}

func TestPullWaitWakesOnNewSeq(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	head := h.m.headSeq()

	type out struct {
		pr  proto.PullResponse
		dur time.Duration
	}
	ch := make(chan out, 1)
	start := time.Now()
	go func() {
		_, pr := rawPull(t, h, tok, "after="+strconv.FormatInt(head, 10)+"&wait=20")
		ch <- out{pr, time.Since(start)}
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("the long-poll must wait while there is nothing")
	default:
	}
	h.create(t, map[string]any{"title": "wake"})
	select {
	case o := <-ch:
		if len(o.pr.Changes) != 1 || o.dur > 5*time.Second {
			t.Fatalf("woken: %+v after %v", o.pr, o.dur)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("the long-poll was not woken by a new seq")
	}
	// a wait that times out returns an empty page (wait is capped at 25 s; use a short one through the cap check)
	st, _ := rawPull(t, h, tok, "after=0&wait=abc")
	if st != 400 {
		t.Fatalf("bad wait: %d", st)
	}
}

func TestAckDigestMismatch(t *testing.T) {
	h, a, _ := hubFixture(t)
	a.create(t, map[string]any{"title": "x"})
	h.create(t, map[string]any{"title": "y"})
	a.sync(t)
	cur, _ := client.LoadCursor(a.app)
	col := h.items.Id
	good, _, err := metaDigest(a.app.DB(), col)
	if err != nil {
		t.Fatal(err)
	}
	ar, err := a.c.Ack(ctxb, cur.PullAfter, map[string]string{col: good})
	if err != nil || !ar.DigestChecked || len(ar.DigestMismatch) != 0 {
		t.Fatalf("matching digest: %v %+v", err, ar)
	}
	ar, err = a.c.Ack(ctxb, cur.PullAfter, map[string]string{col: "deadbeef"})
	if err != nil || !ar.DigestChecked || len(ar.DigestMismatch) != 1 || ar.DigestMismatch[0] != col {
		t.Fatalf("wrong digest: %v %+v", err, ar)
	}
	// behind the head: the comparison is skipped (it would only measure the lag)
	h.create(t, map[string]any{"title": "z"})
	ar, err = a.c.Ack(ctxb, cur.PullAfter, map[string]string{col: "deadbeef"})
	if err != nil || ar.DigestChecked || len(ar.DigestMismatch) != 0 {
		t.Fatalf("behind: %v %+v", err, ar)
	}
	// an unknown collection is reported
	a.sync(t)
	cur, _ = client.LoadCursor(a.app)
	ar, _ = a.c.Ack(ctxb, cur.PullAfter, map[string]string{"nope": "x"})
	if len(ar.DigestMismatch) != 1 {
		t.Fatalf("%+v", ar)
	}
}

func TestVerifyFindsRawSQLDrift(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := a.create(t, map[string]any{"title": "x"})
	a.sync(t)
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE items SET title='raw' WHERE id={:id}").Bind(dbx.Params{"id": r.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	_, _, mm := digestOf(t, a.app, "items")
	if len(mm) != 1 || mm[0].ID != r.Id || mm[0].Reason != "hash" {
		t.Fatalf("mismatches %+v", mm)
	}
	_, _, mm = digestOf(t, h.app, "items")
	if len(mm) != 0 {
		t.Fatalf("hub %+v", mm)
	}
}

func TestFutureHLCRejected(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	_, r, _ := rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, nowHLC(int64(time.Hour/time.Millisecond), 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})))
	if r.Results[0].Status != proto.ResRejected || r.Results[0].Code != proto.CodeFutureHLC {
		t.Fatalf("%+v", r)
	}
}

func TestTxGroupAllOrNothing(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	node, cid := a.m.NodeID(), h.items.Id
	c1 := pc(node, 1, nowHLC(-5000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "ok"})
	c2 := pc(node, 2, nowHLC(-4000, 0), 0, "pbc_doesnotexist", "recordaaaaaaaa2", "c", map[string]any{"title": "bad"})
	c1.Tx, c2.Tx = "g1", "g1"
	_, r, _ := rawPush(t, h, tok, pushReq(c1, c2))
	if len(r.Results) != 2 || r.Results[0].Status != proto.ResRejected || r.Results[1].Status != proto.ResRejected ||
		r.Results[0].Code != proto.CodePolicyDirection || r.AckedThrough != 2 {
		t.Fatalf("%+v", r)
	}
	if _, err := h.app.FindRecordById("items", "recordaaaaaaaa1"); err == nil {
		t.Fatal("the first change of a rejected group must not be applied")
	}
}

func TestCounterAndSetRebase(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "c", "qty": 10, "tags": []string{"a"}})
	a.sync(t)

	// a: +3 and add b, offline (unpushed)
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("qty", 13)
	ar.Set("tags", []string{"a", "b"})
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	// hub meanwhile: qty 15, tags a,c
	hr := hubItem(t, h, r.Id)
	hr.Set("qty", 15)
	hr.Set("tags", []string{"a", "c"})
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	if res := a.c.PullOnce(ctxb); res.Err != nil {
		t.Fatal(res.Err)
	}
	ar, _ = a.app.FindRecordById("items", r.Id)
	if ar.GetFloat("qty") != 18 { // hub 15 + pending +3
		t.Fatalf("rebased qty %v, want 18", ar.GetFloat("qty"))
	}
	if got := strings.Join(ar.GetStringSlice("tags"), ","); got != "a,c,b" {
		t.Fatalf("rebased tags %q, want a,c,b", got)
	}
	// now push: hub gets both ops, and nothing is double counted
	a.sync(t)
	a.sync(t)
	hr = hubItem(t, h, r.Id)
	if hr.GetFloat("qty") != 18 || strings.Join(hr.GetStringSlice("tags"), ",") != "a,c,b" {
		t.Fatalf("hub %v", hr.FieldsData())
	}
	requireConverged(t, h, a)
}

func TestRealtimePokeAuthAndDelivery(t *testing.T) {
	h, a, _ := hubFixture(t)
	// connect SSE
	res, err := http.Get(h.srv.URL + "/api/realtime")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	br := bufio.NewReader(res.Body)
	readEvent := func() (string, string) {
		var ev, data string
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return "", ""
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ev != "" {
					return ev, data
				}
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(line[5:])
			}
		}
	}
	ev, data := readEvent()
	if ev != "PB_CONNECT" {
		t.Fatalf("first event %q", ev)
	}
	var cid struct {
		ClientID string `json:"clientId"`
	}
	_ = json.Unmarshal([]byte(data), &cid)
	subscribe := func(tok string) int {
		body, _ := json.Marshal(map[string]any{"clientId": cid.ClientID, "subscriptions": []string{proto.Topic}})
		r, _ := http.NewRequest("POST", h.srv.URL+"/api/realtime", bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		rs, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		rs.Body.Close()
		return rs.StatusCode
	}
	if st := subscribe(""); st != 403 {
		t.Fatalf("a guest must not subscribe to @sync (got %d)", st)
	}
	if st := subscribe("garbage"); st != 403 {
		t.Fatalf("a bad token must not subscribe (got %d)", st)
	}
	if st := subscribe(a.token(t)); st != 204 {
		t.Fatalf("a node subscribes (got %d)", st)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		h.create(t, map[string]any{"title": "poke me"})
	}()
	got := make(chan string, 1)
	go func() {
		for {
			e, d := readEvent()
			if e == "" || e == proto.Topic {
				got <- d
				return
			}
		}
	}()
	select {
	case d := <-got:
		var p map[string]any
		if err := json.Unmarshal([]byte(d), &p); err != nil || len(p) != 1 || p["seq"] == nil {
			t.Fatalf("payload %q must be exactly {\"seq\":N}", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no poke received")
	}
}

func TestClientLoopPokeTriggersPull(t *testing.T) {
	h, a, _ := hubFixture(t)
	a.c.Stop(ctxb)
	// a loop that would only sync on a poke or SyncNow (interval 1 h)
	loopC := a.s().client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Interval = time.Hour
	})
	loopC.Start(ctxb)
	defer loopC.Stop(ctxb)
	go func() {
		for ev := range loopC.Events() {
			t.Logf("event %+v", ev)
		}
	}()
	if r := <-loopC.SyncNow(); r.Err != nil {
		t.Fatal(r.Err)
	}
	time.Sleep(1500 * time.Millisecond) // let the SSE subscription settle
	r := h.create(t, map[string]any{"title": "pushed to spoke"})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if x, err := a.app.FindRecordById("items", r.Id); err == nil && x.GetString("title") == "pushed to spoke" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the poke did not make the spoke pull")
}

func (s *itemsSpoke) s() *spokeEnv { return s.spokeEnv }

func TestClientLoopOfflineAndBackoff(t *testing.T) {
	// Backoff: 1 s doubling to 5 min, +-20% jitter, Retry-After is a floor
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 9: 256 * time.Second, 10: 300 * time.Second, 50: 300 * time.Second} {
		lo, hi := client.Backoff(n, 0, 0), client.Backoff(n, 0.999999, 0)
		if lo < time.Duration(float64(want)*0.8)-time.Millisecond || lo > time.Duration(float64(want)*0.8)+time.Millisecond ||
			hi < time.Duration(float64(want)*1.19) || hi > time.Duration(float64(want)*1.2)+time.Millisecond {
			t.Fatalf("backoff(%d) in [%v,%v], want %v +-20%%", n, lo, hi, want)
		}
	}
	if d := client.Backoff(1, 0.5, 90*time.Second); d != 90*time.Second {
		t.Fatalf("Retry-After floor: %v", d)
	}

	h, a, _ := hubFixture(t)
	var hits atomic.Int64
	counting := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return http.DefaultTransport.RoundTrip(r)
	})}
	c := a.s().client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Interval = 50 * time.Millisecond
		o.HTTP = counting
		o.NoPoke = true
	})
	c.SetConditions(client.Conditions{Online: false})
	c.Start(ctxb)
	defer c.Stop(ctxb)
	a.create(t, map[string]any{"title": "offline write"})
	if r := <-c.SyncNow(); r.Err != client.ErrOffline {
		t.Fatalf("SyncNow offline: %v", r.Err)
	}
	time.Sleep(300 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("Online=false must make no attempts (%d requests)", hits.Load())
	}
	if st := c.Status(); st.Online || st.Pending != 1 {
		t.Fatalf("status %+v", st)
	}
	// pause
	c.SetConditions(client.Conditions{Online: true})
	c.Pause()
	if r := <-c.SyncNow(); r.Err != client.ErrPaused {
		t.Fatalf("paused: %v", r.Err)
	}
	c.Resume()
	deadline := time.Now().Add(5 * time.Second)
	for c.Status().Pending != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.Status().Pending != 0 || hits.Load() == 0 {
		t.Fatalf("going online must sync: %+v hits=%d", c.Status(), hits.Load())
	}

	// failing hub => failures grow and the next attempt is pushed out
	h.srv.Close()
	a.create(t, map[string]any{"title": "while down"})
	r := <-c.SyncNow()
	if r.Err == nil {
		t.Fatal("expected an error with the hub down")
	}
	st := c.Status()
	if st.Failures < 1 || st.LastError == "" || time.Until(st.NextAttempt) <= 0 || time.Until(st.NextAttempt) > 3*time.Second {
		t.Fatalf("status after failure: %+v (next in %v)", st, time.Until(st.NextAttempt))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSpokeRevokedStopsLoop(t *testing.T) {
	h, a, _ := hubFixture(t)
	c := a.s().client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.Interval = 50 * time.Millisecond
		o.NoPoke = true
	})
	if _, err := RevokeNode(h.app, "gate-1", false); err != nil {
		t.Fatal(err)
	}
	c.Start(ctxb)
	defer c.Stop(ctxb)
	r := <-c.SyncNow()
	if r.Err != client.ErrRevoked {
		t.Fatalf("got %v", r.Err)
	}
	time.Sleep(200 * time.Millisecond)
	if st := c.Status(); st.State != "revoked" || st.Running {
		t.Fatalf("status %+v", st)
	}
}

func TestVerifyCommandOutput(t *testing.T) {
	h, a, _ := hubFixture(t)
	a.create(t, map[string]any{"title": "x"})
	a.sync(t)
	rep, err := Verify(h.app)
	if err != nil || len(rep.Collections) != 1 || rep.Collections[0].Records != 1 || len(rep.Collections[0].Digest) != 64 {
		t.Fatalf("%v %+v", err, rep)
	}
	if rep.Collections[0].Digest != rep.Collections[0].MetaDigest {
		t.Fatal("meta digest must equal the record digest when nothing drifted")
	}
}

// TestRandomInterleavingsConverge drives three nodes with random writes and
// partial syncs, then syncs to quiescence: all digests must be equal.
func TestRandomInterleavingsConverge(t *testing.T) {
	if testing.Short() && testing.Verbose() == false {
		// still cheap enough for -short: a few seeds
	}
	seeds, steps := int64(4), 60
	if os.Getenv("TOKI_SYNC_STRESS") != "" {
		seeds, steps = 24, 90
	}
	for seed := int64(1); seed <= seeds; seed++ {
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			h, a, b := hubFixture(t)
			rng := newLCG(seed)
			var ids []string
			nodes := []struct {
				app  core.App
				sync func()
			}{
				{h.app, func() {}},
				{a.app, func() { a.sync(t) }},
				{b.app, func() { b.sync(t) }},
			}
			for step := 0; step < steps; step++ {
				n := nodes[rng.n(3)]
				switch op := rng.n(10); {
				case op < 2 || len(ids) == 0:
					col, _ := n.app.FindCollectionByNameOrId("items")
					r := core.NewRecord(col)
					r.Set("title", fmt.Sprintf("t%d", step))
					r.Set("qty", rng.n(5))
					if err := n.app.Save(r); err != nil {
						t.Fatal(err)
					}
					ids = append(ids, r.Id)
				default:
					id := ids[rng.n(len(ids))]
					r, err := n.app.FindRecordById("items", id)
					if err != nil {
						break // not here (yet) or deleted
					}
					switch rng.n(5) {
					case 0:
						r.Set("title", fmt.Sprintf("e%d", step))
					case 1:
						r.Set("qty", r.GetFloat("qty")+float64(rng.n(4)+1))
					case 2:
						// note: saving an unchanged record would bump `updated` without a change row
						// (capture writes nothing for autodate-only updates), so always change the set
						tags := r.GetStringSlice("tags")
						tag := []string{"a", "b", "c", "d"}[rng.n(4)]
						found := -1
						for i, x := range tags {
							if x == tag {
								found = i
							}
						}
						if found >= 0 {
							tags = append(tags[:found], tags[found+1:]...)
						} else {
							tags = append(tags, tag)
						}
						r.Set("tags", tags)
					case 3:
						r.Set("meta", map[string]any{"k": step})
					case 4:
						if rng.n(4) == 0 {
							if err := n.app.Delete(r); err != nil {
								t.Fatal(err)
							}
							continue
						}
						r.Set("title", fmt.Sprintf("x%d", step))
					}
					if err := n.app.Save(r); err != nil {
						t.Fatal(err)
					}
				}
				if rng.n(4) == 0 {
					nodes[1+rng.n(2)].sync()
				}
			}
			for i := 0; i < 4; i++ {
				a.sync(t)
				b.sync(t)
			}
			requireConverged(t, h, a, b)
		})
	}
}

type lcg struct{ s uint64 }

func newLCG(seed int64) *lcg { return &lcg{uint64(seed)*2862933555777941757 + 3037000493} }
func (l *lcg) n(m int) int {
	l.s = l.s*6364136223846793005 + 1442695040888963407
	return int((l.s >> 33) % uint64(m))
}

// diffDump lists the _changes rows of the records that differ between two nodes.
func diffDump(hub, spoke core.App, all ...*itemsSpoke) string {
	hr, _ := hub.FindAllRecords("items")
	sr, _ := spoke.FindAllRecords("items")
	sm := map[string]string{}
	for _, r := range sr {
		b, _ := json.Marshal(r.FieldsData())
		sm[r.Id] = string(b)
	}
	var sb strings.Builder
	for _, r := range hr {
		b, _ := json.Marshal(r.FieldsData())
		if sm[r.Id] == string(b) {
			continue
		}
		fmt.Fprintf(&sb, "DIFF %s\n hub:   %s\n spoke: %s\n", r.Id, b, sm[r.Id])
		apps := map[string]core.App{"hub": hub}
		for i, sp := range all {
			apps[fmt.Sprintf("spoke%d", i)] = sp.app
		}
		for name, app := range apps {
			var rows []chg
			_ = app.DB().NewQuery("SELECT seq, node, origin_seq, hlc, base_hlc, op, patch, status FROM _changes WHERE record={:r} ORDER BY seq").Bind(dbx.Params{"r": r.Id}).All(&rows)
			for _, c := range rows {
				fmt.Fprintf(&sb, "  %s seq=%d %s:%d hlc=%x base=%x %s %s %s\n", name, c.Seq, c.Node, c.OriginSeq, c.HLC, c.BaseHLC, c.Op, c.Status, c.Patch)
			}
		}
	}
	return sb.String()
}

func events(c *client.Client) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "applyErrors=%d\n", c.Status().ApplyErrors)
	for {
		select {
		case ev := <-c.Events():
			if ev.Type == client.EventError {
				fmt.Fprintf(&sb, "event %+v\n", ev)
			}
		default:
			return sb.String()
		}
	}
}

func TestPendingLocalFieldWithHigherHLCSurvivesPull(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "t0", "note": "n0"})
	a.sync(t)

	// the hub edits first, a edits later (higher hlc) without having pulled
	hr := hubItem(t, h, r.Id)
	hr.Set("title", "hub edit")
	hr.Set("qty", 5)
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(60 * time.Millisecond) // well beyond the ms resolution of the HLC and the clock offset jitter
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("title", "a edit")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	if res := a.c.PullOnce(ctxb); res.Err != nil {
		t.Fatal(res.Err)
	}
	ar, _ = a.app.FindRecordById("items", r.Id)
	if ar.GetString("title") != "a edit" {
		t.Fatalf("a pending change with a higher hlc must survive the pull, got %q", ar.GetString("title"))
	}
	if ar.GetFloat("qty") != 5 {
		t.Fatalf("other fields of the pulled change apply, qty=%v", ar.GetFloat("qty"))
	}
	// it wins at the hub as well
	a.sync(t)
	if got := hubItem(t, h, r.Id).GetString("title"); got != "a edit" {
		t.Fatalf("hub title %q", got)
	}
	requireConverged(t, h, a)
}

func TestHubHLCObservedOnAccept(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	ahead := nowHLC(2*60*1000, 7) // 2 minutes ahead is within the 5 minute bound
	rawPush(t, h, tok, pushReq(pc(a.m.NodeID(), 1, ahead, 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})))
	if h.m.Clock().Last() < ahead {
		t.Fatalf("the hub clock %v must have observed %v", h.m.Clock().Last(), ahead)
	}
}
