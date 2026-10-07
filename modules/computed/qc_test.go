package computed

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

func (m *Module) prevLen() int {
	n := 0
	m.prev.Range(func(_, _ any) bool { n++; return true })
	return n
}

// C1: a client PATCH that carries the (stale) stored value must not revert a
// recompute that committed after the record was loaded.
func TestC1ClientPatchDoesNotRevertFreshValue(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	p := e.parent(t)
	e.kid(t, p, 1, "")
	e.want(t, p, "cnt", 1)

	fired := false
	e.app.OnRecordUpdateRequest("parents").BindFunc(func(ev *core.RecordRequestEvent) error {
		// runs after the record was loaded (cnt = 1) and before the save: a
		// concurrent client adds a child, the module commits cnt = 2
		if !fired {
			fired = true
			e.kid(t, p, 1, "")
		}
		return ev.Next()
	})
	code, out := e.do(t, false, "PATCH", "/api/collections/parents/records/"+p.Id, `{"title":"edited","cnt":1}`)
	if code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	e.want(t, p, "cnt", 2)
	r, _ := e.app.FindRecordById("parents", p.Id)
	if r.GetString("title") != "edited" {
		t.Fatal("the rest of the PATCH must still apply")
	}
	n := 0
	e.m.clientSaves.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("clientSaves leaked %d entries", n)
	}
}

// C2: the same record object updated twice in one transaction (A->B, B->C).
func TestC2SameRecordTwiceInTransaction(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	a, b, c := e.parent(t), e.parent(t), e.parent(t)
	k := e.kid(t, a, 1, "")
	e.want(t, a, "cnt", 1)
	k, _ = e.app.FindRecordById("kids", k.Id) // loaded like an update request does

	err := e.app.RunInTransaction(func(tx kernel.App) error {
		k.Set("parent", b.Id)
		if err := tx.Save(k); err != nil {
			return err
		}
		k.Set("parent", c.Id)
		return tx.Save(k)
	})
	if err != nil {
		t.Fatal(err)
	}
	e.want(t, a, "cnt", 0)
	e.want(t, b, "cnt", 0)
	e.want(t, c, "cnt", 1)
	if n := e.m.prevLen(); n != 0 {
		t.Fatalf("prev leaked %d entries", n)
	}
}

// C3: module writes of a self-relation collection and rolled back updates leave nothing behind.
func TestC3NoLeakSelfRelationAndRollback(t *testing.T) {
	e := setup(t)
	nc := core.NewBaseCollection("nodes")
	nc.Fields.Add(&core.NumberField{Name: "child_count"})
	if err := e.app.Save(nc); err != nil {
		t.Fatal(err)
	}
	nc.Fields.Add(&core.RelationField{Name: "up", CollectionId: nc.Id, MaxSelect: 1})
	if err := e.app.Save(nc); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(e.app, Def{Collection: "nodes", Field: "child_count", Kind: "count", SourceCollection: "nodes", SourceRelation: "up"}); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
	r1, r2 := core.NewRecord(nc), core.NewRecord(nc)
	for _, r := range []*core.Record{r1, r2} {
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	child := core.NewRecord(nc)
	child.Set("up", r1.Id)
	if err := e.app.Save(child); err != nil {
		t.Fatal(err)
	}
	child, _ = e.app.FindRecordById("nodes", child.Id)
	child.Set("up", r2.Id)
	if err := e.app.Save(child); err != nil {
		t.Fatal(err)
	}
	get := func(r *core.Record) float64 {
		x, _ := e.app.FindRecordById("nodes", r.Id)
		return x.GetFloat("child_count")
	}
	if get(r1) != 0 || get(r2) != 1 {
		t.Fatalf("counts r1=%v r2=%v", get(r1), get(r2))
	}
	if n := e.m.prevLen(); n != 0 {
		t.Fatalf("prev leaked %d entries (self relation)", n)
	}

	// a rolled back transaction recomputes nothing and keeps nothing
	e.def(t, "cnt", "count", "", "")
	p, q := e.parent(t), e.parent(t)
	k := e.kid(t, p, 1, "")
	k, _ = e.app.FindRecordById("kids", k.Id)
	before := e.m.ParentWrites()
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		k.Set("parent", q.Id)
		if err := tx.Save(k); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if e.m.ParentWrites() != before {
		t.Fatal("rolled back write triggered a recompute")
	}
	e.want(t, p, "cnt", 1)
	e.want(t, q, "cnt", 0)
	if n := e.m.prevLen(); n != 0 {
		t.Fatalf("prev leaked %d entries after rollback", n)
	}
}

// C4: definitions are stored by collection id: renaming either collection keeps
// maintenance and the client guard working.
func TestC4CollectionRenameKeepsDefinitions(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	p := e.parent(t)
	e.kid(t, p, 1, "")

	rec, err := e.app.FindFirstRecordByFilter(CollectionName, "field='cnt'")
	if err != nil {
		t.Fatal(err)
	}
	if rec.GetString("collection") != e.pc.Id || rec.GetString("source_collection") != e.kc.Id {
		t.Fatalf("definition must store ids, got %q / %q", rec.GetString("collection"), rec.GetString("source_collection"))
	}

	pc, _ := e.app.FindCollectionByNameOrId("parents")
	pc.Name = "parents_renamed"
	if err := e.app.Save(pc); err != nil {
		t.Fatal(err)
	}
	kc, _ := e.app.FindCollectionByNameOrId("kids")
	kc.Name = "kids_renamed"
	if err := e.app.Save(kc); err != nil {
		t.Fatal(err)
	}

	kr, _ := e.app.FindCollectionByNameOrId("kids_renamed")
	k2 := core.NewRecord(kr)
	k2.Set("parent", p.Id)
	if err := e.app.Save(k2); err != nil {
		t.Fatal(err)
	}
	r, _ := e.app.FindRecordById("parents_renamed", p.Id)
	if r.GetFloat("cnt") != 2 {
		t.Fatalf("cnt after renames = %v, want 2", r.GetFloat("cnt"))
	}
	if code, _ := e.do(t, false, "PATCH", "/api/collections/parents_renamed/records/"+p.Id, `{"cnt":99}`); code != 400 {
		t.Fatalf("guard must survive the rename, got %d", code)
	}
	defs, _ := List(e.app, "")
	if len(defs) != 1 || defs[0].Collection != "parents_renamed" || defs[0].SourceCollection != "kids_renamed" {
		t.Fatalf("names must resolve from ids: %+v", defs)
	}
	if reps, err := e.m.Verify("parents_renamed", ""); err != nil || reps[0].Drift != 0 {
		t.Fatalf("verify: %v %+v", err, reps)
	}
}

// C5/C6: query shapes.
func TestC5C6QueryShapes(t *testing.T) {
	e := setup(t)
	kc, _ := e.app.FindCollectionByNameOrId("kids")
	sql := func(d Def, ids []string) string {
		q, err := buildQuery(e.app, kc, &d, ids)
		if err != nil {
			t.Fatal(err)
		}
		return strings.ToUpper(q.Build().SQL())
	}
	base := Def{Collection: "parents", Field: "cnt", SourceCollection: "kids", SourceRelation: "parent"}

	cnt := base
	cnt.Kind = KindCount
	if s := sql(cnt, []string{"x"}); !strings.Contains(s, "COUNT(*)") || strings.Contains(s, "DISTINCT") {
		t.Fatalf("count without joins must use COUNT(*): %s", s)
	}
	cnt.Filter = "status = 'done'"
	if s := sql(cnt, []string{"x"}); !strings.Contains(s, "COUNT(*)") {
		t.Fatalf("a plain filter adds no join: %s", s)
	}

	last := base
	last.Kind, last.SourceField = KindLast, "amount"
	if s := sql(last, []string{"x"}); !strings.Contains(s, "ORDER BY") || !strings.Contains(s, "`CREATED` DESC") || !strings.Contains(s, "`ID` DESC") || !strings.Contains(s, "LIMIT 1") {
		t.Fatalf("last must be ORDER BY created DESC, id DESC LIMIT 1: %s", s)
	}

	// behaviour: newest wins, ties on created broken by id, NULL counts as 0
	e.def(t, "lst", "last", "amount", "")
	p := e.parent(t)
	e.kid(t, p, 5, "")
	time.Sleep(5 * time.Millisecond) // `created` has millisecond resolution; ties fall back to id (random)
	e.kid(t, p, 7, "")
	e.want(t, p, "lst", 7)
	time.Sleep(5 * time.Millisecond)
	e.kid(t, p, 0, "") // newest child has amount 0 (the number field default)
	e.want(t, p, "lst", 0)
}

func TestC6IndexWarningAndCommand(t *testing.T) {
	e := setup(t)
	d := Def{Collection: "parents", Field: "cnt", Kind: KindCount, SourceCollection: "kids", SourceRelation: "parent"}
	w := MissingIndex(e.app, d)
	if !strings.Contains(w, "toki computed index kids parent") {
		t.Fatalf("expected a warning naming the fix: %q", w)
	}
	name, created, err := EnsureIndex(e.app, "kids", "parent")
	if err != nil || !created || name == "" {
		t.Fatalf("EnsureIndex: %q %v %v", name, created, err)
	}
	if w := MissingIndex(e.app, d); w != "" {
		t.Fatalf("warning must be gone: %q", w)
	}
	if _, created, err := EnsureIndex(e.app, "kids", "parent"); err != nil || created {
		t.Fatalf("second call must be a no-op: %v %v", created, err)
	}
	if _, _, err := EnsureIndex(e.app, "kids", "nope"); err == nil {
		t.Fatal("unknown column must be refused")
	}
	if _, created, err := EnsureIndex(e.app, "kids", "parent", "created"); err != nil || !created {
		t.Fatalf("composite index: %v %v", created, err)
	}
}

// C9/C10: stale definitions are reported clearly; filters on encrypted fields are refused.
func TestC9StaleDefinitionReported(t *testing.T) {
	e := setup(t)
	e.def(t, "total", "sum", "amount", "")
	kc, _ := e.app.FindCollectionByNameOrId("kids")
	kc.Fields.RemoveByName("amount")
	if err := e.app.Save(kc); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.Verify("parents", "total")
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("verify must say the definition is invalid: %v", err)
	}
}

func TestC10FilterOnEncryptedFieldRefused(t *testing.T) {
	e := setup(t)
	kernel.RegisterSensitiveField(e.kc.Id, "status")
	t.Cleanup(func() { kernel.UnregisterSensitiveField(e.kc.Id, "status") })
	_, err := Add(e.app, Def{Collection: "parents", Field: "cnt", Kind: KindCount, SourceCollection: "kids",
		SourceRelation: "parent", Filter: "status = 'done'"})
	if err == nil || !strings.Contains(err.Error(), "encrypted") {
		t.Fatalf("expected a refusal: %v", err)
	}
}
