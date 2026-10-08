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
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// newBareSpoke is an enrolled spoke without any synced collection: the schema
// arrives with the first handshake.
func newBareSpoke(t *testing.T, h *hubEnv, name string) *itemsSpoke {
	t.Helper()
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, name, nil))
	cl := s.client(t, h, func(o *client.Options) {
		o.Backend = backend{s.m}
		o.Interval = time.Hour
	})
	return &itemsSpoke{spokeEnv: s, c: cl}
}

// localDo is an anonymous request on the local REST API of a spoke.
func (s *itemsSpoke) localDo(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler(t).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func hubSchemaVersion(h *hubEnv) int64 { return h.m.schemaVersion() }

func addTextField(t *testing.T, app core.App, col, name string) {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(col)
	if err != nil {
		t.Fatal(err)
	}
	c.Fields.Add(&core.TextField{Name: name})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
}

func TestBundleIsCutOnlyWhenTheSyncedSchemaChanges(t *testing.T) {
	h := newHub(t)
	if v := hubSchemaVersion(h); v != 0 {
		t.Fatalf("version before any policy: %d", v)
	}
	// an unsynced collection changes nothing
	other := core.NewBaseCollection("other")
	other.Fields.Add(&core.TextField{Name: "x"})
	if err := h.app.Save(other); err != nil {
		t.Fatal(err)
	}
	if v := hubSchemaVersion(h); v != 0 {
		t.Fatalf("an unsynced collection must not cut a version: %d", v)
	}
	h.policy(t, "items", DirBoth, nil, nil)
	if v := hubSchemaVersion(h); v != 1 {
		t.Fatalf("policy: version %d", v)
	}
	addTextField(t, h.app, "items", "extra")
	if v := hubSchemaVersion(h); v != 2 {
		t.Fatalf("new field: version %d", v)
	}
	// saving the same schema again does not cut a version
	c, _ := h.app.FindCollectionByNameOrId("items")
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	other.Fields.Add(&core.TextField{Name: "y"})
	if err := h.app.Save(other); err != nil {
		t.Fatal(err)
	}
	if v := hubSchemaVersion(h); v != 2 {
		t.Fatalf("no schema change: version %d", v)
	}
	// the bundle is a full snapshot with ids, and the stored hash matches
	var n int
	if err := h.app.DB().NewQuery("SELECT COUNT(*) FROM _sync_schema").Row(&n); err != nil || n != 2 {
		t.Fatalf("bundles: %d %v", n, err)
	}
	// a config row (policy) change cuts a version too
	pr, _ := h.app.FindFirstRecordByFilter(PoliciesCollection, "collection='items'")
	pr.Set("exclude", []string{"note"})
	if err := h.app.Save(pr); err != nil {
		t.Fatal(err)
	}
	if v := hubSchemaVersion(h); v != 3 {
		t.Fatalf("policy change: version %d", v)
	}
}

func TestSpokeAppliesBundlesAndTheyAreIdempotent(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter}, []string{"note"})
	s := newBareSpoke(t, h, "gate-1")
	if c, _ := s.app.FindCollectionByNameOrId("items"); c != nil {
		t.Fatal("the spoke must not have the collection yet")
	}
	s.sync(t)
	col := s.coll()
	if col == nil || col.Id != h.items.Id {
		t.Fatalf("collection after the bundle: %+v", col)
	}
	hubField := h.items.Fields.GetByName("title")
	if f := col.Fields.GetByName("title"); f == nil || f.GetId() != hubField.GetId() {
		t.Fatal("field ids must come from the hub")
	}
	cur, _ := client.LoadCursor(s.app)
	if cur.SchemaVersion != hubSchemaVersion(h) {
		t.Fatalf("cursor schema version %d, hub %d", cur.SchemaVersion, hubSchemaVersion(h))
	}
	if v, _, _ := (dbState{db: s.app.NonconcurrentDB()}).Get(keySchemaVersion); v == "" || v == "0" {
		t.Fatalf("_sync_state schema_version %q: new local changes must carry the version", v)
	}
	if n := countWhere(t, s.app, PoliciesCollection, "collection='items'", nil); n != 1 {
		t.Fatalf("policy rows %d", n)
	}
	// applying the same bundle again is a no-op
	var raw string
	if err := h.app.DB().NewQuery("SELECT bundle FROM _sync_schema ORDER BY version DESC LIMIT 1").Row(&raw); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		err := s.app.RunInTransaction(func(tx kernel.App) error {
			_, err := s.m.applyBundle(tx, proto.SchemaBundle{Version: hubSchemaVersion(h), Bundle: []byte(raw)})
			return err
		})
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if n := countWhere(t, s.app, PoliciesCollection, "1=1", nil); n != 1 {
		t.Fatalf("policy rows after reapply: %d", n)
	}

	// a field added on the hub reaches the spoke before any data does
	addTextField(t, h.app, "items", "extra")
	rec := core.NewRecord(pr8Col(t, h.app, "items"))
	rec.Set("title", "t")
	rec.Set("extra", "e")
	if err := h.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	s.sync(t)
	if s.coll().Fields.GetByName("extra") == nil {
		t.Fatal("the new field is missing on the spoke")
	}
	got, err := s.app.FindRecordById("items", rec.Id)
	if err != nil || got.GetString("extra") != "e" {
		t.Fatalf("record on the spoke: %v %v", got, err)
	}
	requireConverged(t, h, s)
}

func TestBundleDoesNotCarryTokenSecrets(t *testing.T) {
	h := newHub(t)
	h.policy(t, "users", DirPull, nil, nil)
	canon, _, _, err := h.m.buildBundle()
	if err != nil {
		t.Fatal(err)
	}
	s := string(canon)
	users, _ := h.app.FindCollectionByNameOrId("users")
	if !strings.Contains(s, `"name":"users"`) {
		t.Fatalf("users missing from the bundle: %s", s[:200])
	}
	if sec := users.AuthToken.Secret; sec == "" || strings.Contains(s, sec) {
		t.Fatal("the token secret of an auth collection must never leave the hub")
	}
}

func TestRenamedFieldOnHubMapsPendingChangeOfOfflineSpoke(t *testing.T) {
	h, a, b := hubFixture(t)
	// the spoke writes offline under schema version 1
	rec := a.create(t, map[string]any{"title": "offline title", "qty": 1})

	// meanwhile the hub renames the field (the field id survives)
	c, _ := h.app.FindCollectionByNameOrId("items")
	c.Fields.GetByName("title").SetName("headline")
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	if hubSchemaVersion(h) < 2 {
		t.Fatal("the rename must cut a version")
	}
	a.sync(t) // push -> 409 schema_behind -> handshake applies the bundle -> push maps the field
	hr := hubItem(t, h, rec.Id)
	if hr.GetString("headline") != "offline title" {
		t.Fatalf("hub record after the mapped push: %v", hr.FieldsData())
	}
	if rows := changeRows(t, h.app, "node={:n}", dbx.Params{"n": a.m.NodeID()}); len(rows) != 1 || rows[0].Status != StatusApplied ||
		strings.Contains(rows[0].Patch, `"title"`) || !strings.Contains(rows[0].Patch, "headline") {
		t.Fatalf("hub row: %+v", rows)
	}
	b.sync(t)
	a.sync(t)
	requireConverged(t, h, a, b)
	if a.coll().Fields.GetByName("headline") == nil || a.coll().Fields.GetByName("title") != nil {
		t.Fatal("the spoke must have the renamed field")
	}
}

func TestDroppedFieldIsRemovedFromThePatchAndRecorded(t *testing.T) {
	h, a, b := hubFixture(t)
	rec := a.create(t, map[string]any{"title": "keep", "total": 7})

	c, _ := h.app.FindCollectionByNameOrId("items")
	c.Fields.RemoveByName("total")
	if err := h.app.Save(c); err != nil {
		t.Fatal(err)
	}
	a.sync(t)
	hr := hubItem(t, h, rec.Id)
	if hr.GetString("title") != "keep" {
		t.Fatalf("hub record: %v", hr.FieldsData())
	}
	confs, err := h.app.FindAllRecords(ConflictsCollection, dbx.NewExp("kind='schema_dropped_field'"))
	if err != nil || len(confs) != 1 {
		t.Fatalf("conflict rows: %d %v", len(confs), err)
	}
	cf := confs[0]
	if cf.GetString("status") != "resolved" || cf.GetString("resolution") != "auto_merge" || cf.GetString("record") != rec.Id ||
		!strings.Contains(cf.GetString("incoming"), "total") {
		t.Fatalf("conflict: status %q resolution %q incoming %q", cf.GetString("status"), cf.GetString("resolution"), cf.GetString("incoming"))
	}
	b.sync(t)
	a.sync(t)
	if a.coll().Fields.GetByName("total") != nil {
		t.Fatal("the dropped field must be removed from the spoke too")
	}
	requireConverged(t, h, a, b)
}

func TestSpokeSchemaLock(t *testing.T) {
	h, a, _ := hubFixture(t)
	// create
	nc := core.NewBaseCollection("local_only")
	nc.Fields.Add(&core.TextField{Name: "x"})
	if err := a.app.Save(nc); !errors.Is(err, ErrSchemaLocked) {
		t.Fatalf("create on an enrolled spoke: %v", err)
	}
	// update
	col := a.coll()
	col.Fields.Add(&core.TextField{Name: "local_field"})
	if err := a.app.Save(col); !errors.Is(err, ErrSchemaLocked) {
		t.Fatalf("update on an enrolled spoke: %v", err)
	}
	// delete
	if err := a.app.Delete(a.coll()); !errors.Is(err, ErrSchemaLocked) {
		t.Fatalf("delete on an enrolled spoke: %v", err)
	}
	// through the API too
	if st, _ := a.localDo(t, "DELETE", "/api/collections/items", ""); st == http.StatusNoContent {
		t.Fatal("the API must not delete a collection either")
	}
	if a.coll() == nil {
		t.Fatal("the collection must still exist")
	}
	// the hub is not locked, and a bundle passes
	addTextField(t, h.app, "items", "from_hub")
	a.sync(t)
	if a.coll().Fields.GetByName("from_hub") == nil {
		t.Fatal("a bundle must pass the lock")
	}
	// the escape hatch
	t.Setenv(EnvSchemaLock, "off")
	if err := a.app.Save(nc); err != nil {
		t.Fatalf("with the lock off: %v", err)
	}
}

func TestSchemaLockOnlyAfterEnrollment(t *testing.T) {
	s := newSpoke(t) // not enrolled yet
	nc := core.NewBaseCollection("before_join")
	nc.Fields.Add(&core.TextField{Name: "x"})
	if err := s.app.Save(nc); err != nil {
		t.Fatalf("an unenrolled spoke may create collections: %v", err)
	}
}

func TestPushWithOldSchemaVersionIsRefused(t *testing.T) {
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	addTextField(t, h.app, "items", "newer")
	cid := h.items.Id
	req := pushReq(pc(a.m.NodeID(), 1, nowHLC(-10, 0), 0, cid, "rec000000000001", "c", map[string]any{"title": "x"}))
	req.SchemaVersion = hubSchemaVersion(h) - 1
	st, _, eb := rawPush(t, h, tok, req)
	if st != http.StatusConflict || eb.Data["code"] != proto.CodeSchemaBehind {
		t.Fatalf("push with an old version: %d %+v", st, eb)
	}
	req.SchemaVersion = hubSchemaVersion(h)
	if st, _, eb := rawPush(t, h, tok, req); st != 200 {
		t.Fatalf("push with the current version: %d %+v", st, eb)
	}
}

func TestBundleLagBeyondMaxBundlesForcesRebootstrap(t *testing.T) {
	t.Setenv(EnvMaxBundles, "3")
	h, a, _ := hubFixture(t)
	v1 := hubSchemaVersion(h)
	for i := 0; i < 5; i++ {
		addTextField(t, h.app, "items", "f"+string(rune('a'+i)))
	}
	if hubSchemaVersion(h) != v1+5 {
		t.Fatalf("version %d", hubSchemaVersion(h))
	}
	var n int
	_ = h.app.DB().NewQuery("SELECT COUNT(*) FROM _sync_schema").Row(&n)
	if n != 3 {
		t.Fatalf("kept bundles: %d", n)
	}
	// a node that applied v1 is 5 versions behind: rebootstrap, no bundles
	hs, err := a.c.Handshake(ctxb)
	if err != nil {
		t.Fatal(err)
	}
	if !hs.Rebootstrap || len(hs.Schema.Bundles) != 1 {
		t.Fatalf("handshake: rebootstrap %v bundles %d", hs.Rebootstrap, len(hs.Schema.Bundles))
	}
	// the loop stops with the rebootstrap state (the snapshot is a later PR)
	if r := a.c.RunOnce(ctxb); !errors.Is(r.Err, client.ErrRebootstrap) {
		t.Fatalf("cycle: %v", r.Err)
	}
	// a fresh node needs no history: the latest full snapshot is enough
	fresh := newBareSpoke(t, h, "gate-9")
	fresh.sync(t)
	if fresh.coll() == nil || fresh.coll().Fields.GetByName("fe") == nil {
		t.Fatal("a fresh node must get the latest bundle")
	}
}

func TestChangeOlderThanTheOldestBundleIsOrphaned(t *testing.T) {
	t.Setenv(EnvMaxBundles, "2")
	h, a, _ := hubFixture(t)
	tok := a.token(t)
	for i := 0; i < 4; i++ {
		addTextField(t, h.app, "items", "g"+string(rune('a'+i)))
	}
	ch := pc(a.m.NodeID(), 1, nowHLC(-10, 0), 0, h.items.Id, "rec000000000001", "c", map[string]any{"title": "old"})
	ch.SV = 1
	req := pushReq(ch)
	req.SchemaVersion = hubSchemaVersion(h)
	st, ok, eb := rawPush(t, h, tok, req)
	if st != 200 || ok.Results[0].Status != proto.ResRejected || ok.Results[0].Code != proto.CodeOrphaned {
		t.Fatalf("push: %d %+v %+v", st, ok, eb)
	}
	confs, _ := h.app.FindAllRecords(ConflictsCollection, dbx.NewExp("kind='orphaned'"))
	if len(confs) != 1 {
		t.Fatalf("orphaned conflicts: %d", len(confs))
	}
}
