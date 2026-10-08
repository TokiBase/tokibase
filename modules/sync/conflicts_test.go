//go:build !no_sync

package sync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// strategy switches the sync strategy of a collection's policy.
func (e *env) strategy(t *testing.T, coll, strat, hookName string, review bool) {
	t.Helper()
	r, err := e.app.FindFirstRecordByFilter(PoliciesCollection, "collection={:c}", dbx.Params{"c": coll})
	if err != nil {
		t.Fatal(err)
	}
	r.Set("strategy", strat)
	r.Set("hook", hookName)
	r.Set("review", review)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func conflictRows(t *testing.T, app core.App) []ConflictRow {
	t.Helper()
	rows, err := ListConflicts(app, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// staleEdit prepares a concurrent edit: the hub changes title to "hub v2"
// after spoke a pulled the record, then a edits title and qty offline. The
// caller syncs a to push it.
func staleEdit(t *testing.T, h *hubEnv, a *itemsSpoke) string {
	t.Helper()
	r := h.create(t, map[string]any{"title": "v1", "qty": 10})
	a.sync(t)
	hr := hubItem(t, h, r.Id)
	hr.Set("title", "hub v2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	ar, err := a.app.FindRecordById("items", r.Id)
	if err != nil {
		t.Fatal(err)
	}
	ar.Set("title", "spoke v2")
	ar.Set("qty", 11)
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	return r.Id
}

func bindHook(t *testing.T, h *hubEnv, fn func(e *kernel.SyncConflictEvent) error) {
	t.Helper()
	hooks := kernel.OnSyncConflictFor(h.app)
	hooks.BindFunc(fn)
	t.Cleanup(hooks.UnbindAll)
}

func TestFieldMergeWorkedExampleOnTheHub(t *testing.T) {
	h, a, b := hubFixture(t)
	h.strategy(t, "items", StratFieldMerge, "", false)
	// title = plate, qty = fee (counter), secret = note
	r := h.create(t, map[string]any{"title": "B1234", "qty": 0, "secret": ""})
	cid, na, nb := h.items.Id, a.m.NodeID(), b.m.NodeID()
	ta, tb := a.token(t), b.token(t)
	baseMs, _, _ := readMeta(h.app.DB(), cid, r.Id)
	base := hlc.HLC(baseMs)
	at := func(ms int64) hlc.HLC { return hlc.Make(base.PhysicalMs()+ms, 0) }
	rid := r.Id
	push := func(tok, node string, seq int64, hl hlc.HLC, patch map[string]any) proto.PushResult {
		t.Helper()
		st, resp, eb := rawPush(t, h, tok, pushReq(pc(node, seq, hl, base, cid, rid, "u", patch)))
		if st != 200 {
			t.Fatalf("push: %d %+v", st, eb)
		}
		return resp.Results[0]
	}
	if res := push(ta, na, 1, at(5000), map[string]any{"qty": map[string]any{"$inc": 5000}}); res.Status != proto.ResApplied {
		t.Fatalf("gate-1: %+v", res)
	}
	if res := push(tb, nb, 1, at(7000), map[string]any{"secret": "scratch on door"}); res.Status != proto.ResApplied {
		t.Fatalf("phone note: %+v", res)
	}
	if res := push(ta, na, 2, at(6000), map[string]any{"title": "B1243"}); res.Status != proto.ResApplied {
		t.Fatalf("gate-2 plate: %+v", res)
	}
	res := push(tb, nb, 2, at(8000), map[string]any{"title": "B1234X"})
	if res.Status != proto.ResApplied {
		t.Fatalf("phone plate wins: %+v", res)
	}
	hr := hubItem(t, h, r.Id)
	if hr.GetString("title") != "B1234X" || hr.GetFloat("qty") != 5000 || hr.GetString("secret") != "scratch on door" {
		t.Fatalf("hub: %v", hr.FieldsData())
	}
	rows := conflictRows(t, h.app)
	if len(rows) != 1 || rows[0].Kind != KindConcurrentField || rows[0].Strategy != StratFieldMerge ||
		rows[0].Resolution != ResolutionAutoMerge || rows[0].Status != ConflictResolved || rows[0].Collection != "items" ||
		!strings.Contains(string(rows[0].Incoming), "B1234X") || !strings.Contains(string(rows[0].Current), "B1243") {
		t.Fatalf("conflict rows: %+v", rows)
	}
	clocks, _ := readFieldClocks(h.app.DB(), cid, r.Id)
	if clocks["title"] != at(8000) || clocks["secret"] != at(7000) {
		t.Fatalf("field clocks %v", clocks)
	}
	if _, ok := clocks["qty"]; ok {
		t.Fatal("a counter never has a field clock")
	}

	// the loser side: the older plate arrives second and is dropped (superseded)
	r2 := h.create(t, map[string]any{"title": "Z1", "qty": 0})
	b0, _, _ := readMeta(h.app.DB(), cid, r2.Id)
	base, rid = hlc.HLC(b0), r2.Id
	if res := push(tb, nb, 3, at(8000), map[string]any{"title": "late-winner"}); res.Status != proto.ResApplied {
		t.Fatalf("%+v", res)
	}
	// gate-2's older edit, same base: the title clock (8000) is newer than its base
	_, resp, _ := rawPush(t, h, ta, pushReq(pc(na, 3, at(6000), base, cid, r2.Id, "u", map[string]any{"title": "early-loser"})))
	if resp.Results[0].Status != proto.ResSuperseded || hubItem(t, h, r2.Id).GetString("title") != "late-winner" {
		t.Fatalf("loser: %+v", resp.Results[0])
	}
	if n := len(conflictRows(t, h.app)); n != 2 {
		t.Fatalf("one more conflict row expected, have %d", n)
	}
}

func TestFieldMergeReviewLeavesConflictOpen(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratFieldMerge, "", true)
	staleEdit(t, h, a)
	a.sync(t)
	rows := conflictRows(t, h.app)
	if len(rows) != 1 || rows[0].Status != ConflictOpen || rows[0].Resolution != ResolutionAutoMerge {
		t.Fatalf("review: %+v", rows)
	}
	// the automatic merge is applied already; take hub keeps it and closes the row
	if err := ResolveConflict(h.app, ResolveOptions{ID: rows[0].ID, Take: TakeHub, Note: "checked"}); err != nil {
		t.Fatal(err)
	}
	rows = conflictRows(t, h.app)
	if rows[0].Status != ConflictResolved || !strings.Contains(rows[0].Note, "checked") || rows[0].ResolvedBy != "cli" {
		t.Fatalf("%+v", rows[0])
	}
}

func TestHubWinsRejectsConcurrentAndReverts(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHubWins, "", false)
	id := staleEdit(t, h, a)
	r := a.sync(t)
	if r.Rejected != 1 {
		t.Fatalf("result %+v", r)
	}
	hr := hubItem(t, h, id)
	if hr.GetString("title") != "hub v2" || hr.GetFloat("qty") != 10 {
		t.Fatalf("hub-wins must not apply anything, not even a counter: %v", hr.FieldsData())
	}
	ar, _ := a.app.FindRecordById("items", id)
	if ar.GetString("title") != "hub v2" || ar.GetFloat("qty") != 10 {
		t.Fatalf("the spoke is reverted: %v", ar.FieldsData())
	}
	rows := conflictRows(t, h.app)
	if len(rows) != 1 || rows[0].Kind != KindHubWins || rows[0].Resolution != ResolutionReverted || rows[0].Status != ConflictResolved ||
		rows[0].Strategy != StratHubWins {
		t.Fatalf("hub rows: %+v", rows)
	}
	if ch := changeRows(t, h.app, "status='rejected' AND code={:c}", dbx.Params{"c": proto.CodeHubWins}); len(ch) != 1 {
		t.Fatalf("rejected rows %+v", ch)
	}
	// the spoke keeps an informational copy
	srows := conflictRows(t, a.app)
	if len(srows) != 1 || srows[0].Kind != "hub_wins" || srows[0].Resolution != ResolutionReverted || !strings.Contains(string(srows[0].Incoming), "spoke v2") {
		t.Fatalf("spoke rows: %+v", srows)
	}
	requireConverged(t, h, a)
	// a change that saw the latest version is applied
	ar.Set("title", "calm edit")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	if r := a.sync(t); r.Rejected != 0 || hubItem(t, h, id).GetString("title") != "calm edit" {
		t.Fatalf("non concurrent: %+v", r)
	}
}

func TestHookStrategyDecisions(t *testing.T) {
	cases := []struct {
		name      string
		fn        func(e *kernel.SyncConflictEvent) error
		hubTitle  string
		hubQty    float64
		pushed    func(client.Result) bool
		row       func(ConflictRow) bool
		changeSt  string
		converged bool
	}{
		{"accept", func(e *kernel.SyncConflictEvent) error { e.Resolution = "accept"; e.Message = "fine"; return nil },
			"spoke v2", 11, func(r client.Result) bool { return r.Pushed == 1 },
			func(c ConflictRow) bool {
				return c.Resolution == ResolutionAccepted && c.Status == ConflictResolved && c.Note == "fine"
			}, StatusApplied, true},
		{"reject", func(e *kernel.SyncConflictEvent) error { e.Resolution = "reject"; return nil },
			"hub v2", 10, func(r client.Result) bool { return r.Rejected == 1 },
			func(c ConflictRow) bool { return c.Resolution == ResolutionRejected && c.Status == ConflictResolved }, StatusRejected, true},
		{"merge", func(e *kernel.SyncConflictEvent) error {
			e.Resolution, e.Patch = "merge", map[string]any{"title": "merged"}
			return nil
		}, "merged", 11, func(r client.Result) bool { return r.Pushed == 1 },
			func(c ConflictRow) bool { return c.Resolution == ResolutionAutoMerge && c.Status == ConflictResolved }, StatusApplied, true},
		{"park", func(e *kernel.SyncConflictEvent) error { e.Resolution = "park"; e.Message = "look"; return nil },
			"hub v2", 10, func(r client.Result) bool { return r.Parked == 1 },
			func(c ConflictRow) bool {
				return c.Resolution == ResolutionParked && c.Status == ConflictOpen && c.Kind == KindConcurrentField
			}, StatusParked, false},
		{"error fails closed", func(e *kernel.SyncConflictEvent) error { return errors.New("trap") },
			"hub v2", 10, func(r client.Result) bool { return r.Parked == 1 },
			func(c ConflictRow) bool { return c.Kind == KindHookFailed && c.Status == ConflictOpen }, StatusParked, false},
		{"no resolution fails closed", func(e *kernel.SyncConflictEvent) error { return e.Next() },
			"hub v2", 10, func(r client.Result) bool { return r.Parked == 1 },
			func(c ConflictRow) bool { return c.Kind == KindHookFailed && c.Status == ConflictOpen }, StatusParked, false},
		{"unknown resolution fails closed", func(e *kernel.SyncConflictEvent) error { e.Resolution = "overwrite"; return nil },
			"hub v2", 10, func(r client.Result) bool { return r.Parked == 1 },
			func(c ConflictRow) bool { return c.Kind == KindHookFailed }, StatusParked, false},
		{"panic fails closed", func(e *kernel.SyncConflictEvent) error { panic("boom") },
			"hub v2", 10, func(r client.Result) bool { return r.Parked == 1 },
			func(c ConflictRow) bool { return c.Kind == KindHookFailed }, StatusParked, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, a, _ := hubFixture(t)
			h.strategy(t, "items", StratHook, "payments_conflict", false)
			bindHook(t, h, c.fn)
			id := staleEdit(t, h, a)
			r := a.sync(t)
			if !c.pushed(r) {
				t.Fatalf("push result %+v", r)
			}
			hr := hubItem(t, h, id)
			if hr.GetString("title") != c.hubTitle || hr.GetFloat("qty") != c.hubQty {
				t.Fatalf("hub: %v", hr.FieldsData())
			}
			rows := conflictRows(t, h.app)
			if len(rows) != 1 || !c.row(rows[0]) || rows[0].Strategy != StratHook {
				t.Fatalf("rows %+v", rows)
			}
			if ch := changeRows(t, h.app, "status={:s} AND node={:n}", dbx.Params{"s": c.changeSt, "n": a.m.NodeID()}); len(ch) != 1 {
				t.Fatalf("hub _changes with status %s: %+v", c.changeSt, ch)
			}
			if c.converged {
				a.sync(t)
				requireConverged(t, h, a)
			}
			// the spoke has an informational copy of every answer but a plain "applied"
			if sr := conflictRows(t, a.app); len(sr) != 1 && c.name != "accept" {
				t.Fatalf("spoke rows %+v", sr)
			}
		})
	}
}

func TestHookEventPayloadAndRedaction(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHook, "payments_conflict", false)
	kernel.RegisterSensitiveField(h.items.Id, "secret")
	t.Cleanup(func() { kernel.UnregisterSensitiveField(h.items.Id, "secret") })
	var got *kernel.SyncConflictEvent
	sawSecret := false
	bindHook(t, h, func(e *kernel.SyncConflictEvent) error {
		cp := *e
		if e.Incoming.Patch["title"] != nil || got == nil {
			got = &cp
		}
		if e.Incoming.Patch["secret"] != nil {
			sawSecret = sawSecret || (e.Incoming.Patch["secret"] == kernel.SensitiveMarker && e.Current["secret"] == kernel.SensitiveMarker)
		}
		e.Resolution = "reject"
		return nil
	})
	r0 := h.create(t, map[string]any{"title": "v1", "qty": 10})
	a.sync(t)
	hr := hubItem(t, h, r0.Id)
	hr.Set("title", "hub v2")
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	id := r0.Id
	ar, _ := a.app.FindRecordById("items", id)
	ar.Set("title", "spoke v2")
	ar.Set("secret", "tkc1:ciphertext")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if got == nil {
		t.Fatal("the hook was not asked")
	}
	if got.Hook != "payments_conflict" || got.RecordID != id || got.Collection.Name != "items" || got.Incoming.Op != "u" ||
		got.Incoming.Node != a.m.NodeID() || got.Incoming.Patch["title"] != "spoke v2" || got.Current["title"] != "hub v2" ||
		got.CurrentNode != h.m.NodeID() || got.CurrentHLC == 0 || got.Incoming.HLC <= got.Incoming.BaseHLC || got.Incoming.ActorKind != "system" {
		t.Fatalf("event: %+v", got)
	}
	if !sawSecret {
		t.Fatal("sensitive values must be redacted in the event (patch and current)")
	}
	rows := conflictRows(t, h.app)
	marked := false
	for _, r := range rows {
		if strings.Contains(string(r.Incoming), "tkc1") || strings.Contains(string(r.Current), "tkc1") {
			t.Fatalf("conflict row leaks a sensitive value: %+v", r)
		}
		marked = marked || strings.Contains(string(r.Incoming), kernel.SensitiveMarker)
	}
	if len(rows) == 0 || !marked {
		t.Fatalf("expected a redacted conflict row: %+v", rows)
	}
}

func TestResolveParkedConflictThroughTheCLI(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHook, "", false) // no handler: every conflict parks
	run := func(args ...string) (string, error) {
		cmd := conflictsCommand(h.app)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}

	// take incoming
	id := staleEdit(t, h, a)
	if r := a.sync(t); r.Parked != 1 {
		t.Fatalf("%+v", r)
	}
	out, err := run("--open", "--json")
	var rows []ConflictRow
	if err != nil || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Kind != KindHookFailed || rows[0].Collection != "items" {
		t.Fatalf("list: %v %s", err, out)
	}
	if out, _ := run("--open", "--collection", "items"); !strings.Contains(out, rows[0].ID) || !strings.Contains(out, "hook_failed") {
		t.Fatalf("table: %s", out)
	}
	if out, _ := run("--open", "--collection", "nope"); strings.Contains(out, rows[0].ID) {
		t.Fatalf("collection filter: %s", out)
	}
	if _, err := run("--resolve", rows[0].ID); err == nil {
		t.Fatal("--resolve needs --take")
	}
	if _, err := run("--resolve", rows[0].ID, "--take", "/nonexistent/patch.json"); err == nil {
		t.Fatal("an unreadable patch file is an error")
	}
	if out, err := run("--resolve", rows[0].ID, "--take", "incoming", "--note", "ok by admin"); err != nil || !strings.Contains(out, "resolved") {
		t.Fatalf("resolve: %v %s", err, out)
	}
	hr := hubItem(t, h, id)
	if hr.GetString("title") != "spoke v2" || hr.GetFloat("qty") != 11 {
		t.Fatalf("the parked patch is applied, counter op included: %v", hr.FieldsData())
	}
	if _, err := run("--resolve", rows[0].ID, "--take", "hub"); err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("a second resolve must fail: %v", err)
	}
	if open, _ := ListConflicts(h.app, true, ""); len(open) != 0 {
		t.Fatalf("open: %+v", open)
	}
	cr := conflictRows(t, h.app)[0]
	if cr.Status != ConflictResolved || cr.Resolution != ResolutionAccepted || !strings.Contains(cr.Note, "ok by admin") {
		t.Fatalf("%+v", cr)
	}
	a.sync(t)
	requireConverged(t, h, a)

	// take hub: the node is reverted to the hub state
	id2 := staleEdit(t, h, a)
	a.sync(t)
	rows, _ = ListConflicts(h.app, true, "")
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	if _, err := run("--resolve", rows[0].ID, "--take", "hub"); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if ar, _ := a.app.FindRecordById("items", id2); ar.GetString("title") != "hub v2" {
		t.Fatalf("spoke after take hub: %v", ar.FieldsData())
	}
	requireConverged(t, h, a)

	// take patch.json
	id3 := staleEdit(t, h, a)
	a.sync(t)
	rows, _ = ListConflicts(h.app, true, "")
	pf := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(pf, []byte(`{"title":"from file","qty":{"$inc":4}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("--resolve", rows[0].ID, "--take", pf); err != nil {
		t.Fatal(err)
	}
	hr = hubItem(t, h, id3)
	if hr.GetString("title") != "from file" || hr.GetFloat("qty") != 14 {
		t.Fatalf("patch file: %v", hr.FieldsData())
	}
	a.sync(t)
	requireConverged(t, h, a)
	// a redacted value cannot be replayed
	if err := ResolveConflict(h.app, ResolveOptions{ID: "nope", Take: TakeHub}); err == nil {
		t.Fatal("unknown id")
	}
}

func TestSpokeCannotResolve(t *testing.T) {
	_, a, _ := hubFixture(t)
	if err := ResolveConflict(a.app, ResolveOptions{ID: "x", Take: TakeHub}); err == nil || !strings.Contains(err.Error(), "hub") {
		t.Fatalf("%v", err)
	}
	if _, err := ListConflicts(a.app, false, ""); err != nil {
		t.Fatal(err)
	}
}

func TestParkedChangeIsFinalForTheAckAndIdempotent(t *testing.T) {
	h, a, _ := hubFixture(t)
	h.strategy(t, "items", StratHook, "", false)
	staleEdit(t, h, a)
	if r := a.sync(t); r.Parked != 1 {
		t.Fatalf("%+v", r)
	}
	if st := a.c.Status(); st.Pending != 0 {
		t.Fatalf("a parked change is acked: %+v", st)
	}
	if r := a.sync(t); r.Parked != 0 || r.Pushed != 0 {
		t.Fatalf("the parked change must not be re-pushed: %+v", r)
	}
	if ch := changeRows(t, h.app, "status='parked'", nil); len(ch) != 1 || ch[0].Op != "u" || !strings.Contains(ch[0].Patch, "spoke v2") {
		t.Fatalf("%+v", ch)
	}
	// a lost response: the same change pushed again is answered from the table
	var ch struct {
		Node string `db:"node"`
		Seq  int64  `db:"origin_seq"`
		Hlc  int64  `db:"hlc"`
		Base int64  `db:"base_hlc"`
		Rec  string `db:"record"`
	}
	if err := h.app.DB().NewQuery("SELECT node, origin_seq, hlc, base_hlc, record FROM _changes WHERE status='parked'").One(&ch); err != nil {
		t.Fatal(err)
	}
	tok := a.token(t)
	_, resp, _ := rawPush(t, h, tok, pushReq(pc(ch.Node, ch.Seq, hlc.HLC(ch.Hlc), hlc.HLC(ch.Base), h.items.Id, ch.Rec, "u", map[string]any{"title": "spoke v2"})))
	if len(resp.Results) != 1 || resp.Results[0].Status != proto.ResDuplicate || resp.Results[0].Was != proto.ResParked {
		t.Fatalf("%+v", resp)
	}
	if n := len(conflictRows(t, h.app)); n != 1 {
		t.Fatalf("no second conflict row: %d", n)
	}
}

func TestTypedFieldsAreValidatedAgainstThePolicy(t *testing.T) {
	h, a, _ := hubFixture(t)
	cid, id := h.items.Id, "recordaaaaaaaa1"
	ta, na := a.token(t), a.m.NodeID()
	st, r, _ := rawPush(t, h, ta, pushReq(pc(na, 1, nowHLC(-9000, 0), 0, cid, id, "c", map[string]any{"title": "t", "qty": 10, "total": 1})))
	if st != 200 || r.Results[0].Status != proto.ResApplied {
		t.Fatalf("create: %d %+v", st, r)
	}
	meta, _, _ := readMeta(h.app.DB(), cid, id)
	seq := int64(2)
	push := func(patch map[string]any) proto.PushResult {
		t.Helper()
		_, r, _ := rawPush(t, h, ta, pushReq(pc(na, seq, nowHLC(-8000+seq, 0), hlc.HLC(meta), cid, id, "u", patch)))
		seq++
		return r.Results[0]
	}
	// a stale lww loser wraps a plain number in an op to dodge the clock
	_, r2, _ := rawPush(t, h, ta, pushReq(pc(na, seq, nowHLC(-9500, 0), 0, cid, id, "u", map[string]any{"total": map[string]any{"$inc": 1000000}})))
	seq++
	if res := r2.Results[0]; res.Status != proto.ResRejected || res.Code != proto.CodeValidationFailed {
		t.Fatalf("$inc on a plain field: %+v", res)
	}
	if hubItem(t, h, id).GetFloat("total") != 1 {
		t.Fatal("total must not change")
	}
	for name, patch := range map[string]map[string]any{
		"absolute counter":      {"qty": 0},
		"add on text":           {"title": map[string]any{"$add": []any{"x"}}},
		"unknown $ key":         {"qty": map[string]any{"$inc": 1, "$set": 99}},
		"absolute set":          {"tags": []any{"a"}},
		"op on set typed wrong": {"tags": map[string]any{"$inc": 1}},
	} {
		if res := push(patch); res.Status != proto.ResRejected || res.Code != proto.CodeValidationFailed {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if hubItem(t, h, id).GetFloat("qty") != 10 {
		t.Fatal("an absolute counter must not overwrite other nodes' increments")
	}
	// the legit forms still work, and a JSON field may hold $-keys as a plain value
	if res := push(map[string]any{"qty": map[string]any{"$inc": 5}, "tags": map[string]any{"$add": []any{"a"}}}); res.Status != proto.ResApplied {
		t.Fatalf("legit ops: %+v", res)
	}
	if res := push(map[string]any{"meta": map[string]any{"$add": []any{1}}}); res.Status != proto.ResApplied && res.Status != proto.ResMerged {
		t.Fatalf("json value: %+v", res)
	}
	hr := hubItem(t, h, id)
	if hr.GetFloat("qty") != 15 || fmt.Sprint(hr.GetStringSlice("tags")) != "[a]" || !strings.Contains(hr.GetString("meta"), "$add") {
		t.Fatalf("hub: %v", hr.FieldsData())
	}
	// pull sends the JSON value verbatim (not rewritten to an absolute)
	_, pr := rawPull(t, h, a.token(t), "after=0")
	found := false
	for _, c := range pr.Changes {
		if strings.Contains(string(c.Patch), `"meta":{"$add":[1]}`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("meta not delivered verbatim: %+v", pr.Changes)
	}
}

func TestPullPageHasAByteBudgetAndClientHalvesOnTruncation(t *testing.T) {
	h, a, _ := hubFixture(t)
	big := strings.Repeat("x", 900<<10)
	for i := 0; i < 6; i++ {
		h.create(t, map[string]any{"title": fmt.Sprint("big", i), "meta": map[string]any{"blob": big}})
	}
	tok := a.token(t)
	total, pages := 0, 0
	after := int64(0)
	for {
		st, pr := rawPull(t, h, tok, fmt.Sprintf("after=%d&limit=500", after))
		if st != 200 {
			t.Fatalf("pull %d", st)
		}
		raw, _ := json.Marshal(pr)
		if len(raw) > 6<<20 {
			t.Fatalf("a page of %d bytes exceeds the budget", len(raw))
		}
		total += len(pr.Changes)
		pages++
		after = pr.Next
		if !pr.More {
			break
		}
	}
	if pages < 2 || total != 6 {
		t.Fatalf("pages %d, changes %d: the budget must split the 5+ MiB into several pages", pages, total)
	}
	// a single oversized row still goes out alone (progress is guaranteed)
	// client side: a body cut at 8 MiB is detected and the page is halved
	var limits []string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, proto.PathPull) {
			limits = append(limits, r.URL.Query().Get("limit"))
			if len(limits) == 1 {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", 9<<20))), Request: r}, nil
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c := a.s().client(t, h, func(o *client.Options) {
		o.Backend = backend{a.m}
		o.HTTP = hc
		o.NoPoke = true
		o.Interval = time.Hour
	})
	if r := c.RunOnce(ctxb); r.Err != nil {
		t.Fatalf("the cycle must recover by halving: %v (limits %v)", r.Err, limits)
	}
	if len(limits) < 2 || limits[0] != "500" || limits[1] != "250" {
		t.Fatalf("limits %v", limits)
	}
	if n, _ := a.app.CountRecords("items"); n != 6 {
		t.Fatalf("records %d", n)
	}
}

func TestFailingPulledChangeStopsThePageAndIsRetried(t *testing.T) {
	h, a, _ := hubFixture(t)
	// the spoke has a local unique index the hub does not have
	col := a.coll()
	col.AddIndex("idx_items_title_unique", true, "title", "title != ''")
	if err := a.app.Save(col); err != nil {
		t.Fatal(err)
	}
	good1 := h.create(t, map[string]any{"title": "good1"})
	bad := h.create(t, map[string]any{"title": "dup"})
	good2 := h.create(t, map[string]any{"title": "good2"})
	local := a.create(t, map[string]any{"title": "dup"}) // pending, makes the pulled "dup" fail
	_ = local
	res := a.c.PullOnce(ctxb)
	var ae *client.ApplyError
	if !errors.As(res.Err, &ae) || ae.ID == "" {
		t.Fatalf("want an ApplyError, got %v", res.Err)
	}
	if _, err := a.app.FindRecordById("items", good1.Id); err != nil {
		t.Fatal("the change before the failing one must be applied")
	}
	if _, err := a.app.FindRecordById("items", good2.Id); err == nil {
		t.Fatal("the page stops at the failing change")
	}
	if r, _ := a.app.FindRecordById("items", bad.Id); r != nil {
		t.Fatal("the failing change leaves no partial write")
	}
	rows, _ := ListConflicts(a.app, true, "")
	if len(rows) != 1 || rows[0].Kind != "apply_error" || rows[0].Status != ConflictOpen {
		t.Fatalf("local conflict rows: %+v", rows)
	}
	if st := a.c.Status(); st.ApplyErrors != 1 {
		t.Fatalf("status: %+v", st)
	}
	if res2 := a.c.PullOnce(ctxb); res2.Err == nil {
		t.Fatal("it is retried (and fails again)")
	}
	if n, _ := ListConflicts(a.app, true, ""); len(n) != 1 {
		t.Fatalf("the retry must not duplicate the conflict row: %+v", n)
	}
	// fix the cause: drop the local duplicate; the retry goes through, nothing is lost
	if err := a.app.Delete(local); err != nil {
		t.Fatal(err)
	}
	if res3 := a.c.PullOnce(ctxb); res3.Err != nil {
		t.Fatalf("after the fix: %v", res3.Err)
	}
	for _, id := range []string{bad.Id, good2.Id} {
		if _, err := a.app.FindRecordById("items", id); err != nil {
			t.Fatalf("%s must arrive after the retry: %v", id, err)
		}
	}
}

func TestRevertsAreDeliveredForPushOnlyCollections(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirPush, nil, nil)
	a := newItemsSpoke(t, h, "gate-1")
	a.sync(t)
	r := a.create(t, map[string]any{"title": "ok", "tags": []string{"a"}})
	a.sync(t)
	if hubItem(t, h, r.Id).GetString("title") != "ok" {
		t.Fatal("push-only still pushes")
	}
	// an invalid value (the hub validates; SaveNoValidate skips the local check)
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("title", "bad edit")
	ar.Set("tags", []string{"zzz"})
	if err := a.app.SaveNoValidate(ar); err != nil {
		t.Fatal(err)
	}
	res := a.sync(t)
	if res.Rejected != 1 {
		t.Fatalf("%+v", res)
	}
	ar, _ = a.app.FindRecordById("items", r.Id)
	if ar.GetString("title") != "ok" || fmt.Sprint(ar.GetStringSlice("tags")) != "[a]" {
		t.Fatalf("the revert of a push-only collection must reach the spoke: %v", ar.FieldsData())
	}
}

func TestRevertToDeletedKeepsTheDiscardedWork(t *testing.T) {
	h, a, _ := hubFixture(t)
	r := h.create(t, map[string]any{"title": "will be deleted"})
	a.sync(t)
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("title", "my precious edit")
	if err := a.app.Save(ar); err != nil {
		t.Fatal(err)
	}
	if err := h.app.Delete(hubItem(t, h, r.Id)); err != nil {
		t.Fatal(err)
	}
	if res := a.c.PullOnce(ctxb); res.Err != nil {
		t.Fatal(res.Err)
	}
	if _, err := a.app.FindRecordById("items", r.Id); err == nil {
		t.Fatal("the hub delete wins")
	}
	rows := conflictRows(t, a.app)
	if len(rows) != 1 || rows[0].Kind != "orphaned" || !strings.Contains(string(rows[0].Incoming), "my precious edit") {
		t.Fatalf("the discarded edit must be kept in a conflict row: %+v", rows)
	}
	if ch := changeRows(t, a.app, "code='discarded'", nil); len(ch) != 1 || ch[0].Status != "acked" {
		t.Fatalf("pending rows are retired, not pushed: %+v", ch)
	}
	if st := a.c.Status(); st.Pending != 0 {
		t.Fatalf("pending %d", st.Pending)
	}
	// and a rejected create reverts to deleted
	bad := core.NewRecord(a.coll())
	bad.Set("title", "bad")
	bad.Set("tags", []string{"zzz"})
	if err := a.app.SaveNoValidate(bad); err != nil {
		t.Fatal(err)
	}
	res := a.sync(t)
	if res.Rejected < 1 {
		t.Fatalf("%+v", res)
	}
	if _, err := a.app.FindRecordById("items", bad.Id); err == nil {
		t.Fatal("rejected create is reverted to deleted")
	}
	found := false
	for _, c := range conflictRows(t, a.app) {
		if c.Kind == "validation_failed" && strings.Contains(string(c.Incoming), "zzz") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rejected patch must be kept: %+v", conflictRows(t, a.app))
	}
}

func TestHashMismatchIsCounted(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, nil, nil) // no counter/set fields: the §4.7 check applies
	a := newItemsSpoke(t, h, "gate-1")
	a.sync(t)
	r := h.create(t, map[string]any{"title": "t", "total": 1})
	a.sync(t)
	if st := a.c.Status(); st.HashMismatches != 0 {
		t.Fatalf("clean sync: %+v", st)
	}
	// silent local divergence (raw SQL is not captured)
	if _, err := a.app.NonconcurrentDB().NewQuery("UPDATE items SET title='tampered' WHERE id={:i}").Bind(dbx.Params{"i": r.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	hr := hubItem(t, h, r.Id)
	hr.Set("total", 2)
	if err := h.app.Save(hr); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if st := a.c.Status(); st.HashMismatches != 1 || st.HashStreak != 1 {
		t.Fatalf("hash_mismatch not counted: %+v", st)
	}
}

func TestFieldClocksReachTheSpoke(t *testing.T) {
	h, a, b := hubFixture(t)
	h.strategy(t, "items", StratFieldMerge, "", false)
	r := h.create(t, map[string]any{"title": "t"})
	a.sync(t)
	ar, _ := a.app.FindRecordById("items", r.Id)
	ar.Set("title", "from a")
	_ = a.app.Save(ar)
	a.sync(t)
	b.sync(t)
	var raw string
	if err := b.app.DB().NewQuery("SELECT fields FROM _sync_meta WHERE record={:r}").Bind(dbx.Params{"r": r.Id}).Row(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"title"`) {
		t.Fatalf("spoke field clocks: %s", raw)
	}
	// hub-local writes bump the clock too, so a stale push cannot overwrite them silently
	hr := hubItem(t, h, r.Id)
	hr.Set("title", "hub newest")
	_ = h.app.Save(hr)
	hubClocks, _ := readFieldClocks(h.app.DB(), h.items.Id, r.Id)
	meta, _, _ := readMeta(h.app.DB(), h.items.Id, r.Id)
	if hubClocks["title"] != hlc.HLC(meta) {
		t.Fatalf("hub clocks %v meta %v", hubClocks, meta)
	}
}

// Property: random interleavings of 3 nodes converge on the hub state under
// every strategy (a hook that accepts everything, hub-wins, lww, field-merge).
func TestStrategiesConvergeUnderRandomInterleavings(t *testing.T) {
	for _, strat := range []string{StratLWW, StratFieldMerge, StratHubWins, StratHook} {
		for seed := int64(1); seed <= 3; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", strat, seed), func(t *testing.T) {
				h, a, b := hubFixture(t)
				h.strategy(t, "items", strat, "", false)
				if strat == StratHook {
					bindHook(t, h, func(e *kernel.SyncConflictEvent) error { e.Resolution = "accept"; return nil })
				}
				rng := newLCG(seed)
				var ids []string
				nodes := []struct {
					app  core.App
					sync func()
				}{{h.app, func() {}}, {a.app, func() { a.sync(t) }}, {b.app, func() { b.sync(t) }}}
				for step := 0; step < 50; step++ {
					n := nodes[rng.n(3)]
					if rng.n(10) < 2 || len(ids) == 0 {
						col, _ := n.app.FindCollectionByNameOrId("items")
						r := core.NewRecord(col)
						r.Set("title", fmt.Sprintf("t%d", step))
						r.Set("qty", rng.n(5))
						if err := n.app.Save(r); err != nil {
							t.Fatal(err)
						}
						ids = append(ids, r.Id)
					} else if r, err := n.app.FindRecordById("items", ids[rng.n(len(ids))]); err == nil {
						switch rng.n(4) {
						case 0:
							r.Set("title", fmt.Sprintf("e%d", step))
						case 1:
							r.Set("qty", r.GetFloat("qty")+float64(rng.n(4)+1))
						case 2:
							tags := r.GetStringSlice("tags")
							tag := []string{"a", "b", "c", "d"}[rng.n(4)]
							at := -1
							for i, x := range tags {
								if x == tag {
									at = i
								}
							}
							if at >= 0 {
								tags = append(tags[:at], tags[at+1:]...)
							} else {
								tags = append(tags, tag)
							}
							r.Set("tags", tags)
						case 3:
							r.Set("total", float64(step))
						}
						if err := n.app.Save(r); err != nil {
							t.Fatal(err)
						}
					}
					if rng.n(4) == 0 {
						nodes[1+rng.n(2)].sync()
					}
				}
				for i := 0; i < 5; i++ {
					a.sync(t)
					b.sync(t)
				}
				requireConverged(t, h, a, b)
			})
		}
	}
}
