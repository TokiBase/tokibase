//go:build !no_sync

package sync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/computed"
	"github.com/tokibase/tokibase/modules/crypto"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/filesystem"
	"github.com/tokibase/tokibase/tools/hook"
)

type env struct {
	app   *tests.TestApp
	m     *Module
	mux   http.Handler
	su    *core.Record
	usr   *core.Record
	items *core.Collection
}

type chg struct {
	Seq        int64  `db:"seq"`
	Node       string `db:"node"`
	OriginSeq  int64  `db:"origin_seq"`
	HLC        int64  `db:"hlc"`
	BaseHLC    int64  `db:"base_hlc"`
	Collection string `db:"collection"`
	Record     string `db:"record"`
	Op         string `db:"op"`
	Patch      string `db:"patch"`
	Hash       []byte `db:"hash"`
	Actor      string `db:"actor"`
	Tx         string `db:"tx"`
	Status     string `db:"status"`
}

func newApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	return app
}

func buildMux(t *testing.T, app *tests.TestApp) http.Handler {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		var err error
		h, err = se.Router.BuildMux()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func setupWith(t *testing.T, app *tests.TestApp, role Role) *env {
	t.Helper()
	m := RegisterRole(app, role)
	open := ""
	c := core.NewBaseCollection("items")
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
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	app.Settings().Batch.Enabled = true
	app.Settings().Batch.MaxRequests = 50
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	e := &env{app: app, m: m, items: c, mux: buildMux(t, app)}
	e.su, _ = app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	e.usr, _ = app.FindAuthRecordByEmail("users", "test@example.com")
	return e
}

func setup(t *testing.T) *env {
	t.Helper()
	e := setupWith(t, newApp(t), RoleHub)
	e.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter, "tags": TypeSet}, []string{"note"})
	return e
}

func (e *env) policy(t *testing.T, coll, dir string, types map[string]string, exclude []string) {
	t.Helper()
	pc, err := e.app.FindCollectionByNameOrId(PoliciesCollection)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(pc)
	r.Set("collection", coll)
	r.Set("direction", dir)
	r.Set("enabled", true)
	if types != nil {
		r.Set("field_types", types)
	}
	if exclude != nil {
		r.Set("exclude", exclude)
	}
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (e *env) pol(t *testing.T) *policy {
	t.Helper()
	p, err := e.m.pol.For(e.items)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) item(t *testing.T, kv ...any) *core.Record {
	t.Helper()
	r := core.NewRecord(e.items)
	for i := 0; i < len(kv); i += 2 {
		r.Set(kv[i].(string), kv[i+1])
	}
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) changes(t *testing.T) []chg {
	t.Helper()
	var out []chg
	if err := e.app.DB().NewQuery("SELECT seq,node,origin_seq,hlc,base_hlc,collection,record,op,patch,hash,actor,tx,status FROM _changes ORDER BY seq").All(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.app.DB().NewQuery("SELECT COUNT(*) FROM " + table).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func patchOf(t *testing.T, c chg) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Patch), &m); err != nil {
		t.Fatalf("patch %q: %v", c.Patch, err)
	}
	return m
}

func (e *env) do(t *testing.T, who *core.Record, method, url, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != nil {
		tok, err := who.NewAuthToken()
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", tok)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func jsonEq(t *testing.T, got any, want string) {
	t.Helper()
	g, _ := json.Marshal(got)
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	wb, _ := json.Marshal(w)
	if !bytes.Equal(g, wb) {
		t.Fatalf("got %s, want %s", g, wb)
	}
}

func TestCaptureCreateUpdateDelete(t *testing.T) {
	e := setup(t)

	r := e.item(t, "title", "Ticket 1", "qty", 5, "tags", []string{"a", "b"}, "note", "private", "total", 99)
	rows := e.changes(t)
	if len(rows) != 1 {
		t.Fatalf("create: %d rows", len(rows))
	}
	c := rows[0]
	if c.Op != "c" || c.Collection != e.items.Id || c.Record != r.Id || c.Status != "local" || c.Tx != "" {
		t.Fatalf("create row: %+v", c)
	}
	if c.Node != e.m.NodeID() || c.Node == "" || c.OriginSeq != c.Seq || c.Seq != 1 || c.BaseHLC != 0 {
		t.Fatalf("identity: %+v node=%s", c, e.m.NodeID())
	}
	if c.Actor != ActorNode {
		t.Fatalf("actor without request must be %q, got %q", ActorNode, c.Actor)
	}
	p := patchOf(t, c)
	if p["title"] != "Ticket 1" || p["qty"] != float64(5) {
		t.Fatalf("create patch: %v", p)
	}
	jsonEq(t, p["tags"], `["a","b"]`)
	for _, banned := range []string{"id", "photo", "note"} {
		if _, ok := p[banned]; ok {
			t.Fatalf("%s must not be in the patch: %v", banned, p)
		}
	}
	for _, need := range []string{"created", "updated", "meta", "secret"} {
		if _, ok := p[need]; !ok {
			t.Fatalf("%s missing from the create patch: %v", need, p)
		}
	}
	fresh, _ := e.app.FindRecordById("items", r.Id)
	want, err := RecordHash(fresh, e.pol(t))
	if err != nil || !bytes.Equal(want, c.Hash) || len(c.Hash) != sha256.Size {
		t.Fatalf("hash mismatch: %x vs %x (%v)", want, c.Hash, err)
	}
	var meta struct {
		HLC  int64  `db:"hlc"`
		Node string `db:"node"`
		Hash []byte `db:"hash"`
	}
	if err := e.app.DB().NewQuery("SELECT hlc,node,hash FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": e.items.Id, "r": r.Id}).One(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.HLC != c.HLC || meta.Node != c.Node || !bytes.Equal(meta.Hash, c.Hash) {
		t.Fatalf("meta: %+v", meta)
	}

	// update: changed fields only, counter delta, set diff
	r, _ = e.app.FindRecordById("items", r.Id)
	// the update must land in a later millisecond than the create so `updated` changes
	time.Sleep(2 * time.Millisecond)
	r.Set("title", "Ticket 1b")
	r.Set("qty", 8)
	r.Set("tags", []string{"b", "c"})
	r.Set("note", "changed but excluded")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	rows = e.changes(t)
	if len(rows) != 2 {
		t.Fatalf("update: %d rows", len(rows))
	}
	u := rows[1]
	if u.Op != "u" || u.BaseHLC != c.HLC || u.HLC <= c.HLC {
		t.Fatalf("update row: %+v", u)
	}
	p = patchOf(t, u)
	jsonEq(t, p["qty"], `{"$inc":3}`)
	jsonEq(t, p["tags"], `{"$add":["c"],"$rm":["a"]}`)
	if p["title"] != "Ticket 1b" {
		t.Fatalf("title: %v", p)
	}
	if _, ok := p["created"]; ok {
		t.Fatalf("unchanged field in patch: %v", p)
	}
	if _, ok := p["updated"]; !ok {
		t.Fatalf("autodate change must be part of a real update: %v", p)
	}
	fresh, _ = e.app.FindRecordById("items", r.Id)
	if want, _ := RecordHash(fresh, e.pol(t)); !bytes.Equal(want, u.Hash) {
		t.Fatal("update hash mismatch")
	}

	// a save that changes nothing synced writes no row
	r, _ = e.app.FindRecordById("items", r.Id)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if n := len(e.changes(t)); n != 2 {
		t.Fatalf("empty patch must be skipped, rows=%d", n)
	}

	// delete
	if err := e.app.Delete(r); err != nil {
		t.Fatal(err)
	}
	rows = e.changes(t)
	if len(rows) != 3 {
		t.Fatalf("delete: %d rows", len(rows))
	}
	d := rows[2]
	if d.Op != "d" || d.Patch != "{}" || d.Hash != nil || d.BaseHLC != u.HLC || d.HLC <= u.HLC {
		t.Fatalf("delete row: %+v", d)
	}
	var tomb struct {
		Kind string `db:"kind"`
		HLC  int64  `db:"hlc"`
		Node string `db:"node"`
	}
	if err := e.app.DB().NewQuery("SELECT kind,hlc,node FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": e.items.Id, "r": r.Id}).One(&tomb); err != nil {
		t.Fatal(err)
	}
	if tomb.Kind != "delete" || tomb.HLC != d.HLC || tomb.Node != c.Node {
		t.Fatalf("tombstone: %+v", tomb)
	}
	if n := e.count(t, "_sync_meta"); n != 0 {
		t.Fatalf("meta row must be removed on delete, got %d", n)
	}
}

func TestUncapturedCollectionsWriteNothing(t *testing.T) {
	e := setup(t)
	// no policy for "other"
	c := core.NewBaseCollection("other")
	c.Fields.Add(&core.TextField{Name: "x"})
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("x", "1")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	// direction none
	e.policy(t, "other", DirNone, nil, nil)
	r.Set("x", "2")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	// system collections: never captured even with a policy row
	if n := len(e.changes(t)); n != 0 {
		t.Fatalf("captured %d rows", n)
	}
}

func TestRollbackLeavesNoRow(t *testing.T) {
	e := setup(t)

	// the surrounding transaction fails after a successful save
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		r := core.NewRecord(e.items)
		r.Set("title", "x")
		if err := tx.Save(r); err != nil {
			return err
		}
		return os.ErrInvalid
	})
	if err == nil {
		t.Fatal("expected the tx error")
	}
	if n := e.count(t, "_changes") + e.count(t, "_sync_meta") + e.count(t, "items"); n != 0 {
		t.Fatalf("rollback left %d rows", n)
	}

	// an inner handler fails after the write: record and change vanish together
	e.app.OnRecordCreateExecute("items").Bind(&hook.Handler[*core.RecordEvent]{
		Id: "failafter", Priority: 50,
		Func: func(ev *core.RecordEvent) error {
			if err := ev.Next(); err != nil {
				return err
			}
			return os.ErrPermission
		},
	})
	r := core.NewRecord(e.items)
	r.Set("title", "y")
	if err := e.app.Save(r); err == nil {
		t.Fatal("expected the hook error")
	}
	if n := e.count(t, "_changes") + e.count(t, "_sync_meta") + e.count(t, "items"); n != 0 {
		t.Fatalf("failed write left %d rows (e.App replacement did not reach the DB write?)", n)
	}
	e.app.OnRecordCreateExecute("items").Unbind("failafter")

	// and the happy path still works afterwards
	e.item(t, "title", "z")
	if n := len(e.changes(t)); n != 1 {
		t.Fatalf("rows=%d", n)
	}
}

func TestExecuteHookAppReachesDBWrite(t *testing.T) {
	// The record insert must run on the transaction app that the capture hook
	// installed in e.App: seen from a handler bound inside it (priority 50).
	e := setup(t)
	var inTx bool
	e.app.OnRecordCreateExecute("items").Bind(&hook.Handler[*core.RecordEvent]{
		Id: "probe", Priority: 50,
		Func: func(ev *core.RecordEvent) error {
			inTx = ev.App.IsTransactional()
			return ev.Next()
		},
	})
	e.item(t, "title", "probe")
	if !inTx {
		t.Fatal("inner Execute handlers must see the capture transaction app")
	}
	// and e.App is restored for the after-success events (non-transactional)
	var afterTx = true
	e.app.OnRecordAfterCreateSuccess("items").Bind(&hook.Handler[*core.RecordEvent]{
		Id: "probe2",
		Func: func(ev *core.RecordEvent) error {
			afterTx = ev.App.IsTransactional()
			return ev.Next()
		},
	})
	e.item(t, "title", "probe2")
	if afterTx {
		t.Fatal("after-success events must get the original app back")
	}
}

func TestTxGroups(t *testing.T) {
	e := setup(t)
	e.item(t, "title", "alone")
	if err := e.app.RunInTransaction(func(tx kernel.App) error {
		for _, title := range []string{"g1", "g2", "g3"} {
			r := core.NewRecord(e.items)
			r.Set("title", title)
			if err := tx.Save(r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows := e.changes(t)
	if len(rows) != 4 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Tx != "" {
		t.Fatalf("single write must have no tx id: %q", rows[0].Tx)
	}
	id := rows[1].Tx
	if id == "" || rows[2].Tx != id || rows[3].Tx != id {
		t.Fatalf("group ids: %q %q %q", rows[1].Tx, rows[2].Tx, rows[3].Tx)
	}
	// the state map is cleaned up after the transaction
	n := 0
	e.m.txs.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d leaked tx states", n)
	}
}

func TestBatchCapturesAllWithSameTx(t *testing.T) {
	e := setup(t)
	seed := e.item(t, "title", "seed", "qty", 1)

	body := `{"requests":[
	 {"method":"POST","url":"/api/collections/items/records","body":{"title":"b1","qty":2}},
	 {"method":"POST","url":"/api/collections/items/records","body":{"title":"b2","qty":3}},
	 {"method":"PATCH","url":"/api/collections/items/records/` + seed.Id + `","body":{"qty":10}}
	]}`
	code, out := e.do(t, e.su, "POST", "/api/batch", body)
	if code != 200 {
		t.Fatalf("batch: %d %s", code, out)
	}
	rows := e.changes(t)
	if len(rows) != 4 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Tx != "" {
		t.Fatal("seed must be alone")
	}
	tx := rows[1].Tx
	if tx == "" || rows[2].Tx != tx || rows[3].Tx != tx {
		t.Fatalf("batch rows must share one tx id: %q %q %q", rows[1].Tx, rows[2].Tx, rows[3].Tx)
	}
	if rows[3].Op != "u" {
		t.Fatalf("op: %+v", rows[3])
	}
	jsonEq(t, patchOf(t, rows[3])["qty"], `{"$inc":9}`)
	wantActor := ActorNode // superusers never hold a grant
	for _, r := range rows[1:] {
		if r.Actor != wantActor {
			t.Fatalf("actor = %q, want %q", r.Actor, wantActor)
		}
	}

	// a failing batch rolls everything back, rows included
	before := len(e.changes(t))
	bad := `{"requests":[
	 {"method":"POST","url":"/api/collections/items/records","body":{"title":"ok"}},
	 {"method":"PATCH","url":"/api/collections/items/records/nonexistent","body":{"qty":1}}
	]}`
	if code, _ := e.do(t, e.su, "POST", "/api/batch", bad); code == 200 {
		t.Fatal("batch should fail")
	}
	if n := len(e.changes(t)); n != before {
		t.Fatalf("failed batch left %d rows", n-before)
	}
}

func TestActorFromRequest(t *testing.T) {
	e := setup(t)
	// a user without a grant (and every hub write) is captured as "node"
	code, out := e.do(t, e.usr, "POST", "/api/collections/items/records", `{"title":"by user"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	rows := e.changes(t)
	if len(rows) != 1 || rows[0].Actor != ActorNode {
		t.Fatalf("actor: %+v", rows)
	}
	// anonymous request and Go code: "node"
	code, out = e.do(t, nil, "POST", "/api/collections/items/records", `{"title":"anon"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	if rows = e.changes(t); rows[1].Actor != ActorNode {
		t.Fatalf("anonymous actor: %q", rows[1].Actor)
	}
	// the stash is cleared
	n := 0
	e.m.stash.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d leaked stash entries", n)
	}
}

func TestCryptoCiphertextCapturedVerbatim(t *testing.T) {
	crypto.WaitForServers = false
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	t.Setenv(crypto.EnvMasterKey, base64.StdEncoding.EncodeToString(key))
	app := newApp(t)
	cm := crypto.Register(app)
	if err := crypto.EnsureSchema(app); err != nil {
		t.Fatal(err)
	}
	e := setupWith(t, app, RoleHub)
	e.policy(t, "items", DirBoth, nil, nil)
	if _, err := crypto.Enable(app, "items", "secret", crypto.ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	cm.Invalidate()

	code, out := e.do(t, e.su, "POST", "/api/collections/items/records", `{"title":"t","secret":"my plaintext"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	var created struct {
		Id string `json:"id"`
	}
	_ = json.Unmarshal(out, &created)

	var stored string
	if err := app.DB().NewQuery("SELECT secret FROM items WHERE id={:id}").Bind(dbx.Params{"id": created.Id}).Row(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || stored == "my plaintext" {
		t.Fatalf("not encrypted at rest: %q", stored)
	}
	rows := e.changes(t)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	p := patchOf(t, rows[0])
	if p["secret"] != stored {
		t.Fatalf("patch must carry the stored ciphertext verbatim:\n patch  %v\n stored %v", p["secret"], stored)
	}
	if strings.Contains(rows[0].Patch, "my plaintext") {
		t.Fatal("plaintext leaked into _changes")
	}
	// the hash covers the ciphertext
	fresh, _ := app.FindRecordById("items", created.Id)
	if want, _ := RecordHash(fresh, e.pol(t)); !bytes.Equal(want, rows[0].Hash) {
		t.Fatal("hash must be computed over the stored ciphertext")
	}

	// an update that does not touch the encrypted field keeps the ciphertext and does not re-emit it
	code, out = e.do(t, e.su, "PATCH", "/api/collections/items/records/"+created.Id, `{"title":"t2"}`)
	if code != 200 {
		t.Fatalf("%d %s", code, out)
	}
	rows = e.changes(t)
	if _, has := patchOf(t, rows[1])["secret"]; has {
		t.Fatalf("untouched ciphertext re-emitted: %s", rows[1].Patch)
	}
}

func TestComputedOnlyUpdateCapturesNothingAndIsDerived(t *testing.T) {
	app := newApp(t)
	computed.Register(app)
	e := setupWith(t, app, RoleHub)
	e.policy(t, "items", DirBoth, nil, nil)

	lines := core.NewBaseCollection("lines")
	lines.Fields.Add(
		&core.RelationField{Name: "item", CollectionId: e.items.Id, MaxSelect: 1},
		&core.NumberField{Name: "amount"},
	)
	if err := app.Save(lines); err != nil {
		t.Fatal(err)
	}
	if kernel.IsDerived(e.items.Id, "total") {
		t.Fatal("not derived before the definition exists")
	}
	if _, err := computed.Add(app, computed.Def{Collection: "items", Field: "total", Kind: computed.KindSum,
		SourceCollection: "lines", SourceRelation: "item", SourceField: "amount"}); err != nil {
		t.Fatal(err)
	}
	if !kernel.IsDerived(e.items.Id, "total") {
		t.Fatal("computed must register its target field as derived")
	}
	if got := kernel.DerivedFieldsOf(e.items.Id); len(got) != 1 || got[0] != "total" {
		t.Fatalf("DerivedFieldsOf = %v", got)
	}

	it := e.item(t, "title", "parent")
	if n := len(e.changes(t)); n != 1 {
		t.Fatalf("rows=%d", n)
	}
	l := core.NewRecord(lines)
	l.Set("item", it.Id)
	l.Set("amount", 25)
	if err := app.Save(l); err != nil {
		t.Fatal(err)
	}
	got, _ := app.FindRecordById("items", it.Id)
	if got.GetFloat("total") != 25 {
		t.Fatalf("computed did not run: total=%v", got.GetFloat("total"))
	}
	if n := len(e.changes(t)); n != 1 {
		t.Fatalf("a computed-only parent update must capture nothing, rows=%d", n)
	}
	// the derived field is also left out of the create patch and the hash
	if _, has := patchOf(t, e.changes(t)[0])["total"]; has {
		t.Fatal("derived field in patch")
	}

	// removing the definition unregisters it
	if ok, err := computed.Remove(app, "items", "total"); err != nil || !ok {
		t.Fatalf("remove: %v %v", ok, err)
	}
	if kernel.IsDerived(e.items.Id, "total") {
		t.Fatal("must be unregistered after the definition is removed")
	}
}

func TestFileFieldsExcluded(t *testing.T) {
	e := setup(t)
	f, err := filesystem.NewFileFromBytes([]byte("hello"), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	r := e.item(t, "title", "with file", "photo", f)
	rows := e.changes(t)
	if _, has := patchOf(t, rows[0])["photo"]; has {
		t.Fatalf("file field in patch: %s", rows[0].Patch)
	}
	// the hash does not depend on the file
	fresh, _ := e.app.FindRecordById("items", r.Id)
	h1, _ := RecordHash(fresh, nil)
	fresh.SetRaw("photo", "other_name.txt")
	h2, _ := RecordHash(fresh, nil)
	if !bytes.Equal(h1, h2) {
		t.Fatal("file field must not be part of the canonical hash")
	}
	// a file-only update captures nothing
	n := len(e.changes(t))
	r, _ = e.app.FindRecordById("items", r.Id)
	f2, _ := filesystem.NewFileFromBytes([]byte("second"), "b.txt")
	r.Set("photo", f2)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if got := len(e.changes(t)); got != n {
		t.Fatalf("file-only update wrote a row (%d -> %d)", n, got)
	}
}

func TestTombstoneGuardOnCreate(t *testing.T) {
	e := setup(t)
	r := e.item(t, "title", "gone")
	id := r.Id
	if err := e.app.Delete(r); err != nil {
		t.Fatal(err)
	}
	before := len(e.changes(t))
	again := core.NewRecord(e.items)
	again.Id = id
	again.Set("title", "zombie")
	err := e.app.Save(again)
	if err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("expected the tombstone error, got %v", err)
	}
	if n := e.count(t, "items"); n != 0 {
		t.Fatalf("items=%d", n)
	}
	if n := len(e.changes(t)); n != before {
		t.Fatal("rejected create must not write a change")
	}
	// another id is fine
	e.item(t, "title", "fresh")
}

func TestLegalTombstoneIsImmutable(t *testing.T) {
	e := setup(t)
	exec := func(q string) error {
		_, err := e.app.DB().NewQuery(q).Execute()
		return err
	}
	ins := func(rec, kind string) {
		t.Helper()
		if err := exec("INSERT INTO _sync_tombstones (collection,record,kind,hlc,node,created) VALUES ('c','" + rec + "','" + kind + "',1,'n','2026-10-08 00:00:00.000Z')"); err != nil {
			t.Fatal(err)
		}
	}
	ins("legal1", "legal")
	ins("del1", "delete")

	if err := exec("UPDATE _sync_tombstones SET reason='x' WHERE record='legal1'"); err == nil || !strings.Contains(err.Error(), "legal tombstone is permanent") {
		t.Fatalf("update of a legal tombstone: %v", err)
	}
	if err := exec("DELETE FROM _sync_tombstones WHERE record='legal1'"); err == nil || !strings.Contains(err.Error(), "legal tombstone is permanent") {
		t.Fatalf("delete of a legal tombstone: %v", err)
	}
	if err := exec("DELETE FROM _sync_tombstones WHERE collection='c'"); err == nil {
		t.Fatal("a bulk prune must not remove legal rows")
	}
	if err := exec("DELETE FROM _sync_tombstones WHERE record='del1'"); err != nil {
		t.Fatalf("delete tombstones are prunable: %v", err)
	}
	if n := e.count(t, "_sync_tombstones"); n != 1 {
		t.Fatalf("tombstones=%d", n)
	}
	if err := exec("INSERT INTO _sync_tombstones (collection,record,kind,hlc,node,created) VALUES ('c','x','bogus',1,'n','t')"); err == nil {
		t.Fatal("kind check")
	}
}

func TestRoleOffCapturesNothing(t *testing.T) {
	app := newApp(t)
	if m := RegisterRole(app, RoleOff); m != nil {
		t.Fatal("role off must not register a module")
	}
	t.Setenv(EnvRole, "")
	if m := Register(app); m != nil {
		t.Fatal("default role must be off")
	}
	if Enabled() {
		t.Fatal("Enabled")
	}
	if app.HasTable("_changes") || app.HasTable("_sync_state") {
		t.Fatal("role off must not create tables")
	}
	c := core.NewBaseCollection("things")
	c.Fields.Add(&core.TextField{Name: "x"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("x", "1")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	if app.HasTable("_changes") {
		t.Fatal("no tables")
	}
	s, err := GetStatus(app, RoleOff)
	if err != nil || s.Role != "off" || s.Pending != 0 {
		t.Fatalf("status: %+v %v", s, err)
	}
}

func TestRoleFromEnv(t *testing.T) {
	for in, want := range map[string]Role{"": RoleOff, "off": RoleOff, "garbage": RoleOff, "hub": RoleHub, " SPOKE ": RoleSpoke} {
		t.Setenv(EnvRole, in)
		if got := RoleFromEnv(); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestReplicaApplyWritesMetaOnly(t *testing.T) {
	e := setup(t)
	var noCtx context.Context
	o := &kernel.SyncOrigin{Mode: kernel.SyncModePull, Node: "nhub", HLC: uint64(hlc.Make(1_700_000_000_000, 4)), ChangeID: "nhub:7", Actor: "rec:x:y"}
	ctx := kernel.WithSyncOrigin(context.Background(), o)
	if !kernel.IsSyncReplica(ctx) || kernel.SyncOriginFrom(ctx) != o || kernel.IsSyncReplica(context.Background()) || kernel.SyncOriginFrom(noCtx) != nil {
		t.Fatal("origin helpers")
	}

	r := core.NewRecord(e.items)
	r.Set("title", "from hub")
	if err := e.app.SaveWithContext(ctx, r); err != nil {
		t.Fatal(err)
	}
	if n := len(e.changes(t)); n != 0 {
		t.Fatalf("pull apply wrote %d _changes rows", n)
	}
	var meta struct {
		HLC  int64  `db:"hlc"`
		Node string `db:"node"`
	}
	if err := e.app.DB().NewQuery("SELECT hlc,node FROM _sync_meta WHERE record={:r}").Bind(dbx.Params{"r": r.Id}).One(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.HLC != int64(o.HLC) || meta.Node != "nhub" {
		t.Fatalf("meta: %+v", meta)
	}
	if e.m.Clock().Last() < hlc.HLC(o.HLC) {
		t.Fatal("clock must observe the applied HLC")
	}

	// a local write after it carries the pulled clock as base
	fresh, _ := e.app.FindRecordById("items", r.Id)
	fresh.Set("title", "local edit")
	if err := e.app.Save(fresh); err != nil {
		t.Fatal(err)
	}
	rows := e.changes(t)
	if len(rows) != 1 || rows[0].BaseHLC != int64(o.HLC) || rows[0].HLC <= int64(o.HLC) {
		t.Fatalf("local edit after pull: %+v", rows)
	}

	// replica delete: tombstone yes, change row no
	if err := e.app.DeleteWithContext(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	if len(e.changes(t)) != 1 || e.count(t, "_sync_tombstones") != 1 || e.count(t, "_sync_meta") != 0 {
		t.Fatal("replica delete: tombstone only")
	}
}

func TestHLCFloorPersistedAndRestored(t *testing.T) {
	e := setup(t)
	e.m.Clock().SetFloorEvery(2)
	e.item(t, "title", "1")
	if _, ok, _ := (dbState{db: e.app.DB()}).Get(hlc.FloorKey); ok {
		t.Fatal("floor written too early")
	}
	e.item(t, "title", "2")
	v, ok, err := (dbState{db: e.app.DB()}).Get(hlc.FloorKey)
	if err != nil || !ok {
		t.Fatalf("floor not persisted: %v %v", ok, err)
	}
	floor, err := hlc.Parse(v)
	if err != nil || floor == 0 {
		t.Fatal(err)
	}
	last := e.m.Clock().Last()

	// _changes pruned: a restart (Init) must not go below the floor
	if _, err := e.app.DB().NewQuery("DELETE FROM _changes").Execute(); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Init(); err != nil {
		t.Fatal(err)
	}
	if got := e.m.Clock().Last(); got < floor || got > last {
		t.Fatalf("restart clock %v not in [%v, %v]", got, floor, last)
	}
	if n := e.m.Clock().Now(); n <= floor {
		t.Fatal("clock went backwards after restart")
	}
	// the node id survives a restart
	id := e.m.NodeID()
	_ = e.m.Init()
	if e.m.NodeID() != id || id == "" || id[0] != 'h' {
		t.Fatalf("node id: %q vs %q", id, e.m.NodeID())
	}

	// only a committed transaction marks the floor as persisted
	c := e.m.Clock()
	c.SetFloorEvery(1)
	e.item(t, "title", "committed")
	if _, due := c.NeedsFloor(); due {
		t.Fatal("a committed write must mark the floor as persisted")
	}
	_ = e.app.RunInTransaction(func(tx kernel.App) error {
		r := core.NewRecord(e.items)
		r.Set("title", "rb")
		if err := tx.Save(r); err != nil {
			return err
		}
		return os.ErrInvalid
	})
	if _, due := c.NeedsFloor(); !due {
		t.Fatal("floor must stay due after a rollback")
	}
}

func TestStatus(t *testing.T) {
	e := setup(t)
	s, err := GetStatus(e.app, RoleHub)
	if err != nil || s.Role != "hub" || s.NodeID != e.m.NodeID() || s.Pending != 0 || s.LastHLC != "" {
		t.Fatalf("empty: %+v %v", s, err)
	}
	e.item(t, "title", "a")
	e.item(t, "title", "b")
	s, _ = GetStatus(e.app, RoleHub)
	if s.Pending != 2 || len(s.LastHLC) != 16 {
		t.Fatalf("status: %+v", s)
	}
	last := e.changes(t)[1]
	if s.LastHLC != hlc.HLC(last.HLC).String() {
		t.Fatalf("last hlc %s vs %s", s.LastHLC, hlc.HLC(last.HLC))
	}

	cmd := NewCommand(e.app)
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"status", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got Status
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got.Pending != 2 || got.NodeID != s.NodeID {
		t.Fatalf("json: %s %v", buf.String(), err)
	}
}

func TestPolicyCacheInvalidatedOnChange(t *testing.T) {
	e := setupWith(t, newApp(t), RoleHub)
	e.item(t, "title", "before policy")
	if n := e.count(t, "_changes"); n != 0 {
		t.Fatal("no policy, no capture")
	}
	e.policy(t, "items", DirBoth, nil, nil)
	e.item(t, "title", "after policy")
	if n := e.count(t, "_changes"); n != 1 {
		t.Fatalf("policy must apply immediately, rows=%d", n)
	}
	pr, _ := e.app.FindFirstRecordByFilter(PoliciesCollection, "collection='items'")
	pr.Set("enabled", false)
	if err := e.app.Save(pr); err != nil {
		t.Fatal(err)
	}
	e.item(t, "title", "disabled")
	if n := e.count(t, "_changes"); n != 1 {
		t.Fatalf("disabled policy must stop capture, rows=%d", n)
	}
}
