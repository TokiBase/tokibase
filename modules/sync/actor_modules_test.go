//go:build !no_sync && !no_fieldperm && !no_batchguard && !no_webhooks

package sync

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/batchguard"
	"github.com/tokibase/tokibase/modules/fieldperm"
	"github.com/tokibase/tokibase/modules/webhooks"
	"github.com/tokibase/tokibase/tools/hook"
)

// Interaction tests with fieldperm, batchguard and webhooks (compiled out with their stubs).

func TestFieldpermWriteRuleEnforcedInReplay(t *testing.T) {
	h, a, _ := actorHub(t)
	fieldperm.Register(h.app)
	if _, err := fieldperm.Set(h.app, "items", "secret", fieldperm.SetOptions{Write: sp(`@request.auth.email = "nobody@example.com"`), WriteSet: true}); err != nil {
		t.Fatal(err)
	}
	r := a.create(t, map[string]any{"title": "t", "secret": "s3"})
	res := a.sync(t)
	if res.Rejected != 1 {
		t.Fatalf("result %+v", res)
	}
	if _, err := h.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("a write to a protected field was applied")
	}
	// a record without the protected field passes
	ok := a.create(t, map[string]any{"title": "plain"})
	a.sync(t)
	if _, err := h.app.FindRecordById("items", ok.Id); err != nil {
		t.Fatalf("plain record: %v", err)
	}
}

func TestPullHidesFieldpermReadFields(t *testing.T) {
	h, a, b := actorHub(t)
	fieldperm.Register(h.app)
	if _, err := fieldperm.Set(h.app, "items", "secret", fieldperm.SetOptions{Read: sp(`@request.auth.email = "nobody@example.com"`), ReadSet: true}); err != nil {
		t.Fatal(err)
	}
	r := h.create(t, map[string]any{"title": "pub", "secret": "classified"})
	a.sync(t)
	b.sync(t)
	got, err := b.app.FindRecordById("items", r.Id)
	if err != nil {
		t.Fatal(err)
	}
	if got.GetString("title") != "pub" || got.GetString("secret") != "" {
		t.Fatalf("pulled record: %v", got.FieldsData())
	}
}

func TestBatchguardRunsForTxGroupsOnly(t *testing.T) {
	h, a, _ := actorHub(t)
	batchguard.Register(h.app)
	if err := batchguard.EnsureCollection(h.app); err != nil {
		t.Fatal(err)
	}
	var batches atomic.Int32
	h.app.OnBatchRequest().Bind(&hook.Handler[*core.BatchRequestEvent]{Id: "count", Priority: -20000, Func: func(e *core.BatchRequestEvent) error {
		batches.Add(1)
		return e.Next()
	}})
	if _, err := batchguard.Save(h.app, batchguard.Rule{
		Name: "nope", Enabled: true, Match: []batchguard.Match{{Collection: "items", Method: "POST"}},
		Assert: "sum(items, qty) == 999", Message: "no way",
	}); err != nil {
		t.Fatal(err)
	}
	a.app.Settings().Batch.Enabled = true
	a.app.Settings().Batch.MaxRequests = 50
	if err := a.app.Save(a.app.Settings()); err != nil {
		t.Fatal(err)
	}
	// a single change: not a batch, the guard does not run
	single := a.create(t, map[string]any{"title": "single", "qty": 1})
	a.sync(t)
	if _, err := h.app.FindRecordById("items", single.Id); err != nil {
		t.Fatalf("single change: %v", err)
	}
	if batches.Load() != 0 {
		t.Fatalf("OnBatchRequest ran %d times for a single change", batches.Load())
	}
	// a tx group of two creates goes through OnBatchRequest and batchguard refuses it
	aid := grant(t, a, h.usr)
	body := `{"requests":[{"method":"POST","url":"/api/collections/items/records","body":{"title":"g1","qty":1}},{"method":"POST","url":"/api/collections/items/records","body":{"title":"g2","qty":2}}]}`
	if code, out := a.asUser(t, aid, "POST", "/api/batch", body); code != 200 {
		t.Fatalf("local batch: %d %s", code, out)
	}
	res := a.sync(t)
	if res.Rejected != 2 {
		t.Fatalf("tx group result %+v", res)
	}
	if batches.Load() != 1 {
		t.Fatalf("OnBatchRequest ran %d times for the tx group, want 1", batches.Load())
	}
	if n := countWhere(t, h.app, "items", "title IN ('g1','g2')", nil); n != 0 {
		t.Fatal("the group must be all or nothing")
	}
}

func TestWebhookFiresOnceOnHubNotOnSpokePull(t *testing.T) {
	t.Setenv("TOKI_WEBHOOK_ALLOW_PRIVATE", "1")
	h, a, b := actorHub(t)
	for _, app := range []core.App{h.app, a.app, b.app} {
		webhooks.Register(app)
		if _, err := webhooks.Add(app, webhooks.Webhook{Name: "w", URL: "http://127.0.0.1:9/x", Secret: "s3cret-s3cret-s3cret",
			Events: []string{"record.create"}, Collections: []string{"items"}, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(app core.App) int {
		rows, err := webhooks.ListDeliveries(app, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.Collection == "items" {
				n++
			}
		}
		return n
	}
	a.create(t, map[string]any{"title": "hook"})
	aLocal := count(a.app) // the originating spoke fires for its own write
	a.sync(t)
	b.sync(t)
	if n := count(h.app); n != 1 {
		t.Fatalf("hub deliveries %d, want exactly 1", n)
	}
	if n := count(b.app); n != 0 {
		t.Fatalf("a pulled change must not fire a webhook on the spoke (%d)", n)
	}
	if n := count(a.app); n != aLocal {
		t.Fatalf("the origin spoke fired again (%d -> %d)", aLocal, n)
	}
}
