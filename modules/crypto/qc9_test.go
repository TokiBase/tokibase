//go:build !no_crypto

package crypto

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// QC of PR9 (review P9-2, P9-4, P9-8, P9-11): the sweeps report to the sync
// module, the guards of a spoke or of a process without sync, partial key export.

type fakeSweeper struct {
	role     string
	synced   bool
	calls    []map[string][]string
	blockers []string
	failWith error
}

func (f *fakeSweeper) SyncRole() string                               { return f.role }
func (f *fakeSweeper) IsSynced(string) bool                           { return f.synced }
func (f *fakeSweeper) RetireBlockers(string, []int) ([]string, error) { return f.blockers, nil }
func (f *fakeSweeper) RecordSweep(_ kernel.App, _ string, changed map[string][]string) error {
	if f.failWith != nil {
		return f.failWith
	}
	cp := map[string][]string{}
	for k, v := range changed {
		cp[k] = append([]string(nil), v...)
	}
	f.calls = append(f.calls, cp)
	return nil
}

func withSweeper(t *testing.T, e *env, f *fakeSweeper) {
	t.Helper()
	kernel.SetSyncSweeper(e.app, f)
	t.Cleanup(func() { kernel.SetSyncSweeper(e.app, nil) })
}

func swept(f *fakeSweeper) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, c := range f.calls {
		for id, fields := range c {
			if out[id] == nil {
				out[id] = map[string]bool{}
			}
			for _, fl := range fields {
				out[id][fl] = true
			}
		}
	}
	return out
}

func TestQC9SweepsReportEveryRewrittenRecord(t *testing.T) {
	e := setup(t, "")
	f := &fakeSweeper{role: "hub", synced: true}
	withSweeper(t, e, f)
	ids := []string{e.create(t), e.create(t), e.create(t)}

	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	got := swept(f)
	for _, id := range ids {
		if !got[id]["diagnosis"] {
			t.Fatalf("enable: record %s was not reported: %v", id, got)
		}
	}
	f.calls = nil
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	got = swept(f)
	for _, id := range ids {
		if !got[id]["diagnosis"] {
			t.Fatalf("rotate: record %s was not reported", id)
		}
	}
	f.calls = nil
	if _, err := Disable(e.app, "patients", "diagnosis", nil); err != nil {
		t.Fatal(err)
	}
	got = swept(f)
	for _, id := range ids {
		if !got[id]["diagnosis"] {
			t.Fatalf("disable: record %s was not reported", id)
		}
	}
}

func TestQC9ASweepThatCannotBeRecordedIsRolledBack(t *testing.T) {
	e := setup(t, "")
	f := &fakeSweeper{role: "hub", synced: true, failWith: errors.New("change log full")}
	withSweeper(t, e, f)
	id := e.create(t)
	before := e.raw(t, id, "diagnosis")
	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err == nil {
		t.Fatal("enable must fail when the sweep cannot be recorded")
	}
	if got := e.raw(t, id, "diagnosis"); got != before {
		t.Fatalf("the values must be untouched: %q", got)
	}
}

func TestQC9SpokeAndProcessWithoutSyncRefuse(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	// a spoke
	withSweeper(t, e, &fakeSweeper{role: "spoke", synced: true})
	const want = "sync role spoke"
	if _, _, err := Rotate(e.app, "patients", nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := Retire(e.app, "patients"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("retire: %v", err)
	}
	if _, err := Enable(e.app, "patients", "name", ModeRandom, nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("enable: %v", err)
	}
	if _, err := Disable(e.app, "patients", "diagnosis", nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("disable: %v", err)
	}
	// status and verify stay available
	if _, err := Status(e.app); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(e.app, "patients", 10); err != nil {
		t.Fatal(err)
	}

	// a process without the sync module whose database has a policy for the collection
	kernel.SetSyncSweeper(e.app, nil)
	pc := core.NewBaseCollection("_sync_policies")
	pc.System = true
	pc.Fields.Add(&core.TextField{Name: "collection"}, &core.TextField{Name: "direction"}, &core.BoolField{Name: "enabled"})
	if err := e.app.Save(pc); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(pc)
	r.Set("collection", "patients")
	r.Set("direction", "both")
	r.Set("enabled", true)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Rotate(e.app, "patients", nil); err == nil || !strings.Contains(err.Error(), "TOKI_SYNC_ROLE=hub") {
		t.Fatalf("a rotate that cannot be recorded must be refused: %v", err)
	}
	r.Set("enabled", false)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatalf("a disabled policy does not matter: %v", err)
	}
}

func TestQC9RetireAsksTheSyncHub(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	f := &fakeSweeper{role: "hub", synced: true, blockers: []string{"nabc (gate-1): has not fetched the new key"}}
	withSweeper(t, e, f)
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	res, err := Retire(e.app, "patients")
	if err != nil || len(res.Retired) != 0 || len(res.Blocked) != 1 {
		t.Fatalf("a blocking node must stop the retire: %+v %v", res, err)
	}
	res, err = RetireForce(e.app, "patients", true)
	if err != nil || len(res.Retired) != 1 || len(res.Blocked) != 0 {
		t.Fatalf("force: %+v %v", res, err)
	}
}

func TestQC9ExportKeysSkipsACollectionThatDoesNotUnwrap(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	c := core.NewBaseCollection("docs")
	c.Fields.Add(&core.TextField{Name: "body"})
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(e.app, "docs", "body", ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	docs, _ := e.app.FindCollectionByNameOrId("docs")
	pat := patients(t, e)
	if _, err := e.app.NonconcurrentDB().NewQuery("UPDATE _crypto_keys SET wrapped_dek='garbage' WHERE collection={:c}").Bind(map[string]any{"c": docs.Id}).Execute(); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
	node := x25519(t)
	keys, failed, err := kernel.SyncKeyProviderOf(e.app).ExportKeys([]string{pat.Id, docs.Id}, node.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("a bad collection must not fail the call: %v", err)
	}
	if len(failed) != 1 || failed[0].Collection != docs.Id {
		t.Fatalf("failed = %+v", failed)
	}
	if len(keys) != 1 || keys[0].Collection != pat.Id {
		t.Fatalf("keys = %+v", keys)
	}
	// nothing at all can be exported without a master key
	nokey := setup(t, "-")
	if _, _, err := kernel.SyncKeyProviderOf(nokey.app).ExportKeys([]string{pat.Id}, node.PublicKey().Bytes()); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("no master key: %v", err)
	}
}

func TestQC9ValidationFailureForgetsTheRememberedCiphertext(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	ct := e.raw(t, id, "diagnosis")
	origin := &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: "nspoke"}
	ctx := kernel.WithSyncOrigin(context.Background(), origin)
	rec, _ := e.app.FindRecordById("patients", id)
	rec.Set("diagnosis", ct)
	rec.Set("email", "not-an-email") // fails validation after onValidate remembered the ciphertext
	if err := e.app.SaveWithContext(ctx, rec); err == nil {
		t.Fatal("the save must fail validation")
	}
	if _, ok := origin.Recall(syncCTKey{rec, "diagnosis"}); ok {
		t.Fatal("a failed save must not keep the record pinned in the origin")
	}
}

// A ciphertext under a retired version is refused on a push with the typed error.
func TestQC9PushUnderARetiredVersionIsTyped(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	oldCT := e.raw(t, id, "diagnosis")
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	if res, err := Retire(e.app, "patients"); err != nil || len(res.Retired) != 1 {
		t.Fatalf("retire: %+v %v", res, err)
	}
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: "nspoke"})
	rec, _ := e.app.FindRecordById("patients", id)
	rec.Set("diagnosis", oldCT)
	rec.Set("name", "changed")
	err := e.app.SaveNoValidateWithContext(ctx, rec)
	if !errors.Is(err, kernel.ErrSyncKeyRetired) {
		t.Fatalf("want ErrSyncKeyRetired, got %v", err)
	}
}

// A validator that changes the plaintext of a synced write: the stored value is a
// fresh ciphertext of the changed plaintext, not the remembered one.
func TestQC9ValidatorThatChangesThePlaintextIsReEncrypted(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	ct := e.raw(t, id, "diagnosis")
	e.app.OnRecordValidate("patients").Bind(&hook.Handler[*core.RecordEvent]{
		Id: "qc9val", Priority: 10, Func: func(ev *core.RecordEvent) error {
			if kernel.SyncOriginFrom(ev.Context) != nil {
				ev.Record.Set("diagnosis", "normalised")
			}
			return ev.Next()
		},
	})
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush, Node: "nspoke"})
	rec, _ := e.app.FindRecordById("patients", id)
	rec.Set("diagnosis", ct)
	rec.Set("name", "x")
	if err := e.app.SaveWithContext(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got := e.raw(t, id, "diagnosis")
	if got == ct || !strings.HasPrefix(got, Prefix) {
		t.Fatalf("a changed plaintext needs a new ciphertext, got %q", got)
	}
	fresh, _ := e.app.FindRecordById("patients", id)
	if err := Decrypt(e.app, fresh); err != nil || fresh.GetString("diagnosis") != "normalised" {
		t.Fatalf("decrypts to %q (%v)", fresh.GetString("diagnosis"), err)
	}
}
