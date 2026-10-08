//go:build !no_sync

package sync

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

func bp(b bool) *bool       { return &b }
func strp(s string) *string { return &s }

// ---- policy validation -------------------------------------------------

func TestPolicyValidationMatrix(t *testing.T) {
	h := newHub(t)
	// a collection with one field of each interesting type
	c := core.NewBaseCollection("matrix")
	c.Fields.Add(
		&core.TextField{Name: "txt"},
		&core.NumberField{Name: "num"},
		&core.BoolField{Name: "flag"},
		&core.SelectField{Name: "one", MaxSelect: 1, Values: []string{"a", "b"}},
		&core.SelectField{Name: "many", MaxSelect: 3, Values: []string{"a", "b", "c"}},
		&core.JSONField{Name: "js"},
		&core.FileField{Name: "doc", MaxSelect: 1, MaxSize: 1 << 20},
	)
	c.AddIndex("idx_matrix_txt", false, "txt", "")
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	type tc struct {
		name string
		set  func(r *core.Record)
		bad  string // field of the expected validation error ("" = must save)
	}
	cases := []tc{
		{"plain both", func(r *core.Record) { r.Set("collection", "matrix") }, ""},
		{"counter on number", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"num": "counter"})
		}, ""},
		{"counter on text", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"txt": "counter"})
		}, "field_types"},
		{"set on multi select", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"many": "set"})
		}, ""},
		{"set on single select", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"one": "set"})
		}, "field_types"},
		{"set on number", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"num": "set"})
		}, "field_types"},
		{"reserve on text", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"txt": "reserve:tickets"})
		}, ""},
		{"reserve on number", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"num": "reserve:tickets"})
		}, ""},
		{"reserve on bool", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"flag": "reserve:tickets"})
		}, "field_types"},
		{"reserve without name", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"txt": "reserve:"})
		}, "field_types"},
		{"unknown type", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"txt": "bogus"})
		}, "field_types"},
		{"typed unknown field", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("field_types", map[string]string{"nope": "counter"})
		}, "field_types"},
		{"field_types not an object", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("field_types", []string{"x"}) }, "field_types"},
		{"exclude unknown field is a warning", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("exclude", []string{"nope"}) }, ""},
		{"exclude not an array", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("exclude", map[string]string{"a": "b"}) }, "exclude"},
		{"partition ok", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "txt = @node.branch") }, ""},
		{"partition spaces ok", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "  txt=@node.branch ") }, ""},
		{"partition single select ok", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "one = @node.k") }, ""},
		{"partition bad syntax", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "txt == @node.branch") }, "partition"},
		{"partition literal", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "txt = 'B12'") }, "partition"},
		{"partition unknown field", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "nope = @node.branch") }, "partition"},
		{"partition on json", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "js = @node.branch") }, "partition"},
		{"partition on multi select", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("partition", "many = @node.branch") }, "partition"},
		{"partition field excluded", func(r *core.Record) {
			r.Set("collection", "matrix")
			r.Set("partition", "txt = @node.branch")
			r.Set("exclude", []string{"txt"})
		}, "partition"},
		{"auth both", func(r *core.Record) { r.Set("collection", "users"); r.Set("direction", "both") }, "direction"},
		{"auth default direction", func(r *core.Record) { r.Set("collection", "users") }, "direction"},
		{"auth push", func(r *core.Record) { r.Set("collection", "users"); r.Set("direction", "push") }, "direction"},
		{"auth pull", func(r *core.Record) { r.Set("collection", "users"); r.Set("direction", "pull") }, ""},
		{"system collection", func(r *core.Record) { r.Set("collection", "_sync_nodes") }, "collection"},
		{"missing collection enabled", func(r *core.Record) { r.Set("collection", "ghost") }, "collection"},
		{"missing collection disabled", func(r *core.Record) { r.Set("collection", "ghost"); r.Set("enabled", false) }, ""},
		{"bad strategy", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("strategy", "newest-wins") }, "strategy"},
		{"bad crypto", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("crypto", "plain") }, "crypto"},
		{"negative order", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("order", -1) }, "order"},
		{"order ok", func(r *core.Record) { r.Set("collection", "matrix"); r.Set("order", 3) }, ""},
	}
	pc, _ := h.app.FindCollectionByNameOrId(PoliciesCollection)
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := core.NewRecord(pc)
			r.Set("enabled", true)
			tt.set(r)
			err := h.app.Save(r)
			if tt.bad == "" {
				if err != nil {
					t.Fatalf("must save: %v", err)
				}
				_ = h.app.Delete(r)
				return
			}
			if err == nil {
				_ = h.app.Delete(r)
				t.Fatalf("must fail on %q", tt.bad)
			}
			if !strings.Contains(err.Error(), tt.bad) {
				t.Fatalf("error %q does not name %q", err, tt.bad)
			}
		})
	}

	t.Run("one policy per collection", func(t *testing.T) {
		mk := func(ref string) error {
			r := core.NewRecord(pc)
			r.Set("collection", ref)
			r.Set("enabled", true)
			return h.app.Save(r)
		}
		if err := mk("matrix"); err != nil {
			t.Fatal(err)
		}
		if err := mk(c.Id); err == nil {
			t.Fatal("a second policy for the same collection (by id) must fail")
		}
	})
}

// A spoke accepts the hub's rows even when they would not validate there.
func TestPolicyValidationIsHubOnly(t *testing.T) {
	s := newSpoke(t)
	pc, _ := s.app.FindCollectionByNameOrId(PoliciesCollection)
	r := core.NewRecord(pc)
	r.Set("collection", "not_yet_created")
	r.Set("direction", "both")
	r.Set("enabled", true)
	if err := s.app.Save(r); err != nil {
		t.Fatalf("a spoke must store the hub's policy rows as they are: %v", err)
	}
}

func TestPolicyDefaultsViaAPIAndCLI(t *testing.T) {
	h := newHub(t)
	// created through the REST API without the field: pull_view_rule defaults to true
	code, body := h.do(t, h.su, "POST", "/api/collections/_sync_policies/records", `{"collection":"items","direction":"both","enabled":true}`)
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	var rec map[string]any
	_ = json.Unmarshal(body, &rec)
	if rec["pull_view_rule"] != true {
		t.Fatalf("pull_view_rule must default to true: %s", body)
	}
	// an explicit false is kept
	_ = h.app.Delete(mustRec(t, h.app, rec["id"].(string)))
	code, body = h.do(t, h.su, "POST", "/api/collections/_sync_policies/records", `{"collection":"items","pull_view_rule":false,"enabled":true}`)
	if code != 200 || !strings.Contains(string(body), `"pull_view_rule":false`) {
		t.Fatalf("explicit false: %d %s", code, body)
	}
	// and the validation also guards the API
	code, _ = h.do(t, h.su, "POST", "/api/collections/_sync_policies/records", `{"collection":"users","direction":"both","enabled":true}`)
	if code != 400 {
		t.Fatalf("auth both through the API must be 400, got %d", code)
	}
}

func mustRec(t *testing.T, app core.App, id string) *core.Record {
	t.Helper()
	r, err := app.FindRecordById(PoliciesCollection, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestPoliciesCLI(t *testing.T) {
	t.Setenv(EnvRole, "hub")
	h := newHub(t)
	out, err := runCmd(t, policiesCommand(h.app), "set", "items", "--direction", "both", "--field-type", "qty=counter", "--field-type", "tags=set", "--exclude", "note", "--order", "2")
	if err != nil {
		t.Fatalf("set: %v %s", err, out)
	}
	rows, _ := ListPolicies(h.app)
	if len(rows) != 1 || rows[0].Collection != "items" || !rows[0].PullViewRule || !rows[0].Enabled || rows[0].FieldTypes["qty"] != "counter" || rows[0].Order != 2 {
		t.Fatalf("rows %+v", rows)
	}
	// only the passed flags change
	if out, err = runCmd(t, policiesCommand(h.app), "set", "items", "--partition", "title = @node.branch", "--pull-view-rule=false"); err != nil {
		t.Fatalf("set: %v %s", err, out)
	}
	rows, _ = ListPolicies(h.app)
	if rows[0].Partition != "title = @node.branch" || rows[0].PullViewRule || rows[0].FieldTypes["tags"] != "set" || rows[0].Direction != "both" {
		t.Fatalf("rows %+v", rows[0])
	}
	// validation errors reach the CLI
	if _, err = runCmd(t, policiesCommand(h.app), "set", "items", "--field-type", "title=counter"); err == nil {
		t.Fatal("counter on a text field must fail")
	}
	if _, err = runCmd(t, policiesCommand(h.app), "set", "items", "--field-type", "broken"); err == nil {
		t.Fatal("a malformed --field-type must fail")
	}
	out, err = runCmd(t, policiesCommand(h.app), "list", "--json")
	if err != nil || !strings.Contains(out, `"collection":"items"`) {
		t.Fatalf("list: %v %s", err, out)
	}
	// lint: warnings do not fail
	out, err = runCmd(t, policiesCommand(h.app), "lint")
	if err != nil {
		t.Fatalf("lint with warnings must succeed: %v %s", err, out)
	}
	for _, want := range []string{`file field "photo"`, "not indexed", "0 error(s)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lint output lacks %q:\n%s", want, out)
		}
	}
	if out, err = runCmd(t, policiesCommand(h.app), "rm", "items"); err != nil {
		t.Fatalf("rm: %v %s", err, out)
	}
	if rows, _ = ListPolicies(h.app); len(rows) != 0 {
		t.Fatalf("rows after rm: %+v", rows)
	}
	if _, err = runCmd(t, policiesCommand(h.app), "rm", "items"); err == nil {
		t.Fatal("removing a missing policy must fail")
	}
}

func TestLintFindings(t *testing.T) {
	h := newHub(t)
	// rows that validation lets through but lint reports; a hub-wins counter, a null view rule
	if _, err := SetPolicy(h.app, "items", PolicyChange{
		Strategy: strp("hub-wins"), FieldTypes: map[string]string{"qty": "counter"}, Exclude: []string{"ghost", "qty"},
	}); err != nil {
		t.Fatal(err)
	}
	c, _ := h.app.FindCollectionByNameOrId("items")
	c.ViewRule = nil
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	issues, err := LintPolicies(h.app)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, i := range issues {
		if i.Level == "error" {
			t.Fatalf("unexpected error: %v", i)
		}
		all = append(all, i.Message)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"ignored under strategy hub-wins", `unknown field "ghost"`, "both typed and excluded", `file field "photo"`, "viewRule is null"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("lint lacks %q:\n%s", want, joined)
		}
	}
	// a pull-only policy with a strategy: no effect
	if _, err := SetPolicy(h.app, "items", PolicyChange{Direction: strp("pull"), Strategy: strp("field-merge")}); err != nil {
		t.Fatal(err)
	}
	issues, _ = LintPolicies(h.app)
	found := false
	for _, i := range issues {
		found = found || strings.Contains(i.Message, "has no effect when direction")
	}
	if !found {
		t.Fatalf("direction/strategy combination not reported: %+v", issues)
	}
}

// ---- directions --------------------------------------------------------

func TestDirectionChecksOnPushAndPull(t *testing.T) {
	h, a, _ := hubFixture(t)
	node := a.m.NodeID()
	cid := h.items.Id
	oseq := int64(0)
	tok := a.token(t)
	push := func(op, rec string) proto.PushResult {
		t.Helper()
		oseq++
		st, res, eb := rawPush(t, h, tok, pushReq(pc(node, oseq, nowHLC(-5000, uint16(oseq)), 0, cid, rec, op, map[string]any{"title": "x"})))
		if st != 200 {
			t.Fatalf("push: %d %+v", st, eb)
		}
		return res.Results[0]
	}
	setDir := func(dir string) {
		t.Helper()
		if _, err := SetPolicy(h.app, "items", PolicyChange{Direction: strp(dir)}); err != nil {
			t.Fatal(err)
		}
	}

	setDir("pull")
	if r := push("c", "pullonly0000001"); r.Status != proto.ResRejected || r.Code != proto.CodePolicyDirection {
		t.Fatalf("pull-only collection must refuse pushes: %+v", r)
	}
	setDir("none")
	if r := push("c", "noneonly0000001"); r.Status != proto.ResRejected || r.Code != proto.CodePolicyDirection {
		t.Fatalf("direction none must refuse pushes: %+v", r)
	}
	setDir("push")
	if r := push("c", "pushonly0000001"); r.Status != proto.ResApplied {
		t.Fatalf("push-only collection accepts pushes: %+v", r)
	}

	// pull: push-only and none collections are not delivered
	pulled := func() int {
		t.Helper()
		_, pr := rawPull(t, h, tok, "after=0&limit=1000")
		n := 0
		for _, c := range pr.Changes {
			if c.Collection == cid && !c.Revert {
				n++
			}
		}
		return n
	}
	h.create(t, map[string]any{"title": "hub local"})
	if n := pulled(); n != 0 {
		t.Fatalf("a push-only collection must not be pulled (got %d changes)", n)
	}
	setDir("none")
	if n := pulled(); n != 0 {
		t.Fatalf("direction none must not be pulled (got %d changes)", n)
	}
	setDir("pull")
	if n := pulled(); n < 1 {
		t.Fatal("a pull-only collection is pulled")
	}
	setDir("both")
	if n := pulled(); n < 2 {
		t.Fatalf("both: the pushed row and the hub write are pulled (got %d)", n)
	}
}

// ---- partitions --------------------------------------------------------

type ticketHub struct {
	*hubEnv
	col *core.Collection
}

func ticketCollection(t *testing.T, app core.App) *core.Collection {
	t.Helper()
	open := ""
	c := core.NewBaseCollection("tickets")
	c.Id = "pbc_tickets"
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "branch"},
		&core.NumberField{Name: "fee"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_tickets_branch", false, "branch", "")
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func newTicketHub(t *testing.T) *ticketHub {
	t.Helper()
	h := newHub(t)
	c := ticketCollection(t, h.app)
	if _, err := SetPolicy(h.app, "tickets", PolicyChange{Partition: strp("branch = @node.branch"), FieldTypes: map[string]string{"fee": "counter"}}); err != nil {
		t.Fatal(err)
	}
	return &ticketHub{hubEnv: h, col: c}
}

func (h *ticketHub) ticket(t *testing.T, title, branch string) *core.Record {
	t.Helper()
	r := core.NewRecord(h.col)
	r.Set("title", title)
	r.Set("branch", branch)
	if err := h.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

type ticketSpoke struct {
	*spokeEnv
	c *client.Client
}

func newTicketSpoke(t *testing.T, h *ticketHub, name, branch string) *ticketSpoke {
	t.Helper()
	s := newSpoke(t)
	ticketCollection(t, s.app)
	params := map[string]string{}
	if branch != "" {
		params["branch"] = branch
	}
	h.join(t, s, h.enroll(t, name, params))
	cl := s.client(t, h.hubEnv, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
	})
	return &ticketSpoke{spokeEnv: s, c: cl}
}

func (s *ticketSpoke) sync(t *testing.T) client.Result {
	t.Helper()
	r := s.c.RunOnce(ctxb)
	if r.Err != nil {
		t.Fatalf("sync: %v", r.Err)
	}
	return r
}

func (s *ticketSpoke) has(id string) *core.Record {
	r, _ := s.app.FindRecordById("tickets", id)
	return r
}

func (s *ticketSpoke) create(t *testing.T, title, branch string) *core.Record {
	t.Helper()
	col, _ := s.app.FindCollectionByNameOrId("tickets")
	r := core.NewRecord(col)
	r.Set("title", title)
	r.Set("branch", branch)
	if err := s.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func countRows(t *testing.T, app core.App, q string, args dbx.Params) int {
	t.Helper()
	var n int
	if err := app.DB().NewQuery(q).Bind(args).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPartitionPullFiltersAndMoveEvicts(t *testing.T) {
	h := newTicketHub(t)
	a := newTicketSpoke(t, h, "gate-a", "A")
	b := newTicketSpoke(t, h, "gate-b", "B")
	t1 := h.ticket(t, "t1", "A")
	t2 := h.ticket(t, "t2", "B")
	a.sync(t)
	b.sync(t)
	if a.has(t1.Id) == nil || a.has(t2.Id) != nil {
		t.Fatalf("gate-a must hold only its own branch (t1=%v t2=%v)", a.has(t1.Id) != nil, a.has(t2.Id) != nil)
	}
	if b.has(t2.Id) == nil || b.has(t1.Id) != nil {
		t.Fatal("gate-b must hold only its own branch")
	}
	// the hub rows carry the partition keys
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE record={:r} AND op='c' AND part_old='' AND part_new='A'", dbx.Params{"r": t1.Id}); n != 1 {
		t.Fatalf("create row of t1: part_new must be A (%d)", n)
	}

	// move t1 from A to B on the hub
	t1.Set("branch", "B")
	if err := h.app.Save(t1); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE record={:r} AND op='u' AND part_old='A' AND part_new='B'", dbx.Params{"r": t1.Id}); n != 1 {
		t.Fatalf("move row of t1: part_old A / part_new B expected (%d)", n)
	}
	// what gate-a is told: an eviction, no data
	cur, _ := client.LoadCursor(a.app)
	_, pr := rawPull(t, h.hubEnv, a.token(t), "after="+itoa(cur.PullAfter))
	var evict *proto.PullChange
	for i := range pr.Changes {
		if pr.Changes[i].Record == t1.Id {
			evict = &pr.Changes[i]
		}
	}
	if evict == nil || evict.Op != "x" || !evict.Evict || len(evict.Patch) != 0 {
		t.Fatalf("gate-a must receive an evict row for t1, got %+v", pr.Changes)
	}
	a.sync(t)
	b.sync(t)
	if a.has(t1.Id) != nil {
		t.Fatal("gate-a must have evicted t1")
	}
	// an eviction is not a delete: no tombstone, no clock
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_tombstones WHERE record={:r}", dbx.Params{"r": t1.Id}); n != 0 {
		t.Fatalf("an eviction must leave no tombstone (%d)", n)
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_meta WHERE record={:r}", dbx.Params{"r": t1.Id}); n != 0 {
		t.Fatalf("an eviction must leave no record clock (%d)", n)
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _changes", nil); n != 0 {
		t.Fatalf("an eviction must not be captured as a change (%d)", n)
	}
	got := b.has(t1.Id)
	if got == nil || got.GetString("title") != "t1" || got.GetString("branch") != "B" {
		t.Fatalf("gate-b must receive the whole record that entered its partition: %v", got)
	}

	// and back: B -> A re-creates it on gate-a (no tombstone in the way)
	t1, _ = h.app.FindRecordById("tickets", t1.Id)
	t1.Set("branch", "A")
	t1.Set("title", "t1 back")
	if err := h.app.Save(t1); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if r := a.has(t1.Id); r == nil || r.GetString("title") != "t1 back" {
		t.Fatalf("gate-a must get t1 back: %v", r)
	}
	if b.has(t1.Id) != nil {
		t.Fatal("gate-b must have evicted t1")
	}

	// a delete in the partition is a delete (with tombstone), not an eviction
	if err := h.app.Delete(t1); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	b.sync(t)
	if a.has(t1.Id) != nil {
		t.Fatal("the delete must reach gate-a")
	}
	if n := countRows(t, a.app, "SELECT COUNT(*) FROM _sync_tombstones WHERE record={:r} AND kind='delete'", dbx.Params{"r": t1.Id}); n != 1 {
		t.Fatal("a delete leaves a tombstone")
	}
	// gate-b never saw the delete row (it left B earlier): no tombstone, no noise
	if n := countRows(t, b.app, "SELECT COUNT(*) FROM _sync_tombstones WHERE record={:r}", dbx.Params{"r": t1.Id}); n != 0 {
		t.Fatal("gate-b must not learn about a delete of a record outside its partition")
	}
	_ = t2
}

func TestPartitionNodeWithoutParamGetsNothing(t *testing.T) {
	h := newTicketHub(t)
	h.ticket(t, "t1", "A")
	n := newTicketSpoke(t, h, "gate-none", "")
	n.sync(t)
	if cnt := countRows(t, n.app, "SELECT COUNT(*) FROM tickets", nil); cnt != 0 {
		t.Fatalf("a node without the partition parameter must receive nothing (%d)", cnt)
	}
	// and may push nothing
	n.create(t, "mine", "A")
	n.sync(t)
	if cnt := countRows(t, h.app, "SELECT COUNT(*) FROM tickets WHERE title='mine'", nil); cnt != 0 {
		t.Fatal("a node without the partition parameter must not create records")
	}
}

func TestPartitionPushRejectsOutsideRecords(t *testing.T) {
	h := newTicketHub(t)
	a := newTicketSpoke(t, h, "gate-a", "A")
	other := h.ticket(t, "other branch", "B")
	a.sync(t)

	hubRowCode := func(rec string) string {
		var code string
		_ = h.app.DB().NewQuery("SELECT code FROM _changes WHERE record={:r} AND status='rejected' ORDER BY seq DESC LIMIT 1").Bind(dbx.Params{"r": rec}).Row(&code)
		return code
	}

	// 1. a create outside the partition
	bad := a.create(t, "wrong branch", "B")
	a.sync(t)
	if _, err := h.app.FindRecordById("tickets", bad.Id); err == nil {
		t.Fatal("the hub must not accept a record outside the partition of the node")
	}
	if c := hubRowCode(bad.Id); c != proto.CodePolicyPartition {
		t.Fatalf("code %q", c)
	}
	if a.has(bad.Id) != nil {
		t.Fatal("the node's copy is reverted")
	}

	// 2. a create without the partition value
	nopart := a.create(t, "no branch", "")
	a.sync(t)
	if c := hubRowCode(nopart.Id); c != proto.CodePolicyPartition {
		t.Fatalf("a create without a partition value must be refused, code %q", c)
	}

	// 3. an in-partition record moved out by the spoke
	ok := a.create(t, "mine", "A")
	a.sync(t)
	if _, err := h.app.FindRecordById("tickets", ok.Id); err != nil {
		t.Fatalf("an in-partition create is accepted: %v", err)
	}
	if n := countRows(t, h.app, "SELECT COUNT(*) FROM _changes WHERE record={:r} AND status='applied' AND part_new='A' AND part_old=''", dbx.Params{"r": ok.Id}); n != 1 {
		t.Fatal("hub row of the accepted create must carry part_new=A")
	}
	mine := a.has(ok.Id)
	mine.Set("branch", "B")
	if err := a.app.Save(mine); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if hr, _ := h.app.FindRecordById("tickets", ok.Id); hr == nil || hr.GetString("branch") != "A" {
		t.Fatalf("the hub record must stay in partition A: %v", hr)
	}
	if c := hubRowCode(ok.Id); c != proto.CodePolicyPartition {
		t.Fatalf("moving a record out of the partition must be refused, code %q", c)
	}
	if r := a.has(ok.Id); r == nil || r.GetString("branch") != "A" {
		t.Fatalf("the revert must restore branch A on the node: %v", r)
	}

	// 4. touching a record of another partition (before-state check), as a node that never saw it
	tok := a.token(t)
	var pushed int64
	_ = h.app.DB().NewQuery("SELECT pushed_origin_seq FROM _sync_nodes WHERE name='gate-a'").Row(&pushed)
	st, res, eb := rawPush(t, h.hubEnv, tok, pushReq(
		pc(a.m.NodeID(), pushed+1, nowHLC(-4000, 1), 0, "pbc_tickets", other.Id, "u", map[string]any{"title": "hijack"}),
		pc(a.m.NodeID(), pushed+2, nowHLC(-4000, 2), 0, "pbc_tickets", other.Id, "d", map[string]any{}),
	))
	if st != 200 {
		t.Fatalf("%d %+v", st, eb)
	}
	for _, r := range res.Results {
		if r.Status != proto.ResRejected || r.Code != proto.CodePolicyPartition {
			t.Fatalf("a change to a record of another partition must be refused: %+v", res.Results)
		}
	}
	if hr, _ := h.app.FindRecordById("tickets", other.Id); hr == nil || hr.GetString("title") != "other branch" {
		t.Fatal("the other partition's record must be untouched")
	}
}

// A rejected change on a record that lives in another partition must not reveal it.
func TestPartitionRevertDoesNotLeak(t *testing.T) {
	h := newTicketHub(t)
	a := newTicketSpoke(t, h, "gate-a", "A")
	secret := h.ticket(t, "secret of B", "B")
	a.sync(t)
	// the node pushes a create with the id of a record of another partition
	col, _ := a.app.FindCollectionByNameOrId("tickets")
	r := core.NewRecord(col)
	r.Id = secret.Id
	r.Set("title", "guess")
	r.Set("branch", "A")
	if err := a.app.Save(r); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if hr, _ := h.app.FindRecordById("tickets", secret.Id); hr.GetString("title") != "secret of B" {
		t.Fatal("the hub record must be untouched")
	}
	// the revert row stores nothing and the node learns nothing
	var patch string
	if err := h.app.DB().NewQuery("SELECT patch FROM _changes WHERE record={:r} AND status='revert'").Bind(dbx.Params{"r": secret.Id}).Row(&patch); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(patch, "secret") {
		t.Fatalf("the revert row leaks the record: %s", patch)
	}
	if got := a.has(secret.Id); got != nil && strings.Contains(got.GetString("title"), "secret") {
		t.Fatal("the node must not receive the record of another partition")
	}
}

// ---- pull_view_rule ----------------------------------------------------

// viewHub is a hub whose node acts as a normal user (not a superuser), so that
// the view rule is really evaluated.
func viewHub(t *testing.T) (*hubEnv, *itemsSpoke) {
	t.Helper()
	h := newHub(t)
	if _, err := SetPolicy(h.app, "items", PolicyChange{Direction: strp("both")}); err != nil {
		t.Fatal(err)
	}
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: "gate-1", Profile: "edge", Actor: "users/" + h.usr.Id})
	if err != nil {
		t.Fatal(err)
	}
	a := newItemsSpokeWith(t, h, "gate-1", code)
	a.sync(t)
	return h, a
}

func (s *itemsSpoke) has(id string) *core.Record {
	r, _ := s.app.FindRecordById("items", id)
	return r
}

func TestPullViewRuleHidesAndEvicts(t *testing.T) {
	h, a := viewHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), sp("qty < 100"))
	vis := h.create(t, map[string]any{"title": "visible", "qty": 5})
	hid := h.create(t, map[string]any{"title": "hidden", "qty": 500})
	a.sync(t)
	if a.has(vis.Id) == nil {
		t.Fatal("a record that satisfies the view rule is pulled")
	}
	if a.has(hid.Id) != nil {
		t.Fatal("a record that fails the view rule must not be pulled")
	}

	// it becomes invisible: the node drops its copy
	vis.Set("qty", 500)
	if err := h.app.Save(vis); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if a.has(vis.Id) != nil {
		t.Fatal("a record that stops satisfying the view rule is evicted")
	}
	// and visible again: the update travels as the whole record
	vis.Set("qty", 7)
	if err := h.app.Save(vis); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	if r := a.has(vis.Id); r == nil || r.GetString("title") != "visible" || r.GetFloat("qty") != 7 {
		t.Fatalf("a record that becomes visible is delivered whole: %v", r)
	}

	// pull_view_rule=false: the rule is not applied
	if _, err := SetPolicy(h.app, "items", PolicyChange{PullViewRule: bp(false)}); err != nil {
		t.Fatal(err)
	}
	open := h.create(t, map[string]any{"title": "no rule", "qty": 900})
	a.sync(t)
	if a.has(open.Id) == nil {
		t.Fatal("with pull_view_rule=false the view rule does not limit the pull")
	}
}

func TestNullViewRuleNeverPulledUnlessTrusted(t *testing.T) {
	h, a := viewHub(t)
	h.setRules(t, sp(""), sp(""), sp(""), nil) // viewRule null: superusers only
	r1 := h.create(t, map[string]any{"title": "private"})
	a.sync(t)
	if a.has(r1.Id) != nil {
		t.Fatal("a collection with a null view rule is never pulled by default")
	}
	// raw pull of the page: nothing
	_, pr := rawPull(t, h, a.token(t), "after=0&limit=1000")
	for _, c := range pr.Changes {
		if c.Collection == h.items.Id && !c.Revert {
			t.Fatalf("unexpected change for a null-rule collection: %+v", c)
		}
	}
	if _, err := SetPolicy(h.app, "items", PolicyChange{Trusted: bp(true)}); err != nil {
		t.Fatal(err)
	}
	r2 := h.create(t, map[string]any{"title": "trusted"})
	a.sync(t)
	if a.has(r2.Id) == nil {
		t.Fatal("trusted=true lets a null-rule collection be pulled")
	}
	// with the check switched off it is pulled as well
	if _, err := SetPolicy(h.app, "items", PolicyChange{Trusted: bp(false), PullViewRule: bp(false)}); err != nil {
		t.Fatal(err)
	}
	r3 := h.create(t, map[string]any{"title": "unchecked"})
	a.sync(t)
	if a.has(r3.Id) == nil {
		t.Fatal("pull_view_rule=false skips the view rule check")
	}
}

func (s *ticketSpoke) token(t *testing.T) string {
	t.Helper()
	if _, err := s.c.Handshake(ctxb); err != nil {
		t.Fatal(err)
	}
	return s.c.Token()
}
