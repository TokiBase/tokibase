//go:build !no_crypto

package crypto

import (
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

func patients(t *testing.T, e *env) *core.Collection {
	t.Helper()
	c, err := e.app.FindCollectionByNameOrId("patients")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// K2: the registry that audit/webhooks/mcp consult follows the configuration.
func TestK2SensitiveRegistryFollowsConfig(t *testing.T) {
	e := setup(t, "")
	col := patients(t, e)
	if kernel.IsSensitive(col.Id, "diagnosis") {
		t.Fatal("registered before enable")
	}
	e.enableAll(t)
	for _, f := range []string{"diagnosis", "ssn", "email", "notes"} {
		if !kernel.IsSensitive(col.Id, f) {
			t.Fatalf("%s not registered", f)
		}
	}
	if kernel.IsSensitive(col.Id, "name") {
		t.Fatal("plain field registered")
	}
	if _, err := Disable(e.app, "patients", "diagnosis", nil); err != nil {
		t.Fatal(err)
	}
	_, _ = e.m.fieldsFor(col.Id) // the cache reloads (and the registry syncs) on the next read
	if kernel.IsSensitive(col.Id, "diagnosis") {
		t.Fatal("still registered after disable")
	}
}

// K1: rename, retype and delete of an encrypted field are refused.
func TestK1SchemaChangesRefused(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)

	col := patients(t, e)
	col.Fields.GetByName("diagnosis").SetName("dx")
	if err := e.app.Save(col); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Fatalf("rename must be refused: %v", err)
	}

	col = patients(t, e)
	col.Fields.RemoveByName("diagnosis")
	if err := e.app.Save(col); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("delete must be refused: %v", err)
	}

	col = patients(t, e)
	old := col.Fields.GetByName("notes")
	col.Fields.RemoveById(old.GetId())
	nf := &core.TextField{Name: "notes", Id: old.GetId()}
	col.Fields.Add(nf)
	if err := e.app.Save(col); err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("retype must be refused: %v", err)
	}

	// unrelated changes still work, including renaming the collection
	col = patients(t, e)
	col.Fields.GetByName("name").SetName("full_name")
	col.Fields.Add(&core.NumberField{Name: "age"})
	col.Name = "patients2"
	if err := e.app.Save(col); err != nil {
		t.Fatalf("unrelated change: %v", err)
	}
	e.m.Invalidate()
	_, b := e.do(t, e.su, "GET", "/api/collections/patients2/records/"+id, "")
	if b["diagnosis"] != "flu" {
		t.Fatalf("collection rename must keep decrypting: %v", b)
	}

	// after disable the field is free to change
	if _, err := Disable(e.app, "patients2", "diagnosis", nil); err != nil {
		t.Fatal(err)
	}
	col, _ = e.app.FindCollectionByNameOrId("patients2")
	col.Fields.GetByName("diagnosis").SetName("dx")
	if err := e.app.Save(col); err != nil {
		t.Fatalf("rename after disable: %v", err)
	}
}

// K3: POST body, hidden fields and fieldperm-style hiding.
func TestK3LookupPostAndHiddenField(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.create(t)

	code, b := e.do(t, nil, "POST", "/api/crypto/lookup/patients/ssn", `{"value":"123-45"}`)
	if code != 200 || len(b["items"].([]any)) != 1 {
		t.Fatalf("POST lookup: %d %v", code, b)
	}
	if code, _ := e.do(t, nil, "POST", "/api/crypto/lookup/patients/ssn", `not json`); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	if code, _ := e.do(t, nil, "POST", "/api/crypto/lookup/patients/ssn", `{}`); code != 400 {
		t.Fatalf("missing value: %d", code)
	}

	col := patients(t, e)
	col.Fields.GetByName("ssn").SetHidden(true)
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		url := "/api/crypto/lookup/patients/ssn"
		body := `{"value":"123-45"}`
		if method == "GET" {
			url += "?value=123-45"
			body = ""
		}
		_, b = e.do(t, nil, method, url, body)
		if n := len(b["items"].([]any)); n != 0 {
			t.Fatalf("%s: anonymous caller got %d records through a hidden field", method, n)
		}
		_, b = e.do(t, e.su, method, url, body)
		if n := len(b["items"].([]any)); n != 1 {
			t.Fatalf("%s: superuser must still find the record, got %d", method, n)
		}
	}

	// a field hidden by a hook at enrich time (what fieldperm does) behaves the same
	col.Fields.GetByName("ssn").SetHidden(false)
	_ = e.app.Save(col)
	e.app.OnRecordEnrich().BindFunc(func(ev *core.RecordEnrichEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		if ev.RequestInfo != nil && !ev.RequestInfo.HasSuperuserAuth() {
			ev.Record.Hide("ssn")
		}
		return nil
	})
	_, b = e.do(t, nil, "POST", "/api/crypto/lookup/patients/ssn", `{"value":"123-45"}`)
	if n := len(b["items"].([]any)); n != 0 {
		t.Fatalf("enrich-hidden field leaked an equality match: %d", n)
	}
}

// K4: disable keeps the config until a final sweep finds no ciphertext.
func TestK4DisableIsCrashSafe(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id1, id2 := e.create(t), e.create(t)

	// simulate a crash right after the state switch
	rec, _ := e.m.configRecord(patients(t, e).Id, "diagnosis")
	rec.Set("state", StateDisabling)
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
	st, err := Status(e.app)
	if err != nil || len(st.Pending) != 1 || st.Pending[0].State != StateDisabling || st.Pending[0].Field != "diagnosis" {
		t.Fatalf("status must show the pending state: %+v %v", st, err)
	}
	// a write while disabling stores plaintext; reads of old ciphertext still decrypt
	code, _ := e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id2, `{"diagnosis":"cold"}`)
	if code != 200 || e.raw(t, id2, "diagnosis") != "cold" {
		t.Fatalf("write in disabling state: %d raw=%q", code, e.raw(t, id2, "diagnosis"))
	}
	_, b := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id1, "")
	if b["diagnosis"] != "flu" {
		t.Fatalf("read during disabling: %v", b["diagnosis"])
	}
	done, err := Resume(e.app, nil)
	if err != nil || len(done) != 1 {
		t.Fatalf("resume: %v %v", done, err)
	}
	if e.raw(t, id1, "diagnosis") != "flu" {
		t.Fatalf("not decrypted: %q", e.raw(t, id1, "diagnosis"))
	}
	if r, _ := e.m.configRecord(patients(t, e).Id, "diagnosis"); r != nil {
		t.Fatal("config must be gone after the final sweep")
	}
	if st, _ := Status(e.app); len(st.Pending) != 0 {
		t.Fatalf("pending after resume: %+v", st.Pending)
	}
}

func TestK4DisableFailureKeepsConfig(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	e.create(t)
	raw := []byte(e.raw(t, id, "diagnosis"))
	raw[len(raw)-3] ^= 1
	e.app.DB().NewQuery("UPDATE patients SET diagnosis={:d} WHERE id={:id}").Bind(map[string]any{"d": string(raw), "id": id}).Execute()

	if _, err := Disable(e.app, "patients", "diagnosis", nil); err == nil {
		t.Fatal("a tampered row must abort the disable")
	}
	r, _ := e.m.configRecord(patients(t, e).Id, "diagnosis")
	if r == nil || r.GetString("state") != StateDisabling {
		t.Fatalf("config must survive in state disabling: %v", r)
	}
	// the lock was released: the operator can retry once the row is fixed
	e.app.DB().NewQuery("DELETE FROM patients WHERE id={:id}").Bind(map[string]any{"id": id}).Execute()
	if _, err := Disable(e.app, "patients", "diagnosis", nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r, _ := e.m.configRecord(patients(t, e).Id, "diagnosis"); r != nil {
		t.Fatal("config must be removed after a clean sweep")
	}
}

// K5: auth identity fields, unique indexes, indexed fields and views are refused.
func TestK5EligibilityIdentityIndexesViews(t *testing.T) {
	e := setup(t, "")
	m := core.NewAuthCollection("members")
	m.Fields.Add(&core.TextField{Name: "nick"}, &core.TextField{Name: "bio"})
	m.AddIndex("idx_nick", true, "nick", "")
	m.PasswordAuth.IdentityFields = []string{"email", "nick"}
	if err := e.app.Save(m); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(e.app, "members", "nick", ModeRandom, nil); err == nil {
		t.Fatal("identity field must be refused")
	}
	if _, err := Enable(e.app, "members", "bio", ModeRandom, nil); err != nil {
		t.Fatalf("plain auth field: %v", err)
	}

	col := patients(t, e)
	col.AddIndex("idx_u_name", true, "name", "")
	col.AddIndex("idx_dx", false, "diagnosis", "")
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(e.app, "patients", "name", ModeRandom, nil); err == nil || !strings.Contains(err.Error(), "unique index") {
		t.Fatalf("unique index: %v", err)
	}
	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err == nil || !strings.Contains(err.Error(), "index") {
		t.Fatalf("indexed field: %v", err)
	}

	v := core.NewViewCollection("patient_view")
	v.ViewQuery = "SELECT id, ssn FROM patients"
	if err := e.app.Save(v); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(e.app, "patients", "ssn", ModeRandom, nil); err == nil || !strings.Contains(err.Error(), "view") {
		t.Fatalf("view must block enabling: %v", err)
	}
}

// K8: views over encrypted columns are refused at save time; expanded relations decrypt.
func TestK8ViewsRefusedAndExpandDecrypts(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)

	for i, q := range []string{"SELECT id, diagnosis FROM patients", "select p.id, p.ssn as s from patients p", "SELECT id, json_extract(notes, '$.a') AS n FROM patients"} {
		v := core.NewViewCollection("vw" + string(rune('a'+i)))
		v.ViewQuery = q
		if err := e.app.Save(v); err == nil || !strings.Contains(err.Error(), "encrypted") {
			t.Fatalf("view %q must be refused: %v", q, err)
		}
	}
	ok := core.NewViewCollection("patient_names")
	ok.ViewQuery = "SELECT id, name FROM patients"
	if err := e.app.Save(ok); err != nil {
		t.Fatalf("view over plain columns: %v", err)
	}

	pat := patients(t, e)
	visits := core.NewBaseCollection("visits")
	visits.Fields.Add(&core.RelationField{Name: "patient", CollectionId: pat.Id, MaxSelect: 1})
	open := ""
	visits.ListRule, visits.ViewRule = &open, &open
	if err := e.app.Save(visits); err != nil {
		t.Fatal(err)
	}
	vr := core.NewRecord(visits)
	vr.Set("patient", id)
	if err := e.app.Save(vr); err != nil {
		t.Fatal(err)
	}
	_, b := e.do(t, e.su, "GET", "/api/collections/visits/records/"+vr.Id+"?expand=patient", "")
	ex, _ := b["expand"].(map[string]any)
	p, _ := ex["patient"].(map[string]any)
	if p["diagnosis"] != "flu" || p["ssn"] != "123-45" {
		t.Fatalf("expanded relation must be decrypted: %v", b)
	}
	// the Go helper decrypts the expand tree too
	rec, _ := e.app.FindRecordById("visits", vr.Id)
	e.app.ExpandRecord(rec, []string{"patient"}, nil)
	if err := Decrypt(e.app, rec); err != nil {
		t.Fatal(err)
	}
	if got := rec.ExpandedOne("patient").GetString("diagnosis"); got != "flu" {
		t.Fatalf("Decrypt must walk the expand tree: %q", got)
	}
}

// K6: a failed decrypt shows a sentinel that, sent back, never erases ciphertext.
func TestK6SentinelRoundTripKeepsCiphertext(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	rawDx, rawNotes := e.raw(t, id, "diagnosis"), e.raw(t, id, "notes")

	oldMaster := e.m.master
	e.m.master = nil // simulates an unreadable key: every decrypt fails, writes of encrypted fields are refused
	_, b := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != Undecryptable || b["notes"] != Undecryptable {
		t.Fatalf("expected the sentinel: %v %v", b["diagnosis"], b["notes"])
	}
	e.m.master = oldMaster
	e.m.Invalidate()

	// the whole record, including the sentinels, is sent back
	body := `{"name":"Renamed","diagnosis":"` + Undecryptable + `","notes":"` + Undecryptable + `"}`
	code, out := e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, body)
	if code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	if e.raw(t, id, "diagnosis") != rawDx || e.raw(t, id, "notes") != rawNotes {
		t.Fatalf("stored ciphertext was changed by a sentinel round trip")
	}
	_, b = e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "flu" || b["name"] != "Renamed" {
		t.Fatalf("value must be readable again: %v", b)
	}
}

// K9: operation lock, interrupted rotate, retire scans unconfigured columns.
func TestK9LockResumeRotateAndRetire(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	col := patients(t, e)

	unlock, err := e.m.acquireLock(col.Id, "rotate", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Rotate(e.app, "patients", nil); err == nil || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("concurrent rotate must be refused: %v", err)
	}
	if _, err := Retire(e.app, "patients"); err == nil {
		t.Fatal("retire must be refused while locked")
	}
	if _, err := Disable(e.app, "patients", "diagnosis", nil); err == nil {
		t.Fatal("disable must be refused while locked")
	}
	unlock()

	// an interrupted rotate: new key exists, rows still on v1, lock left behind
	if _, err := e.m.acquireLock(col.Id, "rotate", false); err != nil {
		t.Fatal(err)
	}
	ver, err := e.m.newKey(col.Id)
	if err != nil || ver != 2 {
		t.Fatalf("newKey %d %v", ver, err)
	}
	st, _ := Status(e.app)
	if len(st.Pending) != 1 || st.Pending[0].State != "locked:rotate" {
		t.Fatalf("status pending: %+v", st.Pending)
	}
	if !strings.HasPrefix(e.raw(t, id, "diagnosis"), "tkc1:1:") {
		t.Fatal("precondition: row on v1")
	}
	done, err := Resume(e.app, nil)
	if err != nil || len(done) != 1 {
		t.Fatalf("resume: %v %v", done, err)
	}
	if !strings.HasPrefix(e.raw(t, id, "diagnosis"), "tkc1:2:") {
		t.Fatalf("resume must re-encrypt to v2: %q", e.raw(t, id, "diagnosis")[:10])
	}
	if keys, _ := e.m.KeyInfos(col.Id); len(keys) != 2 {
		t.Fatalf("resume must not create another key: %v", keys)
	}

	// a ciphertext of v1 hidden in a column that has no config blocks retire
	c2 := patients(t, e)
	c2.Fields.Add(&core.TextField{Name: "stray"})
	if err := e.app.Save(c2); err != nil {
		t.Fatal(err)
	}
	e.app.DB().NewQuery("UPDATE patients SET stray={:d} WHERE id={:id}").Bind(map[string]any{"d": "tkc1:1:AAAA", "id": id}).Execute()
	res, err := Retire(e.app, "patients")
	if err != nil || len(res.Retired) != 0 || len(res.InUse) != 1 {
		t.Fatalf("retire must see the unconfigured column: %+v %v", res, err)
	}
	e.app.DB().NewQuery("UPDATE patients SET stray='' WHERE id={:id}").Bind(map[string]any{"id": id}).Execute()
	if res, err = Retire(e.app, "patients"); err != nil || len(res.Retired) != 1 {
		t.Fatalf("retire: %+v %v", res, err)
	}

	// cool-down: a fresh key blocks retire
	RetireCooldown = cacheTTL * 2
	t.Cleanup(func() { RetireCooldown = 0 })
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Retire(e.app, "patients"); err == nil || !strings.Contains(err.Error(), "wait") {
		t.Fatalf("cool-down: %v", err)
	}
}
