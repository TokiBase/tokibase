//go:build !no_sync

package sync

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// QC of PR4 (docs/SYNC_DESIGN.md §1.6, review-sync-pr4): findings P4-1 ... P4-12.

// withToken does a local request on the spoke with an arbitrary auth token.
func (s *itemsSpoke) withToken(t *testing.T, tok, method, url, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", tok)
	rec := httptest.NewRecorder()
	s.handler(t).ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// expectUngrantedRejected pushes what the local request of tok wrote and checks
// that the hub refused it as actor_unknown and applied nothing.
func expectUngrantedRejected(t *testing.T, h *hubEnv, s *itemsSpoke, tok string) {
	t.Helper()
	code, out := s.withToken(t, tok, "POST", "/api/collections/items/records", `{"title":"ungranted"}`)
	if code != 200 {
		t.Fatalf("local write: %d %s", code, out)
	}
	id := idOf(t, out)
	var actor string
	if err := s.app.DB().NewQuery("SELECT actor FROM _changes WHERE record={:r} AND status='local'").Bind(dbx.Params{"r": id}).Row(&actor); err != nil {
		t.Fatal(err)
	}
	if actor == ActorNode || !strings.HasPrefix(actor, ActorRecPrefix) {
		t.Fatalf("captured actor %q, want rec:<collection>:<id>", actor)
	}
	if res := s.sync(t); res.Rejected != 1 {
		t.Fatalf("result %+v", res)
	}
	if _, err := h.app.FindRecordById("items", id); err == nil {
		t.Fatal("a user without a valid grant was applied (as the service actor)")
	}
	rows := changeRows(t, h.app, "record={:r} AND status='rejected'", dbx.Params{"r": id})
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	var code2 string
	_ = h.app.DB().NewQuery("SELECT code FROM _changes WHERE record={:r} AND status='rejected'").Bind(dbx.Params{"r": id}).Row(&code2)
	if code2 != proto.CodeActorUnknown {
		t.Fatalf("code %q", code2)
	}
	if countWhere(t, h.app, "_changes", "status='revert' AND record={:r}", dbx.Params{"r": id}) != 1 {
		t.Fatal("no revert row")
	}
}

func TestUserWithoutGrantIsNeverTheServiceActor(t *testing.T) { // P4-1
	t.Run("no grant at all", func(t *testing.T) {
		h, a, _ := actorHub(t)
		u := core.NewRecord(mustCol(t, a.app, "users"))
		u.Set("email", "nogrant@example.com")
		u.SetPassword("1234567890")
		if err := a.app.Save(u); err != nil {
			t.Fatal(err)
		}
		tok, _ := u.NewAuthToken()
		expectUngrantedRejected(t, h, a, tok)
	})
	t.Run("expired grant, live local token", func(t *testing.T) {
		h, a, _ := actorHub(t)
		aid := grant(t, a, h.usr)
		tok, err := a.m.LocalToken(aid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.app.DB().NewQuery("UPDATE _sync_actors SET exp={:e}").Bind(dbx.Params{"e": time.Now().Add(-time.Hour).UnixMilli()}).Execute(); err != nil {
			t.Fatal(err)
		}
		if _, err := a.m.LocalToken(aid); err == nil {
			t.Fatal("no new token for an expired grant")
		}
		expectUngrantedRejected(t, h, a, tok)
	})
	t.Run("removed grant", func(t *testing.T) {
		h, a, _ := actorHub(t)
		aid := grant(t, a, h.usr)
		tok, _ := a.m.LocalToken(aid)
		if err := a.c.RemoveActor(ctxb, aid); err != nil {
			t.Fatal(err)
		}
		expectUngrantedRejected(t, h, a, tok)
	})
	t.Run("no request auth stays node", func(t *testing.T) {
		_, a, _ := actorHub(t)
		r := a.create(t, map[string]any{"title": "hook"})
		var actor string
		_ = a.app.DB().NewQuery("SELECT actor FROM _changes WHERE record={:r}").Bind(dbx.Params{"r": r.Id}).Row(&actor)
		if actor != ActorNode {
			t.Fatalf("actor %q", actor)
		}
	})
}

func mustCol(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestForgedHLCCannotUseAnExpiredGrant(t *testing.T) { // P4-3
	h, a, _ := actorHub(t)
	aid := grant(t, a, h.usr)
	cid := h.items.Id
	setExp := func(exp time.Time, iat time.Time) {
		if _, err := h.app.DB().NewQuery("UPDATE _sync_actor_grants SET exp={:e}, iat={:i} WHERE aid={:a}").
			Bind(dbx.Params{"e": exp.UnixMilli(), "i": iat.UnixMilli(), "a": aid}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	// expired 26 h ago (grace 24 h): a change stamped just before the expiry is a forgery
	setExp(time.Now().Add(-26*time.Hour), time.Now().Add(-27*time.Hour))
	c := pc(a.m.NodeID(), 1, nowHLC(-26*3600*1000-1000, 0), 0, cid, "recordaaaaaaaa1", "c", map[string]any{"title": "forged"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Status != proto.ResRejected || r.Code != proto.CodeActorExpired {
		t.Fatalf("forged hlc after the grace: %+v", r)
	}
	// expired 23 h ago: an offline device inside the grace may still push
	setExp(time.Now().Add(-23*time.Hour), time.Now().Add(-24*time.Hour))
	c = pc(a.m.NodeID(), 2, nowHLC(-23*3600*1000-1000, 0), 0, cid, "recordaaaaaaaa2", "c", map[string]any{"title": "offline"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Status != proto.ResApplied {
		t.Fatalf("inside the grace: %+v", r)
	}
	// a short grace closes it
	t.Setenv(EnvGrace, "1h")
	c = pc(a.m.NodeID(), 3, nowHLC(-23*3600*1000-1000, 1), 0, cid, "recordaaaaaaaa3", "c", map[string]any{"title": "late"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Code != proto.CodeActorExpired {
		t.Fatalf("TOKI_SYNC_GRACE=1h: %+v", r)
	}
	// lower bound: before the grant was issued
	setExp(time.Now().Add(time.Hour), time.Now().Add(-time.Minute))
	c = pc(a.m.NodeID(), 4, nowHLC(-3600*1000, 0), 0, cid, "recordaaaaaaaa4", "c", map[string]any{"title": "backdated"})
	c.Actor = aid
	if r := pushOne(t, h, a, c); r.Code != proto.CodeActorExpired {
		t.Fatalf("backdated below iat-5m: %+v", r)
	}
}

func TestParkedNoticeAndTTL(t *testing.T) { // P4-2 (a) + (c)
	t.Setenv(EnvParkTTL, "1h")
	active := withSessions(t)
	h, a, _ := actorHub(t)
	aid := grant(t, a, h.usr)
	code, out := a.asUser(t, aid, "POST", "/api/collections/items/records", `{"title":"offline work"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	id := idOf(t, out)
	active.Store(false)
	a.sync(t) // push: parked
	a.sync(t) // pull: the notice
	pend, err := client.PendingReview(a.app)
	if err != nil || len(pend) != 1 || pend[0].Record != id || pend[0].Notice != proto.NoticeParked || pend[0].Code != proto.CodeActorRevoked {
		t.Fatalf("pending review %+v %v", pend, err)
	}
	if _, err := a.app.FindRecordById("items", id); err != nil {
		t.Fatal("the node keeps serving its local value")
	}
	if n, err := h.m.ExpireParked(); err != nil || n != 0 {
		t.Fatalf("a fresh parked change must stay: %d %v", n, err)
	}
	// age the parked row beyond the TTL
	if _, err := h.app.DB().NewQuery("UPDATE _changes SET created='2020-01-01 00:00:00.000Z' WHERE status='parked'").Execute(); err != nil {
		t.Fatal(err)
	}
	if n, err := h.m.ExpireParked(); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	if countWhere(t, h.app, "_changes", "status='parked'", nil) != 0 || countWhere(t, h.app, "_changes", "status='rejected' AND code={:c}", dbx.Params{"c": proto.CodeParkExpired}) != 1 {
		t.Fatal("the parked row must become rejected/park_expired")
	}
	if countWhere(t, h.app, "_changes", "status='revert' AND record={:r}", dbx.Params{"r": id}) != 1 {
		t.Fatal("no revert row")
	}
	var status, resolution string
	if err := h.app.DB().NewQuery("SELECT status, resolution FROM _sync_conflicts").Row(&status, &resolution); err != nil || status != "resolved" || resolution != "rejected" {
		t.Fatalf("conflict %s/%s %v", status, resolution, err)
	}
	a.sync(t) // the revert reaches the node
	if _, err := a.app.FindRecordById("items", id); err == nil {
		t.Fatal("the reverted record must be gone on the node")
	}
	if pend, _ := client.PendingReview(a.app); len(pend) != 0 {
		t.Fatalf("the revert clears the review mark: %+v", pend)
	}
}

func TestInvisibleRevertKeepsLocalDataUnlessEvictIsOn(t *testing.T) { // P4-6
	for _, evict := range []bool{false, true} {
		name := "notice"
		if evict {
			name = "evict"
		}
		t.Run(name, func(t *testing.T) {
			if evict {
				t.Setenv(EnvEvictInvisible, "1")
			}
			h, a, _ := actorHub(t)
			r := h.create(t, map[string]any{"title": "secret", "qty": 7})
			a.sync(t)
			if _, err := a.app.FindRecordById("items", r.Id); err != nil {
				t.Fatal("the node should have the record")
			}
			h.setRules(t, sp(""), sp("title != 'secret'"), sp(""), sp("title != 'secret'"))
			mine, _ := a.app.FindRecordById("items", r.Id)
			mine.Set("title", "probe")
			if err := a.app.Save(mine); err != nil {
				t.Fatal(err)
			}
			a.sync(t)
			a.sync(t)
			_, err := a.app.FindRecordById("items", r.Id)
			if evict {
				if err == nil {
					t.Fatal("evict is on: the local copy must go")
				}
				if countWhere(t, a.app, "_sync_tombstones", "record={:r}", dbx.Params{"r": r.Id}) != 0 {
					t.Fatal("an eviction leaves no tombstone")
				}
				return
			}
			if err != nil {
				t.Fatal("an invisible record must not be deleted on the node")
			}
			pend, _ := client.PendingReview(a.app)
			if len(pend) != 1 || pend[0].Notice != proto.NoticeInvisible {
				t.Fatalf("pending review %+v", pend)
			}
		})
	}
}

func TestViewRuleAppliesToNormalPullRows(t *testing.T) { // P4-7
	h, a, _ := actorHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("title != 'secret'"))
	h.create(t, map[string]any{"title": "secret"})
	pub := h.create(t, map[string]any{"title": "public"})
	a.sync(t)
	if _, err := a.app.FindRecordById("items", pub.Id); err != nil {
		t.Fatal("a visible record must arrive")
	}
	if n := countWhere(t, a.app, "items", "title='secret'", nil); n != 0 {
		t.Fatal("a record outside the view rule of the service actor was delivered")
	}
}

func TestHookWrittenMembersReplayAsServiceActor(t *testing.T) { // P4-10
	h := newHub(t)
	h.policy(t, "items", DirBoth, nil, nil)
	svc := core.NewRecord(mustCol(t, h.app, "users"))
	svc.Set("email", "svc@example.com")
	svc.SetPassword("1234567890")
	if err := h.app.Save(svc); err != nil {
		t.Fatal(err)
	}
	h.setRules(t, sp(`(@request.body.title = "u" && @request.auth.id = "`+h.usr.Id+`") || (@request.body.title = "s" && @request.auth.id = "`+svc.Id+`")`), sp(""), sp(""), sp(""))
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: "svcnode", Profile: "edge", Actor: "users/" + svc.Id})
	if err != nil {
		t.Fatal(err)
	}
	s := newItemsSpokeWith(t, h, "svcnode", code)
	s.sync(t)
	aid := grant(t, s, h.usr)
	node := s.m.NodeID()
	grp := func(oseq int64, userTitle, hookTitle string) []proto.PushChange {
		c1 := pc(node, oseq, nowHLC(-5000, uint16(oseq)), 0, h.items.Id, "recordaaaaaaa"+itoa(oseq), "c", map[string]any{"title": userTitle})
		c1.Actor, c1.Tx = aid, "tx"+itoa(oseq)
		c2 := pc(node, oseq+1, nowHLC(-4000, uint16(oseq)), 0, h.items.Id, "recordaaaaaaa"+itoa(oseq+1), "c", map[string]any{"title": hookTitle})
		c2.Actor, c2.Tx = ActorNode, "tx"+itoa(oseq)
		return []proto.PushChange{c1, c2}
	}
	tok := s.token(t)
	st, ok, eb := rawPush(t, h, tok, pushReq(grp(1, "u", "s")...))
	if st != 200 || len(ok.Results) != 2 || ok.Results[0].Status != proto.ResApplied || ok.Results[1].Status != proto.ResApplied {
		t.Fatalf("user + hook member: %d %+v %+v", st, ok, eb)
	}
	// the hook-written member needs the user's rights: it replays as the service actor and is refused
	st, ok, eb = rawPush(t, h, tok, pushReq(grp(3, "u", "u")...))
	if st != 200 || ok.Results[0].Status != proto.ResRejected || ok.Results[0].Code != proto.CodeRuleDenied {
		t.Fatalf("hook member must not borrow the user: %d %+v %+v", st, ok, eb)
	}
	// without a service actor the group is refused
	h2 := newHub(t)
	h2.policy(t, "items", DirBoth, nil, nil)
	_, code2, _ := CreateEnrollment(h2.app, EnrollOptions{Name: "bare", Profile: "edge"})
	b := newItemsSpokeWith(t, h2, "bare", code2)
	baid := grant(t, b, h2.usr)
	c1 := pc(b.m.NodeID(), 1, nowHLC(-5000, 1), 0, h2.items.Id, "recordbbbbbbbb1", "c", map[string]any{"title": "x"})
	c1.Actor, c1.Tx = baid, "t"
	c2 := pc(b.m.NodeID(), 2, nowHLC(-4000, 1), 0, h2.items.Id, "recordbbbbbbbb2", "c", map[string]any{"title": "y"})
	c2.Actor, c2.Tx = ActorNode, "t"
	st, ok, _ = rawPush(t, h2, b.token(t), pushReq(c1, c2))
	if st != 200 || ok.Results[0].Code != proto.CodeActorUnknown {
		t.Fatalf("no service actor: %d %+v", st, ok)
	}
}

func TestMixedGrantsInOneGroupAreRefused(t *testing.T) { // P4-10 / review test 2
	h, a, b := actorHub(t)
	aid := grant(t, a, h.usr)
	bid := grant(t, b, h.usr)
	_ = bid
	c1 := pc(a.m.NodeID(), 1, nowHLC(-5000, 1), 0, h.items.Id, "recordcccccccc1", "c", map[string]any{"title": "x"})
	c1.Actor, c1.Tx = aid, "t"
	c2 := pc(a.m.NodeID(), 2, nowHLC(-4000, 1), 0, h.items.Id, "recordcccccccc2", "c", map[string]any{"title": "y"})
	c2.Actor, c2.Tx = "gother", "t"
	st, ok, _ := rawPush(t, h, a.token(t), pushReq(c1, c2))
	if st != 200 || ok.Results[0].Code != proto.CodeActorUnknown {
		t.Fatalf("%d %+v", st, ok)
	}
}

func TestTokenKeyChangeParksTheGrant(t *testing.T) { // review test 3
	h, a, _ := actorHub(t)
	aid := grant(t, a, h.usr)
	code, out := a.asUser(t, aid, "POST", "/api/collections/items/records", `{"title":"before password change"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	u, _ := h.app.FindRecordById("users", h.usr.Id)
	u.RefreshTokenKey()
	if err := h.app.Save(u); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()})
	if len(rows) != 1 || rows[0].Status != StatusParked {
		t.Fatalf("hub rows %+v", rows)
	}
}

func TestSuperuserServiceActorNeedsAnExplicitFlag(t *testing.T) { // P4-8
	h := newHub(t)
	if _, _, err := CreateEnrollment(h.app, EnrollOptions{Name: "su1", Profile: "edge", Actor: core.CollectionNameSuperusers + "/" + h.su.Id}); err == nil {
		t.Fatal("a superuser service actor must need --allow-superuser-actor")
	}
	var seen map[string]any
	SetAuditSink(func(action, _, _ string, d map[string]any) {
		if action == AuditNodeEnroll {
			seen = d
		}
	})
	defer SetAuditSink(nil)
	if _, _, err := CreateEnrollment(h.app, EnrollOptions{Name: "su2", Profile: "edge", Actor: core.CollectionNameSuperusers + "/" + h.su.Id, AllowSuperuserActor: true}); err != nil {
		t.Fatal(err)
	}
	if seen["service_actor_kind"] != "superuser" || seen["warning"] == "" {
		t.Fatalf("audit %+v", seen)
	}
}

func TestReplayTimeoutIsPermanentAfterRetries(t *testing.T) { // P4-9
	h, a, _ := actorHub(t)
	applyFault = func(*hubChange) error { return errors.New("batch transaction timeout") }
	defer func() { applyFault = nil }()
	tok := a.token(t)
	c := pc(a.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recorddddddddd1", "c", map[string]any{"title": "poison"})
	for i := 1; i < maxReplayTimeouts; i++ {
		if st, _, _ := rawPush(t, h, tok, pushReq(c)); st != http.StatusInternalServerError {
			t.Fatalf("attempt %d: %d", i, st)
		}
	}
	st, ok, _ := rawPush(t, h, tok, pushReq(c))
	if st != 200 || ok.Results[0].Status != proto.ResRejected || ok.Results[0].Code != CodeApplyError {
		t.Fatalf("after %d timeouts: %d %+v", maxReplayTimeouts, st, ok)
	}
	// the queue moves on and the lock is free
	applyFault = nil
	c2 := pc(a.m.NodeID(), 2, nowHLC(-900, 0), 0, h.items.Id, "recorddddddddd2", "c", map[string]any{"title": "fine"})
	if r := pushOne(t, h, a, c2); r.Status != proto.ResApplied {
		t.Fatalf("next change: %+v", r)
	}
}

func TestLiteralNullTextSurvivesCreate(t *testing.T) { // P4-11
	h, a, _ := actorHub(t)
	c := pc(a.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordeeeeeeee1", "c", map[string]any{"title": "null"})
	if r := pushOne(t, h, a, c); r.Status != proto.ResApplied {
		t.Fatalf("%+v", r)
	}
	if got := hubItem(t, h, "recordeeeeeeee1").GetString("title"); got != "null" {
		t.Fatalf("title %q", got)
	}
}

func TestRejectAuditNamesTheGrantUser(t *testing.T) { // P4-12
	h, a, b := actorHub(t)
	aid := grant(t, a, h.usr)
	var details map[string]any
	SetAuditSink(func(action, _, _ string, d map[string]any) {
		if action == AuditReject {
			details = d
		}
	})
	defer SetAuditSink(nil)
	c := pc(b.m.NodeID(), 1, nowHLC(-1000, 0), 0, h.items.Id, "recordffffffff1", "c", map[string]any{"title": "x"})
	c.Actor = aid
	if r := pushOne(t, h, b, c); r.Code != proto.CodeActorNodeMismatch {
		t.Fatalf("%+v", r)
	}
	if details["actor_id"] != h.usr.Id || details["actor_collection"] != "users" {
		t.Fatalf("audit %+v", details)
	}
}

func TestRevokeEndpointRefusesAnotherNode(t *testing.T) { // review test 8
	h, a, b := actorHub(t)
	aid := grant(t, a, h.usr)
	r, _ := http.NewRequest("DELETE", h.srv.URL+proto.PathActor+"/"+aid, nil)
	r.Header.Set("Authorization", "Bearer "+b.token(t))
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("status %d", res.StatusCode)
	}
	g, _ := loadGrant(h.app.DB(), aid)
	if g == nil || g.RevokedAt != "" {
		t.Fatal("the grant must stay valid")
	}
}
