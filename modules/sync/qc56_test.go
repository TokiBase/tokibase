//go:build !no_sync

package sync

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// QC of PR5/PR6 (docs/SYNC_DESIGN.md review P56-*).

// ---- P56-1: cascade side effects of a replicated delete are replica writes ----

func childCollections(t *testing.T, app core.App, tickets *core.Collection) {
	t.Helper()
	open := ""
	notes := core.NewBaseCollection("notes")
	notes.Id = "pbc_notes"
	notes.Fields.Add(&core.TextField{Name: "title"},
		&core.RelationField{Name: "ticket", CollectionId: tickets.Id, MaxSelect: 1},
		&core.AutodateField{Name: "created", OnCreate: true}, &core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	logs := core.NewBaseCollection("logs")
	logs.Id = "pbc_logs"
	logs.Fields.Add(&core.TextField{Name: "title"},
		&core.RelationField{Name: "ticket", CollectionId: tickets.Id, MaxSelect: 1, CascadeDelete: true},
		&core.AutodateField{Name: "created", OnCreate: true}, &core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true})
	for _, c := range []*core.Collection{notes, logs} {
		c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
}

func saveChild(t *testing.T, app core.App, col, ticket string) *core.Record {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(col)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("title", col)
	r.Set("ticket", ticket)
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestSpokeCascadeOfReplicatedParentChangeIsNotCaptured(t *testing.T) {
	h := newTicketHub(t)
	childCollections(t, h.app, h.col)
	for _, c := range []string{"notes", "logs"} {
		if _, err := SetPolicy(h.app, c, PolicyChange{}); err != nil {
			t.Fatal(err)
		}
	}
	a := newTicketSpoke(t, h, "gate-a", "A")
	childCollections(t, a.app, mustCol(t, a.app, "tickets"))
	a.sync(t)

	// evict: the ticket moves to branch B
	t1 := h.ticket(t, "t1", "A")
	n1 := saveChild(t, h.app, "notes", t1.Id)
	l1 := saveChild(t, h.app, "logs", t1.Id)
	a.sync(t)
	if a.has(t1.Id) == nil || mustFind(a.app, "notes", n1.Id) == nil || mustFind(a.app, "logs", l1.Id) == nil {
		t.Fatal("the spoke must hold the ticket and its children")
	}
	t1.Set("branch", "B")
	if err := h.app.Save(t1); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if a.has(t1.Id) != nil {
		t.Fatal("the ticket is evicted")
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes", nil); n != 0 {
		t.Fatalf("cascade children of an evict are replica side effects, not local changes (%d rows)", n)
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_tombstones", nil); n != 0 {
		t.Fatalf("an evict leaves no tombstone, not even for cascade children (%d)", n)
	}
	// the hub is untouched
	if hn := mustFind(h.app, "notes", n1.Id); hn == nil || hn.GetString("ticket") != t1.Id {
		t.Fatal("the hub note must still reference the ticket")
	}
	if mustFind(h.app, "logs", l1.Id) == nil {
		t.Fatal("the hub log must still exist")
	}
	a.sync(t) // would push the leaked rows
	if hn := mustFind(h.app, "notes", n1.Id); hn.GetString("ticket") != t1.Id {
		t.Fatal("a push after the evict must not detach the hub note")
	}

	// delete and purge pulled by the spoke
	for _, how := range []string{"delete", "purge"} {
		t2 := h.ticket(t, how, "A")
		n2 := saveChild(t, h.app, "notes", t2.Id)
		a.sync(t)
		if mustFind(a.app, "notes", n2.Id) == nil {
			t.Fatal("note not pulled")
		}
		if how == "delete" {
			if err := h.app.Delete(t2); err != nil {
				t.Fatal(err)
			}
		} else if _, err := h.m.Purge("tickets", t2.Id, "test", "cli", true); err != nil {
			t.Fatal(err)
		}
		a.sync(t)
		a.sync(t)
		if a.has(t2.Id) != nil {
			t.Fatalf("%s: the ticket is gone on the spoke", how)
		}
		if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes", nil); n != 0 {
			t.Fatalf("%s: spoke captured %d local rows for cascade children", how, n)
		}
		if hn := mustFind(h.app, "notes", n2.Id); hn == nil || hn.GetString("ticket") != "" {
			t.Fatalf("%s: the hub performs the cascade itself (note=%v)", how, hn)
		}
	}
}

func mustCol(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustFind(app core.App, col, id string) *core.Record {
	r, _ := app.FindRecordById(col, id)
	return r
}

// A cascade on the HUB during a push replay is still captured (other nodes need it).
func TestHubPushCascadeStillCaptured(t *testing.T) {
	h := newTicketHub(t)
	childCollections(t, h.app, h.col)
	for _, c := range []string{"notes", "logs"} {
		if _, err := SetPolicy(h.app, c, PolicyChange{}); err != nil {
			t.Fatal(err)
		}
	}
	a := newTicketSpoke(t, h, "gate-a", "A")
	childCollections(t, a.app, mustCol(t, a.app, "tickets"))
	b := newTicketSpoke(t, h, "gate-b", "A")
	childCollections(t, b.app, mustCol(t, b.app, "tickets"))
	r := a.create(t, "from a", "A")
	a.sync(t)
	n := saveChild(t, h.app, "notes", r.Id)
	b.sync(t)
	rr, _ := a.app.FindRecordById("tickets", r.Id)
	if err := a.app.Delete(rr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if hn := mustFind(h.app, "notes", n.Id); hn == nil || hn.GetString("ticket") != "" {
		t.Fatalf("the hub cascade unsets the relation: %v", hn)
	}
	if bn := mustFind(b.app, "notes", n.Id); bn == nil || bn.GetString("ticket") != "" {
		t.Fatalf("b receives the hub cascade: %v", bn)
	}
}

// ---- P56-3: no evict/delete rows for records a node never received ----

func TestViewRuleEvictAndDeleteOnlyForRecordsTheNodeHad(t *testing.T) {
	h, a := viewHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("qty < 100"))
	vis := h.create(t, map[string]any{"title": "visible", "qty": 5})
	hid := h.create(t, map[string]any{"title": "hidden", "qty": 500})
	a.sync(t)
	hid.Set("qty", 600) // an update of a record the node never had
	if err := h.app.Save(hid); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Delete(hid); err != nil { // and its delete
		t.Fatal(err)
	}
	vis.Set("qty", 500) // a record the node has becomes invisible
	if err := h.app.Save(vis); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	_, pr := rawPull(t, h, a.token(t), "after=0&limit=1000")
	sawVisEvict := false
	for _, c := range pr.Changes {
		if c.Record == hid.Id {
			t.Fatalf("the node never had %s but learns about it: %+v", hid.Id, c)
		}
		if c.Record == vis.Id && c.Op == "x" {
			sawVisEvict = true
		}
	}
	if !sawVisEvict || a.has(vis.Id) != nil {
		t.Fatal("a record the node had is evicted")
	}
	// a delete of a record the node had is delivered even when it is hidden by then
	vis2 := h.create(t, map[string]any{"title": "v2", "qty": 1})
	a.sync(t)
	if a.has(vis2.Id) == nil {
		t.Fatal("v2 not pulled")
	}
	if err := h.app.Delete(vis2); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if a.has(vis2.Id) != nil {
		t.Fatal("the delete of a record the node had must arrive")
	}
}

func TestLegacyNodeKeepsTheOldEvictBehaviour(t *testing.T) {
	h, a := viewHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("qty < 100"))
	if _, err := h.app.NonconcurrentDB().NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, '1')").
		Bind(dbx.Params{"k": "sent_legacy:" + a.m.NodeID()}).Execute(); err != nil {
		t.Fatal(err)
	}
	hid := h.create(t, map[string]any{"title": "hidden", "qty": 500})
	if err := h.app.Delete(hid); err != nil {
		t.Fatal(err)
	}
	_, pr := rawPull(t, h, a.token(t), "after=0&limit=1000")
	for _, c := range pr.Changes {
		if c.Record == hid.Id && c.Op == "d" {
			return
		}
	}
	t.Fatal("a node that pulled before sent tracking existed still gets delete rows")
}

// ---- P56-4: hidden partition field ----

func TestHiddenPartitionFieldIsAnError(t *testing.T) {
	h := newHub(t)
	c := core.NewBaseCollection("hid")
	c.Fields.Add(&core.TextField{Name: "branch", Hidden: true})
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	_, err := SetPolicy(h.app, "hid", PolicyChange{Partition: strp("branch = @node.branch")})
	if err == nil || !strings.Contains(err.Error(), "hidden") {
		t.Fatalf("a hidden partition field must be refused, got %v", err)
	}
}

func TestPartitionOnlyChangeStillProducesARow(t *testing.T) {
	h := newTicketHub(t)
	a := newTicketSpoke(t, h, "gate-a", "A")
	tk := h.ticket(t, "t", "A")
	a.sync(t)
	if a.has(tk.Id) == nil {
		t.Fatal("pulled")
	}
	// the field becomes hidden later (a legacy row): its value no longer travels
	col := mustCol(t, h.app, "tickets")
	col.Fields.GetByName("branch").SetHidden(true)
	if err := h.app.Save(col); err != nil {
		t.Fatal(err)
	}
	tk, _ = h.app.FindRecordById("tickets", tk.Id)
	tk.Set("branch", "B")
	if err := h.app.Save(tk); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE record={:r} AND op='u' AND part_old='A' AND part_new='B'", dbx.Params{"r": tk.Id}); n != 1 {
		t.Fatalf("a partition-only change needs a change row (%d)", n)
	}
	a.sync(t)
	if a.has(tk.Id) != nil {
		t.Fatal("node A must lose the record that left its partition")
	}
}

// ---- P56-5: malformed policy rows fail closed ----

func TestMalformedPolicyRowIsNotSynced(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"partition", "qty == @node.qty"},
		{"exclude", "secret"},
		{"field_types", "counter"},
		{"strategy", "bogus"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			e := setup(t)
			pr, err := e.app.FindFirstRecordByFilter(PoliciesCollection, "collection='items'")
			if err != nil {
				t.Fatal(err)
			}
			pr.Set(tc.field, tc.value)
			if err := e.app.SaveNoValidate(pr); err != nil {
				t.Fatal(err)
			}
			if p := e.pol(t); p != nil {
				t.Fatalf("%s: a policy that cannot be parsed must not sync the collection (direction %q)", tc.field, p.Direction)
			}
			e.item(t, "title", "x")
			if n := e.count(t, "_changes"); n != 0 {
				t.Fatalf("no change row for a collection with an invalid policy (%d)", n)
			}
			issues, err := LintPolicies(e.app)
			if err != nil {
				t.Fatal(err)
			}
			if tc.field != "strategy" { // the select field enforces strategy on validation only
				found := false
				for _, i := range issues {
					if i.Level == "error" {
						found = true
					}
				}
				if !found {
					t.Fatalf("lint must report the bad %s: %+v", tc.field, issues)
				}
			}
		})
	}
}

// ---- P56-6/7/8: purge completeness ----

func TestPurgeErasesSpokeConflictCopiesAndNameKeyedRows(t *testing.T) {
	h, a, b := hubFixture(t)
	r := a.create(t, map[string]any{"title": "personal", "qty": 1})
	a.sync(t)
	b.sync(t)
	// a spoke-local conflict copy (superseded / discarded patches live here)
	cc, err := b.app.FindCollectionByNameOrId(ConflictsCollection)
	if err != nil {
		t.Fatal(err)
	}
	cr := core.NewRecord(cc)
	cr.Set("collection", "items") // by NAME: the spoke stores what the pull said, either form must match
	cr.Set("record", r.Id)
	cr.Set("kind", "concurrent_field")
	cr.Set("resolution", "auto_lww")
	cr.Set("status", "resolved")
	cr.Set("incoming", map[string]any{"title": "personal-copy"})
	cr.Set("note", "contains personal-copy")
	if err := b.app.SaveNoValidate(cr); err != nil {
		t.Fatal(err)
	}
	// a hub row stored under the collection NAME (an old row or a hostile node)
	if _, err := h.app.NonconcurrentDB().NewQuery(`INSERT INTO _changes (node, origin_seq, hlc, collection, record, op, patch, schema_version, status, created)
  VALUES ('rogue', 1, 1, 'items', {:r}, 'u', '{"title":"personal-by-name"}', 1, 'rejected', '2026-01-01 00:00:00.000Z')`).
		Bind(dbx.Params{"r": r.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	res, err := h.m.Purge("items", r.Id, "gdpr", "cli", true)
	if err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE patch LIKE '%personal%'", nil); n != 0 {
		t.Fatalf("a hub row under the collection name survived (%d), result %+v", n, res)
	}
	b.sync(t)
	if n := countRows(t, b.app, "SELECT COUNT(*) FROM _sync_conflicts WHERE incoming LIKE '%personal%' OR note LIKE '%personal%'", nil); n != 0 {
		t.Fatalf("the spoke-local conflict copy survived the purge (%d)", n)
	}
	// purge again: still fine, nothing left to blank
	if res, err = h.m.Purge("items", r.Id, "again", "cli", true); err != nil || !res.Already {
		t.Fatalf("repeat purge: %+v %v", res, err)
	}
}

func TestPurgeWorksForACollectionWithoutPolicy(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := a.create(t, map[string]any{"title": "old data", "qty": 1})
	a.sync(t)
	if _, err := SetPolicy(h.app, "items", PolicyChange{Direction: strp("none")}); err != nil {
		t.Fatal(err)
	}
	res, err := h.m.Purge("items", r.Id, "the policy was switched off, the data is still in the log", "cli", true)
	if err != nil {
		t.Fatalf("purge is hub-local and needs no policy: %v", err)
	}
	if res.Blanked < 1 {
		t.Fatalf("%+v", res)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE patch LIKE '%old data%'", nil); n != 0 {
		t.Fatal("patch survived")
	}
	// system collections still refuse
	if _, err := h.m.Purge("_sync_nodes", "x", "r", "cli", true); err == nil {
		t.Fatal("a system collection is not purgeable")
	}
}

func TestPushByCollectionNameIsStoredUnderTheID(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	node := a.m.NodeID()
	st, resp, eb := rawPush(t, h, tok, pushReq(pc(node, 1, nowHLC(-1000, 0), 0, "items", "orphanrec00001x", OpUpdate, map[string]any{"title": "leak"})))
	if st != 200 || len(resp.Results) != 1 || resp.Results[0].Status != proto.ResRejected {
		t.Fatalf("%d %+v %+v", st, resp, eb)
	}
	rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": node})
	if len(rows) == 0 || rows[0].Collection != h.items.Id {
		t.Fatalf("rows must be keyed by the collection id: %+v", rows)
	}
	if _, err := h.m.Purge("items", "orphanrec00001x", "r", "cli", true); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE patch LIKE '%leak%'", nil); n != 0 {
		t.Fatal("the refused patch survived the purge")
	}
}

// ---- P56-9/10: hook budget, merge validation ----

func TestHookBudgetIsSharedByAPush(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHook, "", false)
	old := hookPushBudget
	hookPushBudget = 300 * time.Millisecond
	t.Cleanup(func() { hookPushBudget = old })
	var calls atomic.Int32
	bindHook(t, h, func(e *kernel.SyncConflictEvent) error {
		calls.Add(1)
		time.Sleep(time.Until(e.Deadline) + 20*time.Millisecond) // a guest that runs into the deadline
		return errors.New("timed out")
	})
	r := h.create(t, map[string]any{"title": "x", "qty": 1})
	tok := a.token(t)
	node := a.m.NodeID()
	var cs []proto.PushChange
	for i := int64(1); i <= 20; i++ {
		cs = append(cs, pc(node, i, nowHLC(-1000+i, 0), 0, h.items.Id, r.Id, OpUpdate, map[string]any{"title": "v" + string(rune('a'+i))}))
	}
	start := time.Now()
	st, resp, eb := rawPush(t, h, tok, pushReq(cs...))
	if st != 200 {
		t.Fatalf("%d %+v", st, eb)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the hook budget must bound the apply lock, took %v", d)
	}
	if n := calls.Load(); n > 2 {
		t.Fatalf("after the budget is spent the guests are not asked again (%d calls)", n)
	}
	for _, res := range resp.Results {
		if res.Status != proto.ResParked {
			t.Fatalf("every change parks: %+v", res)
		}
	}
}

func TestHookMergePatchIsValidated(t *testing.T) {
	h := newTicketHub(t)
	if _, err := SetPolicy(h.app, "tickets", PolicyChange{Strategy: strp(StratHook)}); err != nil {
		t.Fatal(err)
	}
	a := newTicketSpoke(t, h, "gate-a", "A")
	tk := h.ticket(t, "t", "A")
	a.sync(t)
	merge := map[string]any{}
	bindHook(t, h.hubEnv, func(e *kernel.SyncConflictEvent) error {
		e.Resolution, e.Patch = kernel.SyncResolveMerge, merge
		return nil
	})
	tok := a.token(t)
	node := a.m.NodeID()
	seq := int64(0)
	push := func(m map[string]any) proto.PushResult {
		t.Helper()
		for k := range merge {
			delete(merge, k)
		}
		for k, v := range m {
			merge[k] = v
		}
		seq++
		st, resp, eb := rawPush(t, h.hubEnv, tok, pushReq(pc(node, seq, nowHLC(-500+seq, 0), 0, "pbc_tickets", tk.Id, OpUpdate, map[string]any{"title": "n"})))
		if st != 200 {
			t.Fatalf("%d %+v", st, eb)
		}
		return resp.Results[0]
	}
	if res := push(map[string]any{"fee": 0}); res.Status != proto.ResRejected || res.Code != proto.CodeValidationFailed {
		t.Fatalf("an absolute counter from a guest: %+v", res)
	}
	if res := push(map[string]any{"branch": "B"}); res.Status != proto.ResRejected || res.Code != proto.CodePolicyPartition {
		t.Fatalf("a guest moving the record out of the partition: %+v", res)
	}
	if got, _ := h.app.FindRecordById("tickets", tk.Id); got.GetString("branch") != "A" {
		t.Fatal("the record must not have moved")
	}
	if res := push(map[string]any{"title": "merged"}); res.Status != proto.ResMerged && res.Status != proto.ResApplied {
		t.Fatalf("a valid merge still works: %+v", res)
	}
}

// ---- P56-11: parked create / delete / tx group ----

func parkChanges(t *testing.T, h *hubEnv, node string, cs ...proto.PushChange) {
	t.Helper()
	h.m.applyMu.Lock()
	defer h.m.applyMu.Unlock()
	err := h.app.RunInTransaction(func(tx kernel.App) error {
		for _, c := range cs {
			if _, err := h.m.recordParked(tx, node, mustChange(t, node, c), &rejection{code: CodeHookParked, msg: "parked", park: true}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveParkedCreateDeleteAndGroup(t *testing.T) {
	h, a, _ := hubFixture(t)
	node := a.m.NodeID()
	cid := h.items.Id

	// create
	parkChanges(t, h, node, pc(node, 1, nowHLC(-900, 0), 0, cid, "parkedcreate001", OpCreate, map[string]any{"title": "created by admin"}))
	open, _ := ListConflicts(h.app, true, "")
	if len(open) != 1 {
		t.Fatalf("%+v", open)
	}
	if err := ResolveConflict(h.app, ResolveOptions{ID: open[0].ID, Take: TakeIncoming}); err != nil {
		t.Fatalf("a parked create is resolvable: %v", err)
	}
	if r := mustFind(h.app, "items", "parkedcreate001"); r == nil || r.GetString("title") != "created by admin" {
		t.Fatalf("created: %v", r)
	}

	// delete
	victim := h.create(t, map[string]any{"title": "to delete"})
	parkChanges(t, h, node, pc(node, 2, nowHLC(-800, 0), 0, cid, victim.Id, OpDelete, map[string]any{}))
	open, _ = ListConflicts(h.app, true, "")
	if len(open) != 1 {
		t.Fatalf("%+v", open)
	}
	if err := ResolveConflict(h.app, ResolveOptions{ID: open[0].ID, Take: TakeIncoming}); err != nil {
		t.Fatal(err)
	}
	if mustFind(h.app, "items", victim.Id) != nil {
		t.Fatal("accepting a parked delete deletes the record")
	}
	if tombKind(t, h.app, cid, victim.Id) != "delete" {
		t.Fatal("and leaves the delete tombstone")
	}

	// tx group: resolved as a unit
	c1 := pc(node, 3, nowHLC(-700, 0), 0, cid, "grouprecord0001", OpCreate, map[string]any{"title": "g1"})
	c2 := pc(node, 4, nowHLC(-699, 0), 0, cid, "grouprecord0002", OpCreate, map[string]any{"title": "g2"})
	c1.Tx, c2.Tx = "grp-1", "grp-1"
	parkChanges(t, h, node, c1, c2)
	open, _ = ListConflicts(h.app, true, "")
	if len(open) != 2 {
		t.Fatalf("%+v", open)
	}
	if err := ResolveConflict(h.app, ResolveOptions{ID: open[0].ID, Take: TakeIncoming}); err != nil {
		t.Fatal(err)
	}
	if mustFind(h.app, "items", "grouprecord0001") == nil || mustFind(h.app, "items", "grouprecord0002") == nil {
		t.Fatal("both members of the group are applied together")
	}
	if open, _ = ListConflicts(h.app, true, ""); len(open) != 0 {
		t.Fatalf("all group conflicts are resolved: %+v", open)
	}
	if err := ResolveConflict(h.app, ResolveOptions{ID: "x", Take: TakePatch, Data: map[string]any{}}); err == nil {
		t.Fatal("unknown id")
	}
}

// ---- P56-12: pull_view_rule default on every creation path ----

func TestPullViewRuleDefaultsToTrueOnEveryPath(t *testing.T) {
	e := setup(t) // e.policy saves with app.Save and never sets the flag
	if !e.pol(t).PullViewRule {
		t.Fatal("a policy created by app.Save must default pull_view_rule to true")
	}
	e2 := setupWith(t, newApp(t), RoleHub)
	if _, err := SetPolicy(e2.app, "items", PolicyChange{PullViewRule: bp(false)}); err != nil {
		t.Fatal(err)
	}
	p, err := e2.m.pol.For(e2.items)
	if err != nil || p == nil || p.PullViewRule {
		t.Fatalf("an explicit false stays false: %v %+v", err, p)
	}
}

// ---- P56-15: a policy switched to field-merge has no clocks yet ----

func TestStrategySwitchToFieldMergeDoesNotLetOldEditsThrough(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "v1"})
	hr := hubItem(t, h, r.Id)
	hr.Set("title", "hub v2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	metaMs, _, _ := readMeta(h.app.DB(), h.items.Id, r.Id)
	h.strategy(t, "items", StratFieldMerge, "", false)
	old := hlc.Make(hlc.HLC(metaMs).PhysicalMs()-5000, 0)
	_, resp, eb := rawPush(t, h, a.token(t), pushReq(pc(a.m.NodeID(), 1, old, hlc.Make(hlc.HLC(metaMs).PhysicalMs()-6000, 0), h.items.Id, r.Id, OpUpdate, map[string]any{"title": "older edit"})))
	if len(resp.Results) != 1 {
		t.Fatalf("%+v", eb)
	}
	if res := resp.Results[0]; res.Status != proto.ResSuperseded {
		t.Fatalf("an older concurrent edit must lose: %+v", res)
	}
	if hubItem(t, h, r.Id).GetString("title") != "hub v2" {
		t.Fatal("hub value overwritten")
	}
}
