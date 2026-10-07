//go:build !no_fieldperm

package fieldperm

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/subscriptions"
)

// relEnv adds posts(clan -> clans) and comments(post -> posts), all open.
type relEnv struct {
	*env
	post *core.Record
}

func setupRel(t *testing.T) *relEnv {
	t.Helper()
	e := setup(t)
	r := e.seed(t)
	open := ""
	clans, _ := e.app.FindCollectionByNameOrId("clans")
	posts := core.NewBaseCollection("posts")
	posts.Fields.Add(&core.TextField{Name: "title"}, &core.RelationField{Name: "clan", CollectionId: clans.Id, MaxSelect: 1})
	posts.ListRule, posts.ViewRule = &open, &open
	if err := e.app.Save(posts); err != nil {
		t.Fatal(err)
	}
	comments := core.NewBaseCollection("comments")
	comments.Fields.Add(&core.RelationField{Name: "post", CollectionId: posts.Id, MaxSelect: 1})
	comments.ListRule, comments.ViewRule = &open, &open
	if err := e.app.Save(comments); err != nil {
		t.Fatal(err)
	}
	p := core.NewRecord(posts)
	p.Set("title", "hello")
	p.Set("clan", r.Id)
	if err := e.app.Save(p); err != nil {
		t.Fatal(err)
	}
	c := core.NewRecord(comments)
	c.Set("post", p.Id)
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := Set(e.app, "posts", "clan", SetOptions{ReadSet: true, Read: s("@request.auth.id = '" + e.owner.Id + "'")}); err != nil {
		t.Fatal(err)
	}
	return &relEnv{env: e, post: p}
}

func expandOf(m map[string]any) map[string]any {
	x, _ := m["expand"].(map[string]any)
	return x
}

func TestHiddenRelationNotLeakedViaExpand(t *testing.T) {
	e := setupRel(t)
	_, b := e.do(t, e.other, "GET", "/api/collections/posts/records/"+e.post.Id+"?expand=clan", "")
	if _, has := b["clan"]; has {
		t.Fatalf("field leaked: %v", b)
	}
	if x := expandOf(b); x["clan"] != nil {
		t.Fatalf("expand leaked: %v", b)
	}
	// list
	_, b = e.do(t, e.other, "GET", "/api/collections/posts/records?expand=clan", "")
	for _, it := range b["items"].([]any) {
		if x := expandOf(it.(map[string]any)); x["clan"] != nil {
			t.Fatalf("list expand leaked: %v", it)
		}
	}
	// the allowed viewer still gets it
	_, b = e.do(t, e.owner, "GET", "/api/collections/posts/records/"+e.post.Id+"?expand=clan", "")
	if expandOf(b)["clan"] == nil {
		t.Fatalf("owner must keep the expand: %v", b)
	}
}

func TestHiddenRelationNotLeakedViaNestedExpand(t *testing.T) {
	e := setupRel(t)
	_, b := e.do(t, e.other, "GET", "/api/collections/comments/records?expand=post.clan", "")
	it := b["items"].([]any)[0].(map[string]any)
	post, _ := expandOf(it)["post"].(map[string]any)
	if post == nil {
		t.Fatalf("post must be expanded: %v", it)
	}
	if x := expandOf(post); x["clan"] != nil {
		t.Fatalf("nested expand leaked: %v", post)
	}
	_, b = e.do(t, e.owner, "GET", "/api/collections/comments/records?expand=post.clan", "")
	post, _ = expandOf(b["items"].([]any)[0].(map[string]any))["post"].(map[string]any)
	if expandOf(post)["clan"] == nil {
		t.Fatalf("owner must keep nested expand: %v", post)
	}
}

func TestHiddenRelationNotLeakedViaBackRelation(t *testing.T) {
	e := setupRel(t)
	clan, _ := e.app.FindFirstRecordByFilter("clans", "title='alpha'")
	_, b := e.do(t, e.other, "GET", "/api/collections/clans/records/"+clan.Id+"?expand=posts_via_clan", "")
	if x := expandOf(b); x["posts_via_clan"] != nil {
		t.Fatalf("back-relation leaked: %v", b)
	}
	_, b = e.do(t, e.owner, "GET", "/api/collections/clans/records/"+clan.Id+"?expand=posts_via_clan", "")
	if expandOf(b)["posts_via_clan"] == nil {
		t.Fatalf("owner must see back-relation: %v", b)
	}
}

func TestHiddenRelationNotLeakedViaRealtime(t *testing.T) {
	e := setupRel(t)
	client := subscriptions.NewDefaultClient()
	client.Set(apis.RealtimeClientAuthKey, e.other)
	client.Subscribe(`posts/*?options={"query":{"expand":"clan"}}`)
	e.app.SubscriptionsBroker().Register(client)

	e.post.Set("title", "changed")
	if err := e.app.Save(e.post); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-client.Channel():
		data := string(msg.Data)
		for _, leak := range []string{"s3cret", `"clan":`} {
			if contains(data, leak) {
				t.Fatalf("realtime leaked %s: %s", leak, data)
			}
		}
		if !contains(data, "changed") {
			t.Fatalf("unexpected message: %s", data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no realtime message")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestFailsClosedWhenRulesNeverLoaded(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	now := time.Now()
	e.m.now = func() time.Time { return now }
	var calls atomic.Int32
	failing := true
	e.m.load = func() ([]Rule, error) {
		calls.Add(1)
		if failing {
			return nil, errors.New("db busy")
		}
		return List(e.app, "")
	}
	e.m.Invalidate()
	url := "/api/collections/clans/records/" + r.Id

	_, b := e.do(t, e.other, "GET", url, "")
	if b["title"] != nil || b["secret"] != nil || b["id"] != r.Id {
		t.Fatalf("all fields must be hidden while rules are unavailable: %v", b)
	}
	if code, _ := e.do(t, e.other, "PATCH", url, `{"title":"x"}`); code != 503 {
		t.Fatalf("write must be refused, got %d", code)
	}
	if _, b := e.do(t, e.su, "GET", url, ""); b["secret"] != "s3cret" {
		t.Fatal("superusers are not affected")
	}
	for i := 0; i < 5; i++ {
		e.do(t, e.other, "GET", url, "")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("failed load must be cached for failTTL, calls=%d", n)
	}

	failing = false
	now = now.Add(failTTL + time.Second)
	if _, b := e.do(t, e.other, "GET", url, ""); b["title"] != "alpha" {
		t.Fatalf("must recover after the backoff: %v", b)
	}
}

func TestStaleRulesKeptWhenRefreshFails(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s("leader = @request.auth.id")})
	now := time.Now()
	e.m.now = func() time.Time { return now }
	url := "/api/collections/clans/records/" + r.Id
	e.do(t, e.other, "GET", url, "") // load
	e.m.load = func() ([]Rule, error) { return nil, errors.New("db busy") }
	now = now.Add(cacheTTL + time.Second)
	_, b := e.do(t, e.other, "GET", url, "")
	if b["secret"] != nil || b["title"] != "alpha" {
		t.Fatalf("stale rules must keep applying: %v", b)
	}
}

func TestUnchangedValueAllowedOnUpdate(t *testing.T) {
	e := setup(t)
	r := e.seed(t)
	e.rule(t, "secret", SetOptions{WriteSet: true, Write: s("")}) // locked
	url := "/api/collections/clans/records/" + r.Id
	if code, b := e.do(t, e.other, "PATCH", url, `{"title":"beta","secret":"s3cret"}`); code != 200 {
		t.Fatalf("re-sending the stored value must pass: %d %v", code, b)
	}
	if code, _ := e.do(t, e.other, "PATCH", url, `{"secret":"changed"}`); code != 400 {
		t.Fatalf("a real change must still be denied: %d", code)
	}
}

func TestRequestOnly(t *testing.T) {
	yes := []string{`@request.auth.id != ""`, `@request.auth.role = 'a b' && @request.method = "GET"`, `true`, `@request.auth.id:isset = true`, `@request.auth.created > @now`}
	no := []string{`leader = @request.auth.id`, `@request.body.x = 1`, `@collection.users.id ?= @request.auth.id`, `@request.auth.id != "" && owner = 'x'`}
	for _, r := range yes {
		if !requestOnly(r) {
			t.Errorf("%q should be request-only", r)
		}
	}
	for _, r := range no {
		if requestOnly(r) {
			t.Errorf("%q must stay per-record", r)
		}
	}
}

func TestRequestOnlyRuleEvaluatedOncePerList(t *testing.T) {
	e := setup(t)
	for i := 0; i < 20; i++ {
		e.seed(t)
	}
	e.rule(t, "secret", SetOptions{ReadSet: true, Read: s(`@request.auth.id != ""`)})
	e.rule(t, "title", SetOptions{ReadSet: true, Read: s(`@request.auth.id != ""`)})
	e.m.Invalidate()

	var queries atomic.Int32
	for _, b := range []dbx.Builder{e.app.NonconcurrentDB(), e.app.ConcurrentDB()} {
		d, ok := b.(*dbx.DB)
		if !ok {
			t.Skip("cannot instrument the db")
		}
		d.QueryLogFunc = func(ctx context.Context, _ time.Duration, _ string, _ *sql.Rows, _ error) { queries.Add(1) }
	}
	code, b := e.do(t, e.other, "GET", "/api/collections/clans/records?perPage=20", "")
	if code != 200 || len(b["items"].([]any)) != 20 {
		t.Fatalf("%d %v", code, b)
	}
	t.Logf("queries=%d", queries.Load())
	if got := queries.Load(); got > 10 {
		t.Fatalf("query count %d: request-only rules must not run per record (%s)", got, "20 records x 2 fields")
	}
}
