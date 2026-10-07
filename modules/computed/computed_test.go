//go:build !no_computed

package computed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app *tests.TestApp
	m   *Module
	mux http.Handler
	pc  *core.Collection
	kc  *core.Collection
}

func buildMux(t *testing.T, app *tests.TestApp) http.Handler {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		h = mux
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)
	open := ""

	pc := core.NewBaseCollection("parents")
	pc.Fields.Add(
		&core.TextField{Name: "title"},
		&core.NumberField{Name: "cnt"},
		&core.NumberField{Name: "total"},
		&core.NumberField{Name: "avg"},
		&core.NumberField{Name: "mn"},
		&core.NumberField{Name: "mx"},
		&core.NumberField{Name: "lst"},
		&core.NumberField{Name: "done_cnt", OnlyInt: true},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	pc.ListRule, pc.ViewRule, pc.CreateRule, pc.UpdateRule = &open, &open, &open, &open
	if err := app.Save(pc); err != nil {
		t.Fatal(err)
	}
	kc := core.NewBaseCollection("kids")
	kc.Fields.Add(
		&core.RelationField{Name: "parent", CollectionId: pc.Id, MaxSelect: 1},
		&core.NumberField{Name: "amount"},
		&core.TextField{Name: "status"},
		&core.AutodateField{Name: "created", OnCreate: true},
	)
	if err := app.Save(kc); err != nil {
		t.Fatal(err)
	}
	e := &env{app: app, m: m, pc: pc, kc: kc, mux: buildMux(t, app)}
	return e
}

func (e *env) def(t *testing.T, field, kind, srcField, filter string) {
	t.Helper()
	if _, err := Add(e.app, Def{Collection: "parents", Field: field, Kind: kind,
		SourceCollection: "kids", SourceRelation: "parent", SourceField: srcField, Filter: filter}); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
}

func (e *env) parent(t *testing.T) *core.Record {
	t.Helper()
	r := core.NewRecord(e.pc)
	r.Set("title", "p")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) kid(t *testing.T, p *core.Record, amount float64, status string) *core.Record {
	t.Helper()
	r := core.NewRecord(e.kc)
	r.Set("parent", p.Id)
	r.Set("amount", amount)
	r.Set("status", status)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) get(t *testing.T, p *core.Record, field string) float64 {
	t.Helper()
	r, err := e.app.FindRecordById("parents", p.Id)
	if err != nil {
		t.Fatal(err)
	}
	return r.GetFloat(field)
}

func (e *env) want(t *testing.T, p *core.Record, field string, v float64) {
	t.Helper()
	if got := e.get(t, p, field); differs(got, v) {
		t.Fatalf("%s = %v, want %v", field, got, v)
	}
}

func TestCountSumAvgMaintained(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	e.def(t, "total", "sum", "amount", "")
	e.def(t, "avg", "avg", "amount", "")
	p := e.parent(t)

	a := e.kid(t, p, 10, "x")
	b := e.kid(t, p, 20, "x")
	e.want(t, p, "cnt", 2)
	e.want(t, p, "total", 30)
	e.want(t, p, "avg", 15)

	a.Set("amount", 40)
	if err := e.app.Save(a); err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "total", 60)
	e.want(t, p, "avg", 30)

	if err := e.app.Delete(b); err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "cnt", 1)
	e.want(t, p, "total", 40)
	if err := e.app.Delete(a); err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "cnt", 0)
	e.want(t, p, "total", 0)
	e.want(t, p, "avg", 0)
}

func TestMinMaxLast(t *testing.T) {
	e := setup(t)
	e.def(t, "mn", "min", "amount", "")
	e.def(t, "mx", "max", "amount", "")
	e.def(t, "lst", "last", "amount", "")
	p := e.parent(t)
	e.kid(t, p, 5, "")
	e.kid(t, p, 9, "")
	e.kid(t, p, 7, "")
	e.want(t, p, "mn", 5)
	e.want(t, p, "mx", 9)
	e.want(t, p, "lst", 7)
}

func TestRelationChangeMovesAggregate(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	e.def(t, "total", "sum", "amount", "")
	p1, p2 := e.parent(t), e.parent(t)
	k := e.kid(t, p1, 10, "")
	e.kid(t, p1, 5, "")
	e.want(t, p1, "total", 15)

	k, _ = e.app.FindRecordById("kids", k.Id) // loaded from the DB, like an update request does
	k.Set("parent", p2.Id)
	if err := e.app.Save(k); err != nil {
		t.Fatal(err)
	}
	e.want(t, p1, "cnt", 1)
	e.want(t, p1, "total", 5)
	e.want(t, p2, "cnt", 1)
	e.want(t, p2, "total", 10)
}

func TestFilterRespectedAndOnlyInt(t *testing.T) {
	e := setup(t)
	e.def(t, "done_cnt", "count", "", `status = "done"`)
	e.def(t, "total", "sum", "amount", `status = "done"`)
	p := e.parent(t)
	k := e.kid(t, p, 10, "open")
	e.kid(t, p, 3, "done")
	e.want(t, p, "done_cnt", 1)
	e.want(t, p, "total", 3)
	k.Set("status", "done") // enters the filter on update
	if err := e.app.Save(k); err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "done_cnt", 2)
	e.want(t, p, "total", 13)
	k.Set("status", "open") // leaves it
	if err := e.app.Save(k); err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "done_cnt", 1)
	e.want(t, p, "total", 3)
}

func TestValidation(t *testing.T) {
	e := setup(t)
	bad := []Def{
		{Collection: "nope", Field: "cnt", Kind: "count", SourceCollection: "kids", SourceRelation: "parent"},
		{Collection: "parents", Field: "title", Kind: "count", SourceCollection: "kids", SourceRelation: "parent"},   // not number
		{Collection: "parents", Field: "missing", Kind: "count", SourceCollection: "kids", SourceRelation: "parent"}, // no field
		{Collection: "parents", Field: "cnt", Kind: "count", SourceCollection: "kids", SourceRelation: "amount"},     // not relation
		{Collection: "parents", Field: "cnt", Kind: "sum", SourceCollection: "kids", SourceRelation: "parent"},       // no source_field
		{Collection: "parents", Field: "cnt", Kind: "sum", SourceCollection: "kids", SourceRelation: "parent", SourceField: "status"},
		{Collection: "parents", Field: "cnt", Kind: "count", SourceCollection: "kids", SourceRelation: "parent", SourceField: "amount"},
		{Collection: "parents", Field: "cnt", Kind: "count", SourceCollection: "kids", SourceRelation: "parent", Filter: "bogus ="},
		{Collection: "parents", Field: "cnt", Kind: "median", SourceCollection: "kids", SourceRelation: "parent"},
	}
	for i, d := range bad {
		if _, err := Add(e.app, d); err == nil {
			t.Fatalf("case %d: expected validation error", i)
		}
	}
	// invalid rows cannot be stored through the record API path either
	coll, _ := e.app.FindCollectionByNameOrId(CollectionName)
	r := core.NewRecord(coll)
	r.Set("collection", "parents")
	r.Set("field", "title")
	r.Set("kind", "count")
	r.Set("source_collection", "kids")
	r.Set("source_relation", "parent")
	if err := e.app.Save(r); err == nil {
		t.Fatal("record validation should reject")
	}
}

func (e *env) do(t *testing.T, su bool, method, url, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if su {
		s, err := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := s.NewAuthToken()
		req.Header.Set("Authorization", tok)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestClientWriteRejected(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	p := e.parent(t)
	e.kid(t, p, 1, "")

	for _, su := range []bool{false, true} {
		code, out := e.do(t, su, "PATCH", "/api/collections/parents/records/"+p.Id, `{"cnt": 99}`)
		if code != 400 || !strings.Contains(toJSON(out), ErrCode) {
			t.Fatalf("su=%v: code=%d body=%v", su, code, out)
		}
		code, _ = e.do(t, su, "PATCH", "/api/collections/parents/records/"+p.Id, `{"cnt+": 5}`)
		if code != 400 {
			t.Fatalf("modifier must be rejected, code=%d", code)
		}
		code, _ = e.do(t, su, "POST", "/api/collections/parents/records", `{"title":"x","cnt": 5}`)
		if code != 400 {
			t.Fatalf("create must be rejected, code=%d", code)
		}
	}
	e.want(t, p, "cnt", 1)

	// other fields and an unchanged re-send still work
	code, _ := e.do(t, false, "PATCH", "/api/collections/parents/records/"+p.Id, `{"title":"y","cnt":1}`)
	if code != 200 {
		t.Fatalf("unchanged re-send: code=%d", code)
	}
	code, _ = e.do(t, false, "POST", "/api/collections/parents/records", `{"title":"z","cnt":0}`)
	if code != 200 {
		t.Fatalf("zero on create: code=%d", code)
	}

	t.Setenv(EnvAllowManual, "1")
	code, _ = e.do(t, true, "PATCH", "/api/collections/parents/records/"+p.Id, `{"cnt": 42}`)
	if code != 200 {
		t.Fatalf("superuser with %s: code=%d", EnvAllowManual, code)
	}
	code, _ = e.do(t, false, "PATCH", "/api/collections/parents/records/"+p.Id, `{"cnt": 43}`)
	if code != 400 {
		t.Fatalf("non-superuser stays rejected: code=%d", code)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestBackfillFixesDriftVerifyDoesNotWrite(t *testing.T) {
	e := setup(t)
	p1, p2 := e.parent(t), e.parent(t)
	// children exist before the definition: values are stale
	e.kid(t, p1, 4, "")
	e.kid(t, p1, 6, "")
	e.kid(t, p2, 1, "")
	e.def(t, "cnt", "count", "", "")
	e.def(t, "total", "sum", "amount", "")
	e.want(t, p1, "cnt", 0)

	reps, err := e.m.Verify("parents", "")
	if err != nil {
		t.Fatal(err)
	}
	drift := 0
	for _, r := range reps {
		drift += r.Drift
	}
	if drift != 4 { // 2 parents x 2 fields
		t.Fatalf("drift = %d, want 4 (%+v)", drift, reps[0])
	}
	e.want(t, p1, "cnt", 0) // verify wrote nothing

	reps, err = e.m.Backfill("parents", "cnt", nil)
	if err != nil || len(reps) != 1 || reps[0].Fixed != 2 {
		t.Fatalf("backfill cnt: %v %+v", err, reps)
	}
	e.want(t, p1, "cnt", 2)
	e.want(t, p1, "total", 0)
	if _, err := e.m.Backfill("parents", "", nil); err != nil {
		t.Fatal(err)
	}
	e.want(t, p1, "total", 10)
	e.want(t, p2, "total", 1)
	reps, _ = e.m.Verify("parents", "")
	for _, r := range reps {
		if r.Drift != 0 {
			t.Fatalf("drift after backfill: %+v", r)
		}
	}
	if n, err := e.m.DriftAll(); err != nil || n != 0 {
		t.Fatalf("DriftAll = %d, %v", n, err)
	}
}

func TestDriftAuditedAndJobKinds(t *testing.T) {
	e := setup(t)
	var mu sync.Mutex
	var actions []string
	SetAuditSink(func(a, c, r string, d map[string]any) { mu.Lock(); actions = append(actions, a); mu.Unlock() })
	t.Cleanup(func() { SetAuditSink(nil) })
	p := e.parent(t)
	e.kid(t, p, 1, "")
	e.def(t, "cnt", "count", "", "")
	if n, _ := e.m.DriftAll(); n != 1 {
		t.Fatalf("drift = %d", n)
	}
	if _, err := e.m.Backfill("parents", "cnt", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(e.app, "parents", "cnt"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(actions, ",")
	mu.Unlock()
	for _, a := range []string{ActionDefCreate, ActionDrift, ActionBackfill, ActionDefDelete} {
		if !strings.Contains(got, a) {
			t.Fatalf("missing audit %s in %s", a, got)
		}
	}
}

func TestLoopProtectionSelfRelation(t *testing.T) {
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
	root := core.NewRecord(nc)
	if err := e.app.Save(root); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c := core.NewRecord(nc)
		c.Set("up", root.Id)
		if err := e.app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	r, _ := e.app.FindRecordById("nodes", root.Id)
	if r.GetFloat("child_count") != 3 {
		t.Fatalf("child_count = %v", r.GetFloat("child_count"))
	}
	if w := e.m.ParentWrites(); w != 3 {
		t.Fatalf("parent writes = %d, want 3 (a loop would write more)", w)
	}
}

func TestBurstInTransactionCoalesces(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	e.def(t, "total", "sum", "amount", "")
	p := e.parent(t)
	before := e.m.ParentWrites()
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		for i := 1; i <= 100; i++ {
			r := core.NewRecord(e.kc)
			r.Set("parent", p.Id)
			r.Set("amount", i)
			if err := tx.Save(r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.want(t, p, "cnt", 100)
	e.want(t, p, "total", 5050)
	// the hooks fire after the commit and the first recompute already sees all 100 rows
	if w := e.m.ParentWrites() - before; w > 2 {
		t.Fatalf("parent writes = %d, want <= 2 (one per field)", w)
	}
}

func TestSequentialInsertsExact(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	p := e.parent(t)
	for i := 0; i < 100; i++ {
		e.kid(t, p, 1, "")
	}
	e.want(t, p, "cnt", 100)
}

func TestConcurrentInsertsExact(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	e.def(t, "total", "sum", "amount", "")
	p := e.parent(t)
	const n = 60
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := core.NewRecord(e.kc)
			r.Set("parent", p.Id)
			r.Set("amount", i)
			if err := e.app.Save(r); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	e.want(t, p, "cnt", n)
	e.want(t, p, "total", float64(n*(n+1)/2))
	if w := e.m.ParentWrites(); w > 2*n {
		t.Fatalf("parent writes = %d", w)
	}
}

func TestParentDeleteCascadeNoError(t *testing.T) {
	e := setup(t)
	e.def(t, "cnt", "count", "", "")
	p := e.parent(t)
	e.kid(t, p, 1, "")
	if err := e.app.Delete(p); err != nil {
		t.Fatal(err)
	}
}
