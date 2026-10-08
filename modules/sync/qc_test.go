//go:build !no_sync

package sync

import (
	"bytes"
	"errors"
	"fmt"
	stdsync "sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tools/types"
)

// S1: raw writers on the concurrent pool must not make captured writes fail
// with SQLITE_BUSY_SNAPSHOT (the capture tx takes the write lock first).
func TestCaptureWithConcurrentRawWriter(t *testing.T) {
	e := setup(t)
	if _, err := e.app.DB().NewQuery("CREATE TABLE scratch (k INTEGER)").Execute(); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg stdsync.WaitGroup
	var rawErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := e.app.DB().NewQuery("INSERT INTO scratch (k) VALUES (1)").Execute(); err != nil {
				rawErr = err
				return
			}
		}
	}()
	rec := e.item(t, "title", "base")
	for i := 0; i < 60; i++ {
		rec.Set("title", fmt.Sprintf("t%d", i))
		if err := e.app.Save(rec); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("update %d failed under a raw writer: %v", i, err)
		}
		if i%10 == 0 {
			e.item(t, "title", "c")
		}
	}
	close(stop)
	wg.Wait()
	if rawErr != nil {
		t.Fatalf("raw writer: %v", rawErr)
	}
	if n := e.count(t, "scratch"); n == 0 {
		t.Fatal("raw writer never ran")
	}
}

// S2: a swallowed capture error must not leave the record in the outer tx.
func TestSwallowedCaptureErrorLeavesNoRecord(t *testing.T) {
	e := setup(t)
	keep := e.item(t, "title", "keep") // a committed row before the failure
	if _, err := e.app.DB().NewQuery("DROP TABLE _changes").Execute(); err != nil {
		t.Fatal(err)
	}
	var inner, deleteErr error
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		r := core.NewRecord(e.items)
		r.Set("title", "lost")
		inner = tx.Save(r)
		deleteErr = tx.Delete(keep) // delete also fails to capture
		return nil                  // the caller swallows both errors and commits
	})
	if err != nil {
		t.Fatalf("outer tx: %v", err)
	}
	if inner == nil || deleteErr == nil {
		t.Fatalf("expected capture errors, got %v / %v", inner, deleteErr)
	}
	if n := e.count(t, "items"); n != 1 {
		t.Fatalf("items=%d: the failed create leaked or the failed delete went through", n)
	}
	if n := e.count(t, "_sync_tombstones"); n != 0 {
		t.Fatalf("tombstones=%d", n)
	}
	if _, err := e.app.FindRecordById("items", keep.Id); err != nil {
		t.Fatalf("record deleted without a change row: %v", err)
	}
}

// S2b: after a rolled back savepoint the outer tx group state is consistent.
func TestSavepointRollbackKeepsTxGroupConsistent(t *testing.T) {
	e := setup(t)
	fail := true
	e.app.OnRecordCreateExecute("items").BindFunc(func(ev *core.RecordEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		if ev.Record.GetString("title") == "boom" && fail {
			return errors.New("boom")
		}
		return nil
	})
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		a := core.NewRecord(e.items)
		a.Set("title", "a")
		if err := tx.Save(a); err != nil {
			return err
		}
		b := core.NewRecord(e.items)
		b.Set("title", "boom")
		if tx.Save(b) == nil {
			t.Error("expected the failure")
		}
		c := core.NewRecord(e.items)
		c.Set("title", "c")
		return tx.Save(c)
	})
	if err != nil {
		t.Fatal(err)
	}
	ch := e.changes(t)
	if len(ch) != 2 || e.count(t, "items") != 2 {
		t.Fatalf("changes=%d items=%d", len(ch), e.count(t, "items"))
	}
	if ch[0].Tx == "" || ch[0].Tx != ch[1].Tx {
		t.Fatalf("the two surviving changes must share one group: %q %q", ch[0].Tx, ch[1].Tx)
	}
}

// S3: set fields are order and duplicate insensitive.
func TestSetFieldCanonical(t *testing.T) {
	if got := string(canonicalJSON(canonicalSet([]any{"b", "a", "b", "c"}))); got != `["a","b","c"]` {
		t.Fatalf("canonicalSet = %s", got)
	}
	e := setup(t)
	p := &policy{Types: map[string]string{"tags": TypeSet}}
	mk := func(tags ...string) []byte {
		r := core.NewRecord(e.items)
		r.Id = "aaaaaaaaaaaaaaa"
		r.Set("tags", tags)
		h, err := RecordHash(r, p)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if !bytes.Equal(mk("a", "b"), mk("b", "a")) {
		t.Fatal("hash depends on set order")
	}
	if bytes.Equal(mk("a", "b"), mk("a", "c")) {
		t.Fatal("hash ignores set content")
	}

	// a pure reorder through a real save emits no change row and no empty patch
	it := e.item(t, "title", "t", "tags", []string{"a", "b"})
	it.Set("tags", []string{"b", "a"})
	if err := e.app.Save(it); err != nil {
		t.Fatal(err)
	}
	if n := len(e.changes(t)); n != 1 {
		t.Fatalf("reorder produced %d change rows", n)
	}
	// and a real change is a clean add/rm delta
	it.Set("tags", []string{"b", "c"})
	if err := e.app.Save(it); err != nil {
		t.Fatal(err)
	}
	ch := e.changes(t)
	if len(ch) != 2 {
		t.Fatalf("rows=%d", len(ch))
	}
	jsonEq(t, patchOf(t, ch[1])["tags"], `{"$add":["c"],"$rm":["a"]}`)
}

// S4: integers above 2^53 inside JSON fields survive the patch and the hash.
func TestJSONBigIntegerPreserved(t *testing.T) {
	e := setup(t)
	raw := `{"id":9007199254740993,"nested":[18446744073709551615,1.0],"s":"<&>"}`
	it := e.item(t, "title", "big", "meta", types.JSONRaw(raw))
	ch := e.changes(t)
	if len(ch) != 1 {
		t.Fatalf("rows=%d", len(ch))
	}
	for _, want := range []string{`9007199254740993`, `18446744073709551615`, `1.0`, `"<&>"`} {
		if !bytes.Contains([]byte(ch[0].Patch), []byte(want)) {
			t.Fatalf("patch lost %s: %s", want, ch[0].Patch)
		}
	}
	got, err := e.app.FindRecordById("items", it.Id)
	if err != nil {
		t.Fatal(err)
	}
	h, err := RecordHash(got, &policy{Types: map[string]string{"qty": TypeCounter, "tags": TypeSet}, Exclude: map[string]struct{}{"note": {}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h, ch[0].Hash) {
		t.Fatal("hash of the stored record differs from the captured hash")
	}
	// a neighbouring integer must hash differently (no float64 rounding)
	got.Set("meta", types.JSONRaw(`{"id":9007199254740992,"nested":[18446744073709551615,1.0],"s":"<&>"}`))
	h2, _ := RecordHash(got, &policy{Types: map[string]string{"qty": TypeCounter, "tags": TypeSet}, Exclude: map[string]struct{}{"note": {}}})
	if bytes.Equal(h, h2) {
		t.Fatal("2^53+1 and 2^53 hash equal: numbers went through float64")
	}
}

// S5.1: a policy load failure without a good set refuses the write.
func TestPolicyLoadFailureRefusesWrite(t *testing.T) {
	e := setupWith(t, newApp(t), RoleHub)
	if _, err := e.app.DB().NewQuery("DROP TABLE _sync_policies").Execute(); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(e.items)
	r.Set("title", "x")
	if err := e.app.Save(r); err == nil {
		t.Fatal("write accepted without a policy set")
	}
	if n := e.count(t, "items"); n != 0 {
		t.Fatalf("items=%d", n)
	}
}

// S5.1b: a reload failure keeps serving the last good set (still captured).
func TestPolicyReloadFailureKeepsLastGood(t *testing.T) {
	e := setup(t)
	e.item(t, "title", "1")
	if _, err := e.app.DB().NewQuery("DROP TABLE _sync_policies").Execute(); err != nil {
		t.Fatal(err)
	}
	e.m.pol.invalidate()
	e.item(t, "title", "2")
	if n := len(e.changes(t)); n != 2 {
		t.Fatalf("changes=%d: write became uncaptured after a reload error", n)
	}
}

// S5.2: Init failing on an already bootstrapped app refuses writes.
func TestInitFailureAfterBootstrapRefusesWrites(t *testing.T) {
	app := newApp(t)
	// a foreign _changes table without the expected columns makes Init fail
	if _, err := app.DB().NewQuery("CREATE TABLE _changes (x INTEGER)").Execute(); err != nil {
		t.Fatal(err)
	}
	m := RegisterRole(app, RoleHub)
	if m.ready.Load() {
		t.Fatal("Init should have failed")
	}
	c := core.NewBaseCollection("things")
	c.Fields.Add(&core.TextField{Name: "title"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("title", "x")
	if err := app.Save(r); err == nil {
		t.Fatal("write accepted by a node that failed to initialize")
	}
}

// S5.3: unknown role values are an error.
func TestParseRoleStrict(t *testing.T) {
	for in, want := range map[string]Role{"": RoleOff, "OFF": RoleOff, " hub ": RoleHub, "spoke": RoleSpoke} {
		got, err := ParseRole(in)
		if err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	for _, in := range []string{"hubb", "spokes", "on", "1"} {
		if _, err := ParseRole(in); err == nil {
			t.Fatalf("%q must be an error", in)
		}
	}
	t.Setenv(EnvRole, "hubb")
	if m, err := RegisterFromEnv(newApp(t)); err == nil || m != nil {
		t.Fatalf("RegisterFromEnv: %v %v", m, err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Register must fail closed on an unknown role")
			}
		}()
		Register(newApp(t))
	}()
}

// S6: the boot floor includes the HLCs seen only through _sync_meta.
func TestBootIncludesSyncMetaHLC(t *testing.T) {
	e := setup(t)
	future := hlc.Make(time.Now().Add(2*time.Hour).UnixMilli(), 5)
	if _, err := e.app.DB().NewQuery("INSERT INTO _sync_meta (collection, record, hlc, node) VALUES ('c','r',{:h},'hub')").
		Bind(map[string]any{"h": int64(future)}).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Init(); err != nil {
		t.Fatal(err)
	}
	if got := e.m.Clock().Last(); got < future {
		t.Fatalf("boot clock %v is below the observed meta hlc %v", got, future)
	}
	if n := e.m.Clock().Now(); n <= future {
		t.Fatal("next local hlc must exceed every observed hlc")
	}
}

// S8: a derived/autodate-only save keeps _sync_meta.hash equal to the stored row.
func TestEmptyPatchRefreshesMetaHash(t *testing.T) {
	e := setup(t)
	it := e.item(t, "title", "a")
	time.Sleep(5 * time.Millisecond)
	cur, _ := e.app.FindRecordById("items", it.Id)
	if err := e.app.Save(cur); err != nil { // nothing changes but `updated`
		t.Fatal(err)
	}
	if n := len(e.changes(t)); n != 1 {
		t.Fatalf("an empty patch must not write a change row, rows=%d", n)
	}
	stored, _ := e.app.FindRecordById("items", it.Id)
	want, err := RecordHash(stored, &policy{Types: map[string]string{"qty": TypeCounter, "tags": TypeSet}, Exclude: map[string]struct{}{"note": {}}})
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := e.app.DB().NewQuery("SELECT hash FROM _sync_meta WHERE record={:r}").Bind(map[string]any{"r": it.Id}).Row(&got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("_sync_meta.hash is stale after an autodate-only update")
	}
	var h int64
	_ = e.app.DB().NewQuery("SELECT hlc FROM _sync_meta WHERE record={:r}").Bind(map[string]any{"r": it.Id}).Row(&h)
	if h != e.changes(t)[0].HLC {
		t.Fatal("the record clock must not move")
	}
}

// Missing tests: rolled back delete, delete of a record without meta.
func TestDeleteRollbackAndMissingMeta(t *testing.T) {
	e := setup(t)
	it := e.item(t, "title", "d")
	err := e.app.RunInTransaction(func(tx kernel.App) error {
		r, _ := tx.FindRecordById("items", it.Id)
		if err := tx.Delete(r); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if e.count(t, "_sync_tombstones") != 0 || e.count(t, "items") != 1 || e.count(t, "_sync_meta") != 1 || len(e.changes(t)) != 1 {
		t.Fatal("a rolled back delete left state behind")
	}

	if _, err := e.app.DB().NewQuery("DELETE FROM _sync_meta").Execute(); err != nil {
		t.Fatal(err)
	}
	cur, _ := e.app.FindRecordById("items", it.Id)
	if err := e.app.Delete(cur); err != nil {
		t.Fatal(err)
	}
	ch := e.changes(t)
	if len(ch) != 2 || ch[1].Op != OpDelete || ch[1].BaseHLC != 0 || e.count(t, "_sync_tombstones") != 1 {
		t.Fatalf("delete without meta: %+v", ch)
	}
}

// Cascade delete and a SetNull cascade update are captured in one group.
func TestCascadeDeleteCaptured(t *testing.T) {
	e := setup(t)
	kids := core.NewBaseCollection("kids")
	kids.Fields.Add(&core.TextField{Name: "name"},
		&core.RelationField{Name: "item", CollectionId: e.items.Id, MaxSelect: 1, CascadeDelete: true})
	if err := e.app.Save(kids); err != nil {
		t.Fatal(err)
	}
	e.policy(t, "kids", DirBoth, nil, nil)
	parent := e.item(t, "title", "p")
	k := core.NewRecord(kids)
	k.Set("name", "k")
	k.Set("item", parent.Id)
	if err := e.app.Save(k); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Delete(parent); err != nil {
		t.Fatal(err)
	}
	ch := e.changes(t)
	if len(ch) != 4 {
		t.Fatalf("rows=%d", len(ch))
	}
	d1, d2 := ch[2], ch[3]
	if d1.Op != OpDelete || d2.Op != OpDelete || d1.Tx == "" || d1.Tx != d2.Tx {
		t.Fatalf("cascade must be two deletes in one group: %+v %+v", d1, d2)
	}
	if e.count(t, "_sync_tombstones") != 2 {
		t.Fatal("tombstones for parent and child expected")
	}
}
