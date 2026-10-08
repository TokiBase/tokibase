//go:build !no_sync

package sync

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// ---- fixtures ---------------------------------------------------------

// actorHub is a hub whose spokes have the hub test user as SERVICE actor, so
// that the rules of the hub collections are really evaluated (not superuser).
func actorHub(t *testing.T) (*hubEnv, *itemsSpoke, *itemsSpoke) {
	t.Helper()
	h := newHub(t)
	h.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter, "tags": TypeSet}, []string{"note"})
	mk := func(name string) *itemsSpoke {
		_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: name, Profile: "edge", Actor: "users/" + h.usr.Id})
		if err != nil {
			t.Fatal(err)
		}
		s := newItemsSpokeWith(t, h, name, code)
		s.sync(t)
		return s
	}
	return h, mk("gate-1"), mk("gate-2")
}

// setRules replaces the create/update/delete/view rules of the hub items collection.
func (h *hubEnv) setRules(t *testing.T, create, update, del, view *string) {
	t.Helper()
	c, err := h.app.FindCollectionByNameOrId("items")
	if err != nil {
		t.Fatal(err)
	}
	c.CreateRule, c.UpdateRule, c.DeleteRule, c.ViewRule = create, update, del, view
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
}

func sp(s string) *string { return &s }

// grant gives the spoke a grant for the hub user and returns its aid.
func grant(t *testing.T, s *itemsSpoke, user *core.Record) string {
	t.Helper()
	tok, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.c.AddActor(ctxb, tok)
	if err != nil {
		t.Fatalf("AddActor: %v", err)
	}
	return res.AID
}

// asUser does a local request on the spoke as the granted user.
func (s *itemsSpoke) asUser(t *testing.T, aid, method, url, body string) (int, []byte) {
	t.Helper()
	tok, err := s.m.LocalToken(aid)
	if err != nil {
		t.Fatalf("LocalToken: %v", err)
	}
	mux := s.handler(t)
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", tok)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func idOf(t *testing.T, body []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	return m["id"].(string)
}

func pushOne(t *testing.T, h *hubEnv, s *itemsSpoke, c proto.PushChange) proto.PushResult {
	t.Helper()
	st, ok, eb := rawPush(t, h, s.token(t), pushReq(c))
	if st != 200 || len(ok.Results) != 1 {
		t.Fatalf("push: %d %+v %+v", st, ok, eb)
	}
	return ok.Results[0]
}

func countWhere(t *testing.T, app core.App, table, where string, args dbx.Params) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM " + table + " WHERE " + where).Bind(args).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// ---- tests ------------------------------------------------------------

func TestReplayRuleDeniedRevertsSpoke(t *testing.T) {
	h, a, _ := actorHub(t)
	r := a.create(t, map[string]any{"title": "orig", "qty": 1})
	a.sync(t)
	// the hub moves the record out of what the update rule allows, while A is offline
	hr := hubItem(t, h, r.Id)
	hr.Set("qty", 500)
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	h.setRules(t, sp(""), sp("qty < 100"), sp(""), sp(""))

	mine, _ := a.app.FindRecordById("items", r.Id)
	mine.Set("title", "mine")
	if err := a.app.Save(mine); err != nil {
		t.Fatal(err)
	}
	res := a.sync(t)
	if res.Rejected != 1 {
		t.Fatalf("result %+v", res)
	}
	if got := hubItem(t, h, r.Id); got.GetString("title") != "orig" {
		t.Fatalf("the denied update was applied on the hub: %v", got.FieldsData())
	}
	rows := changeRows(t, h.app, "status='rejected'", nil)
	if len(rows) != 1 {
		t.Fatalf("rejected rows: %+v", rows)
	}
	var code string
	_ = h.app.DB().NewQuery("SELECT code FROM _changes WHERE status='rejected'").Row(&code)
	if code != proto.CodeRuleDenied {
		t.Fatalf("code %q", code)
	}
	// the revert brings the hub state (title and the new qty) back to the spoke
	a.sync(t)
	back, _ := a.app.FindRecordById("items", r.Id)
	if back.GetString("title") != "orig" || back.GetFloat("qty") != 500 {
		t.Fatalf("spoke after revert: %v", back.FieldsData())
	}
}

func TestActorGrantReplayAndRequestInfo(t *testing.T) {
	h, a, _ := actorHub(t)
	// only a sync replay of an authenticated actor passes
	h.setRules(t,
		sp(`@request.context = "sync" && @request.auth.id != "" && @request.headers.x_toki_sync_node != "" && @request.method = "POST"`),
		sp(`@request.context = "sync" && @request.body.title != ""`),
		sp(""), sp(""))
	aid := grant(t, a, h.usr)

	// a grant row exists on the hub and is bound to the node
	if n := countWhere(t, h.app, "_sync_actor_grants", "aid={:a} AND node={:n}", dbx.Params{"a": aid, "n": a.m.NodeID()}); n != 1 {
		t.Fatalf("grant rows: %d", n)
	}
	code, out := a.asUser(t, aid, "POST", "/api/collections/items/records", `{"title":"by user","qty":2}`)
	if code != 200 {
		t.Fatalf("local create: %d %s", code, out)
	}
	id := idOf(t, out)
	if rows := changeRows(t, a.app, "node={:n}", dbx.Params{"n": a.m.NodeID()}); len(rows) != 1 || rows[0].Actor != aid {
		t.Fatalf("captured actor: %+v (want %s)", rows, aid)
	}
	a.sync(t)
	if hubItem(t, h, id).GetString("title") != "by user" {
		t.Fatal("the create was not applied on the hub")
	}
	hr := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()})
	if len(hr) != 1 || hr[0].Actor != aid || hr[0].Status != StatusApplied {
		t.Fatalf("hub row: %+v", hr)
	}
	// the same request straight to the hub is denied by the same rule
	if code, _ := h.do(t, h.usr, "POST", "/api/collections/items/records", `{"title":"direct"}`); code == 200 {
		t.Fatal("@request.context = sync must not match a client request")
	}
	// update through a local request of the user
	if code, out := a.asUser(t, aid, "PATCH", "/api/collections/items/records/"+id, `{"title":"edited"}`); code != 200 {
		t.Fatalf("local update: %d %s", code, out)
	}
	a.sync(t)
	if hubItem(t, h, id).GetString("title") != "edited" {
		t.Fatal("the update was not applied on the hub")
	}
	if code, _ := h.do(t, h.usr, "PATCH", "/api/collections/items/records/"+id, `{"title":"direct"}`); code == 200 {
		t.Fatal("update rule must be sync only")
	}
}

func TestActorFromAnotherNodeAndUnknown(t *testing.T) {
	h, a, b := actorHub(t)
	aid := grant(t, a, h.usr)
	cid := h.items.Id
	c := pc(b.m.NodeID(), 1, nowHLC(-1000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "stolen"})
	c.Actor = aid
	if r := pushOne(t, h, b, c); r.Status != proto.ResRejected || r.Code != proto.CodeActorNodeMismatch {
		t.Fatalf("grant of another node: %+v", r)
	}
	c = pc(b.m.NodeID(), 2, nowHLC(-1000, 0), 0, cid, "recordaaaaaaaa2", "c", map[string]any{"title": "x"})
	c.Actor = "gdoesnotexist"
	if r := pushOne(t, h, b, c); r.Status != proto.ResRejected || r.Code != proto.CodeActorUnknown {
		t.Fatalf("unknown grant: %+v", r)
	}
	if _, err := h.app.FindFirstRecordByFilter("items", "title='stolen'"); err == nil {
		t.Fatal("nothing may be applied")
	}
}

func TestActorWithoutServiceActorIsRejected(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, nil, nil)
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: "bare", Profile: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	s := newItemsSpokeWith(t, h, "bare", code)
	c := pc(s.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})
	if r := pushOne(t, h, s, c); r.Status != proto.ResRejected || r.Code != proto.CodeActorUnknown {
		t.Fatalf("no service actor: %+v", r)
	}
}

func TestActorExpiredWindow(t *testing.T) {
	h, a, _ := actorHub(t)
	aid := grant(t, a, h.usr)
	cid := h.items.Id
	// a fixed window [base-10m, base+1h] set in the grant row: no dependence on
	// how long the test takes or on the second the grant was truncated to
	base := time.Now()
	iat, exp := base.Add(-10*time.Minute), base.Add(time.Hour)
	if _, err := h.app.DB().NewQuery("UPDATE _sync_actor_grants SET iat={:i}, exp={:e} WHERE aid={:a}").
		Bind(dbx.Params{"i": iat.UnixMilli(), "e": exp.UnixMilli(), "a": aid}).Execute(); err != nil {
		t.Fatal(err)
	}
	at := func(t time.Time, l uint16) hlc.HLC { return hlc.Make(t.UnixMilli(), l) }
	// older than iat - 5 min
	c := pc(a.m.NodeID(), 1, at(iat.Add(-6*time.Minute), 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "old"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Code != proto.CodeActorExpired || r.Status != proto.ResRejected {
		t.Fatalf("before the grant: %+v", r)
	}
	// after exp
	c = pc(a.m.NodeID(), 2, at(exp.Add(time.Minute), 0), 0, cid, "recordaaaaaaaa2", "c", map[string]any{"title": "late"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Code != proto.CodeActorExpired {
		t.Fatalf("after exp: %+v", r)
	}
	// inside the window
	c = pc(a.m.NodeID(), 3, at(base.Add(-time.Second), 0), 0, cid, "recordaaaaaaaa3", "c", map[string]any{"title": "ok"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Status != proto.ResApplied {
		t.Fatalf("inside: %+v", r)
	}
}

func TestActorTTLIsCappedAtRetention(t *testing.T) {
	t.Setenv(EnvActorTTL, "400d")
	t.Setenv(EnvRetention, "10d")
	if got := actorTTL(); got != 10*24*time.Hour {
		t.Fatalf("ttl %v", got)
	}
	t.Setenv(EnvActorTTL, "")
	t.Setenv(EnvRetention, "")
	if got := actorTTL(); got != DefaultActorTTL {
		t.Fatalf("default ttl %v", got)
	}
}

// withSessions installs fake session seams: tokens carry sid "sess-1", which
// is active while the returned flag is true.
func withSessions(t *testing.T) *atomic.Bool {
	t.Helper()
	active := &atomic.Bool{}
	active.Store(true)
	oldIssue, oldActive := kernel.OnAuthTokenIssue, kernel.SessionActive
	kernel.OnAuthTokenIssue = func(_ *kernel.Record, _ string, claims jwt.MapClaims, _ time.Duration) error {
		claims["sid"] = "sess-1"
		return nil
	}
	kernel.SessionActive = func(_ kernel.App, sid string) (bool, error) { return sid == "sess-1" && active.Load(), nil }
	t.Cleanup(func() { kernel.OnAuthTokenIssue, kernel.SessionActive = oldIssue, oldActive })
	return active
}

func TestActorRevokedBySessionIsParked(t *testing.T) {
	active := withSessions(t)
	h, a, _ := actorHub(t)
	aid := grant(t, a, h.usr)
	code, out := a.asUser(t, aid, "POST", "/api/collections/items/records", `{"title":"offline work"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	id := idOf(t, out)
	active.Store(false) // the hub session is revoked before the push
	res := a.sync(t)
	if res.Rejected != 1 {
		t.Fatalf("result %+v", res)
	}
	if _, err := h.app.FindRecordById("items", id); err == nil {
		t.Fatal("a revoked actor's change was applied")
	}
	rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()})
	if len(rows) != 1 || rows[0].Status != StatusParked {
		t.Fatalf("hub rows: %+v", rows)
	}
	var kind, resolution, status string
	if err := h.app.DB().NewQuery("SELECT kind, resolution, status FROM _sync_conflicts").Row(&kind, &resolution, &status); err != nil {
		t.Fatalf("no conflict row: %v", err)
	}
	if kind != "actor_revoked" || resolution != "parked" || status != "open" {
		t.Fatalf("conflict %s/%s/%s", kind, resolution, status)
	}
	if n := countWhere(t, h.app, "_changes", "status='revert'", nil); n != 0 {
		t.Fatalf("a parked change gets no revert row (%d)", n)
	}
	// the queue advances
	if r := a.sync(t); r.Rejected != 0 {
		t.Fatalf("parked changes are final: %+v", r)
	}
}

func TestActorRevokeEndpointAndRevokedNode(t *testing.T) {
	h, a, b := actorHub(t)
	aid := grant(t, a, h.usr)
	if err := a.c.RemoveActor(ctxb, aid); err != nil {
		t.Fatal(err)
	}
	c := pc(a.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Status != proto.ResParked || r.Code != proto.CodeActorRevoked {
		t.Fatalf("revoked grant: %+v", r)
	}
	// a node revoked at apply time parks the changes of its service actor too
	tok := b.token(t)
	if _, err := RevokeNode(h.app, b.m.NodeID(), false); err != nil {
		t.Fatal(err)
	}
	_ = tok
	var res *proto.PushResult
	h.m.applyMu.Lock()
	out, err := h.m.processGroup(h.app, b.m.NodeID(), "127.0.0.1", 0,
		[]*hubChange{mustChange(t, b.m.NodeID(), pc(b.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordaaaaaaaa2", "c", map[string]any{"title": "y"}))})
	h.m.applyMu.Unlock()
	if err != nil || len(out) != 1 {
		t.Fatalf("%v %+v", err, out)
	}
	res = &out[0]
	if res.Status != proto.ResParked || res.Code != proto.CodeActorRevoked {
		t.Fatalf("revoked node: %+v", res)
	}
	// a grant request from a revoked node is refused by the node auth
}

func mustChange(t *testing.T, node string, c proto.PushChange) *hubChange {
	t.Helper()
	hc, err := parseChange(node, c)
	if err != nil {
		t.Fatal(err)
	}
	return hc
}

func TestSuperuserActorRefused(t *testing.T) {
	h, a, _ := actorHub(t)
	tok, _ := h.su.NewAuthToken()
	_, err := a.c.AddActor(ctxb, tok)
	if !client.IsCode(err, proto.CodeActorForbidden) {
		t.Fatalf("superuser grant: %v", err)
	}
	if n := countWhere(t, h.app, "_sync_actor_grants", "1=1", nil); n != 0 {
		t.Fatal("no grant may exist")
	}
	// a grant forged before the switch is still refused at apply time
	t.Setenv(EnvAllowSuperuserActors, "1")
	res, err := a.c.AddActor(ctxb, tok)
	if err != nil {
		t.Fatalf("explicit opt-in: %v", err)
	}
	t.Setenv(EnvAllowSuperuserActors, "")
	c := pc(a.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "x"})
	c.Actor = res.AID
	if r := pushOne(t, h, a, c); r.Code != proto.CodeActorForbidden || r.Status != proto.ResRejected {
		t.Fatalf("superuser actor at apply time: %+v", r)
	}
}

func TestInvalidActorToken(t *testing.T) {
	_, a, _ := actorHub(t)
	if _, err := a.c.AddActor(ctxb, "not-a-token"); !client.IsCode(err, proto.CodeActorInvalid) {
		t.Fatalf("%v", err)
	}
}

func TestAutodateOfTheOriginIsPreserved(t *testing.T) {
	h, a, _ := actorHub(t)
	r := a.create(t, map[string]any{"title": "dated"})
	time.Sleep(30 * time.Millisecond)
	a.sync(t)
	hr := hubItem(t, h, r.Id)
	if hr.GetString("created") != r.GetString("created") || hr.GetString("updated") != r.GetString("updated") {
		t.Fatalf("hub created/updated %s/%s, origin %s/%s", hr.GetString("created"), hr.GetString("updated"), r.GetString("created"), r.GetString("updated"))
	}
	// and an update keeps the origin updated
	mine, _ := a.app.FindRecordById("items", r.Id)
	mine.Set("title", "dated2")
	_ = a.app.Save(mine)
	time.Sleep(30 * time.Millisecond)
	a.sync(t)
	hr = hubItem(t, h, r.Id)
	mine, _ = a.app.FindRecordById("items", r.Id)
	if hr.GetString("updated") != mine.GetString("updated") {
		t.Fatalf("hub updated %s, origin %s", hr.GetString("updated"), mine.GetString("updated"))
	}
	requireConverged(t, h, a)
}

// ---- review items of PR3 ----------------------------------------------

func TestRevertCarriesNoDataOutsideTheViewRule(t *testing.T) { // P3-2
	h, a, _ := actorHub(t)
	hidden := h.create(t, map[string]any{"title": "secret", "qty": 7})
	h.setRules(t, sp(""), sp("title != 'secret'"), sp(""), sp("title != 'secret'"))
	tok := a.token(t)
	c := pc(a.m.NodeID(), 1, nowHLC(0, 60000), 0, h.items.Id, hidden.Id, "u", map[string]any{"title": "probe"})
	if r := pushOne(t, h, a, c); r.Status != proto.ResRejected || r.Code != proto.CodeRuleDenied {
		t.Fatalf("probe: %+v", r)
	}
	st, pr := rawPull(t, h, tok, "after=0&limit=500")
	if st != 200 {
		t.Fatal(st)
	}
	found := false
	for _, ch := range pr.Changes {
		if ch.Revert && ch.Record == hidden.Id {
			found = true
			// P4-6: a verdict without data, never an op d (it would delete the device copy)
			if ch.Op == OpDelete || ch.Notice != proto.NoticeInvisible || strings.Contains(string(ch.Patch), "secret") || strings.Contains(string(ch.Patch), "qty") {
				t.Fatalf("the revert leaks the record: %+v %s", ch, ch.Patch)
			}
		}
		if !ch.Revert && ch.Record == hidden.Id && strings.Contains(string(ch.Patch), "secret") {
			// the normal row of the hub creation is out of scope here (view rules on normal rows are PR6)
			continue
		}
	}
	if !found {
		t.Fatal("no revert row")
	}
	// the stored revert row carries no data either
	var patch string
	if err := h.app.DB().NewQuery("SELECT patch FROM _changes WHERE status='revert' AND record={:r}").Bind(dbx.Params{"r": hidden.Id}).Row(&patch); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(patch, "secret") {
		t.Fatalf("stored revert: %s", patch)
	}
}

func TestPermanentApplyErrorDoesNotBlockTheQueue(t *testing.T) { // P3-5
	h, a, _ := actorHub(t)
	tok := a.token(t)
	n := a.m.NodeID()
	applyFault = func(c *hubChange) error {
		if c.oseq == 1 {
			return errors.New("disk image is malformed")
		}
		return nil
	}
	t.Cleanup(func() { applyFault = nil })
	st, ok, eb := rawPush(t, h, tok, pushReq(
		pc(n, 1, nowHLC(-3000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "poison"}),
		pc(n, 2, nowHLC(-2000, 0), 0, h.items.Id, "recordaaaaaaaa2", "c", map[string]any{"title": "fine"})))
	if st != 200 || len(ok.Results) != 2 {
		t.Fatalf("%d %+v %+v", st, ok, eb)
	}
	if ok.Results[0].Status != proto.ResRejected || ok.Results[0].Code != CodeApplyError {
		t.Fatalf("poison: %+v", ok.Results[0])
	}
	if ok.Results[1].Status != proto.ResApplied || ok.AckedThrough != 2 {
		t.Fatalf("next change: %+v acked %d", ok.Results[1], ok.AckedThrough)
	}
	if n := countWhere(t, h.app, "_sync_conflicts", "kind='apply_error'", nil); n != 1 {
		t.Fatalf("conflict rows: %d", n)
	}
	// a transient error is retried, not recorded
	applyFault = func(c *hubChange) error { return errors.New("database is locked") }
	st, _, eb = rawPush(t, h, tok, pushReq(pc(n, 3, nowHLC(-1000, 0), 0, h.items.Id, "recordaaaaaaaa3", "c", map[string]any{"title": "later"})))
	if st != http.StatusInternalServerError {
		t.Fatalf("transient: %d %+v", st, eb)
	}
	if c := countWhere(t, h.app, "_changes", "node={:n} AND origin_seq=3", dbx.Params{"n": n}); c != 0 {
		t.Fatal("a transient failure must not be recorded")
	}
}

func TestProcessedSequenceIsNeverAppliedTwice(t *testing.T) { // P3-6
	h, a, _ := actorHub(t)
	tok := a.token(t)
	n := a.m.NodeID()
	c := pc(n, 1, nowHLC(-3000, 0), 0, h.items.Id, "recordaaaaaaaa1", "c", map[string]any{"title": "once"})
	if st, ok, _ := rawPush(t, h, tok, pushReq(c)); st != 200 || ok.Results[0].Status != proto.ResApplied {
		t.Fatalf("first push: %d %+v", st, ok)
	}
	hr := hubItem(t, h, "recordaaaaaaaa1")
	hr.Set("title", "newer")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	// compaction / restore: the row is gone, pushed_origin_seq stays
	if _, err := h.app.DB().NewQuery("DELETE FROM _changes WHERE node={:n} AND origin_seq=1").Bind(dbx.Params{"n": n}).Execute(); err != nil {
		t.Fatal(err)
	}
	st, ok, _ := rawPush(t, h, tok, pushReq(c))
	if st != 200 || ok.Results[0].Status != proto.ResDuplicate {
		t.Fatalf("re-push: %d %+v", st, ok)
	}
	if got := hubItem(t, h, "recordaaaaaaaa1").GetString("title"); got != "newer" {
		t.Fatalf("the old change was applied again: %q", got)
	}
	if countWhere(t, h.app, "_changes", "node={:n} AND origin_seq=1", dbx.Params{"n": n}) != 0 {
		t.Fatal("no row may be re-created")
	}
}

func TestPullCursorBeyondHeadIsRefused(t *testing.T) { // P3-7
	h, a, _ := actorHub(t)
	tok := a.token(t)
	st, _ := rawPull(t, h, tok, "after=99999999")
	if st != http.StatusGone {
		t.Fatalf("status %d, want 410", st)
	}
	if st, _ := rawPull(t, h, tok, "after="+itoa(h.m.headSeq())); st != 200 {
		t.Fatalf("after == head must work: %d", st)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestAuthAndHiddenFieldsDoNotTravel(t *testing.T) { // P3-10
	h := newHub(t)
	col := core.NewAuthCollection("members")
	col.Fields.Add(&core.TextField{Name: "pin", Hidden: true}, &core.TextField{Name: "nick"})
	if err := h.app.Save(col); err != nil {
		t.Fatal(err)
	}
	names := func(p *policy) map[string]bool {
		out := map[string]bool{}
		for _, f := range syncedFields(col, p) {
			out[f.GetName()] = true
		}
		return out
	}
	got := names(&policy{Types: map[string]string{}})
	for _, n := range []string{"email", "emailVisibility", "verified", "pin", "password", "tokenKey"} {
		if got[n] {
			t.Fatalf("%s must not travel by default", n)
		}
	}
	if !got["nick"] {
		t.Fatal("plain fields travel")
	}
	opt := names(&policy{Types: map[string]string{"email": TypeInclude}})
	if !opt["email"] || opt["verified"] || opt["pin"] {
		t.Fatalf("opt-in only for the named field: %v", opt)
	}
}
