package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app *tests.TestApp
	m   *Module
	mux http.Handler
	su  *core.Record
	usr *core.Record
}

func newKey(t *testing.T) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
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

// setup registers the module (with a master key unless key == "-") and a
// "patients" collection: name (plain), diagnosis (random), ssn (blind-index),
// email (random, email type), notes (json, random).
func setup(t *testing.T, key string) *env {
	t.Helper()
	WaitForServers = false
	if key == "" {
		key = newKey(t)
	}
	if key == "-" {
		t.Setenv(EnvMasterKey, "")
		t.Setenv(EnvMasterKeyFile, "")
	} else {
		t.Setenv(EnvMasterKey, key)
	}
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)
	if err := EnsureSchema(app); err != nil {
		t.Fatal(err)
	}

	c := core.NewBaseCollection("patients")
	c.Fields.Add(
		&core.TextField{Name: "name"},
		&core.TextField{Name: "diagnosis"},
		&core.TextField{Name: "ssn"},
		&core.EmailField{Name: "email"},
		&core.JSONField{Name: "notes"},
	)
	open := ""
	c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	e := &env{app: app, m: m}
	e.su, _ = app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	e.usr, _ = app.FindAuthRecordByEmail("users", "test@example.com")
	e.mux = buildMux(t, app)
	return e
}

func (e *env) enableAll(t *testing.T) {
	t.Helper()
	for f, mode := range map[string]string{"diagnosis": ModeRandom, "ssn": ModeBlindIndex, "email": ModeRandom, "notes": ModeRandom} {
		if _, err := Enable(e.app, "patients", f, mode, nil); err != nil {
			t.Fatalf("enable %s: %v", f, err)
		}
	}
}

func (e *env) do(t *testing.T, who *core.Record, method, url, body string) (int, map[string]any) {
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
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func (e *env) raw(t *testing.T, id, field string) string {
	t.Helper()
	var s *string
	if err := e.app.DB().NewQuery("SELECT [[" + field + "]] FROM patients WHERE id={:id}").Bind(map[string]any{"id": id}).Row(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return ""
	}
	return *s
}

const createBody = `{"name":"Ann","diagnosis":"flu","ssn":"123-45","email":"ann@example.com","notes":{"a":[1,2]}}`

func (e *env) create(t *testing.T) string {
	t.Helper()
	code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", createBody)
	if code != 200 {
		t.Fatal(code, b)
	}
	return b["id"].(string)
}

func TestRoundTripAPI(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", createBody)
	if code != 200 {
		t.Fatal(code, b)
	}
	id := b["id"].(string)
	if b["diagnosis"] != "flu" || b["ssn"] != "123-45" || b["email"] != "ann@example.com" || b["name"] != "Ann" {
		t.Fatalf("create response must be plaintext: %v", b)
	}
	for _, f := range []string{"diagnosis", "ssn", "email"} {
		if raw := e.raw(t, id, f); !strings.HasPrefix(raw, Prefix+"1:") || strings.Contains(raw, "flu") {
			t.Fatalf("%s not ciphertext in DB: %q", f, raw)
		}
	}
	if raw := e.raw(t, id, "notes"); !strings.HasPrefix(raw, `"`+Prefix) {
		t.Fatalf("json not ciphertext: %q", raw)
	}
	if e.raw(t, id, "name") != "Ann" {
		t.Fatal("plain field changed")
	}
	_, b = e.do(t, nil, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "flu" || b["email"] != "ann@example.com" {
		t.Fatalf("view: %v", b)
	}
	if n, _ := json.Marshal(b["notes"]); string(n) != `{"a":[1,2]}` {
		t.Fatalf("json notes: %s", n)
	}
	_, l := e.do(t, nil, "GET", "/api/collections/patients/records", "")
	if it := l["items"].([]any)[0].(map[string]any); it["diagnosis"] != "flu" {
		t.Fatalf("list: %v", l)
	}

	// update an unrelated field: the ciphertext (and so the nonce) is untouched,
	// even for an email field (validation runs on the plaintext)
	before := e.raw(t, id, "diagnosis")
	if code, b := e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"name":"Anna"}`); code != 200 {
		t.Fatal(code, b)
	}
	if e.raw(t, id, "diagnosis") != before {
		t.Fatal("unchanged field was re-encrypted")
	}
	// changing the value re-encrypts
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"diagnosis":"cold"}`)
	if e.raw(t, id, "diagnosis") == before {
		t.Fatal("changed field kept old ciphertext")
	}
	_, b = e.do(t, nil, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "cold" || b["name"] != "Anna" {
		t.Fatalf("after update: %v", b)
	}
	// the same plaintext written twice gives different ciphertext
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"diagnosis":"flu"}`)
	a := e.raw(t, id, "diagnosis")
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"diagnosis":"x"}`)
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"diagnosis":"flu"}`)
	if a == e.raw(t, id, "diagnosis") {
		t.Fatal("deterministic ciphertext")
	}
	// ciphertext is bound to the record: swapping values between rows must not decrypt
	id2 := e.create(t)
	e.app.DB().NewQuery("UPDATE patients SET diagnosis={:d} WHERE id={:id}").Bind(map[string]any{"d": e.raw(t, id, "diagnosis"), "id": id2}).Execute()
	_, b = e.do(t, nil, "GET", "/api/collections/patients/records/"+id2, "")
	if b["diagnosis"] != "" {
		t.Fatalf("ciphertext moved to another record decrypted: %v", b["diagnosis"])
	}
}

func TestEnableBackfillAndDisable(t *testing.T) {
	e := setup(t, "")
	col, _ := e.app.FindCollectionByNameOrId("patients")
	var ids []string
	for i := 0; i < 7; i++ {
		r := core.NewRecord(col)
		r.Set("name", "n")
		r.Set("diagnosis", "d"+string(rune('a'+i)))
		r.Set("ssn", "s"+string(rune('a'+i)))
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Id)
	}
	if n, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err != nil || n != 7 {
		t.Fatalf("enable: %d %v", n, err)
	}
	if _, err := Enable(e.app, "patients", "ssn", ModeBlindIndex, nil); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		if !IsCiphertext(e.raw(t, id, "diagnosis")) {
			t.Fatal("row not encrypted")
		}
		_, b := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
		if b["diagnosis"] != "d"+string(rune('a'+i)) {
			t.Fatalf("read: %v", b)
		}
	}
	recs, err := FindByBlindIndex(e.app, "patients", "ssn", "sc")
	if err != nil || len(recs) != 1 || recs[0].Id != ids[2] {
		t.Fatalf("lookup backfilled row: %v %v", recs, err)
	}
	// resume is idempotent
	if n, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err != nil || n != 0 {
		t.Fatalf("resume: %d %v", n, err)
	}
	if _, err := Enable(e.app, "patients", "diagnosis", ModeBlindIndex, nil); err == nil {
		t.Fatal("mode change must be refused")
	}
	if rep, err := Verify(e.app, "patients", 100); err != nil || len(rep.Failures) != 0 || rep.Decrypted != 14 {
		t.Fatalf("verify: %+v %v", rep, err)
	}

	if _, err := Disable(e.app, "patients", "diagnosis", nil); err != nil {
		t.Fatal(err)
	}
	if got := e.raw(t, ids[0], "diagnosis"); got != "da" {
		t.Fatalf("disable: %q", got)
	}
	if _, err := Disable(e.app, "patients", "ssn", nil); err != nil {
		t.Fatal(err)
	}
	var n int
	e.app.DB().NewQuery("SELECT COUNT(*) FROM " + IndexTable).Row(&n)
	if n != 0 {
		t.Fatalf("index rows left: %d", n)
	}
	if _, err := Disable(e.app, "patients", "ssn", nil); err == nil {
		t.Fatal("disable of a plain field must fail")
	}
}

func TestBlindIndexLookup(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	id2 := e.create(t)

	recs, err := FindByBlindIndex(e.app, "patients", "ssn", "123-45")
	if err != nil || len(recs) != 2 {
		t.Fatalf("got %d %v", len(recs), err)
	}
	if !IsCiphertext(recs[0].GetString("ssn")) {
		t.Fatal("Go API returns records as stored")
	}
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "nope"); len(recs) != 0 {
		t.Fatal("false positive")
	}
	if _, err := FindByBlindIndex(e.app, "patients", "diagnosis", "flu"); err == nil {
		t.Fatal("random field has no index")
	}

	code, b := e.do(t, nil, "GET", "/api/crypto/lookup/patients/ssn?value=123-45", "")
	if code != 200 || len(b["items"].([]any)) != 2 {
		t.Fatal(code, b)
	}
	if it := b["items"].([]any)[0].(map[string]any); it["ssn"] != "123-45" || it["diagnosis"] != "flu" {
		t.Fatalf("lookup must return decrypted record: %v", it)
	}

	// update moves the index entry, delete removes it
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"ssn":"999"}`)
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "123-45"); len(recs) != 1 || recs[0].Id != id2 {
		t.Fatalf("after update: %v", recs)
	}
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "999"); len(recs) != 1 || recs[0].Id != id {
		t.Fatalf("new value: %v", recs)
	}
	e.do(t, e.su, "DELETE", "/api/collections/patients/records/"+id, "")
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "999"); len(recs) != 0 {
		t.Fatal("deleted record still found")
	}
	var n int
	e.app.DB().NewQuery("SELECT COUNT(*) FROM " + IndexTable + " WHERE record={:r}").Bind(map[string]any{"r": id}).Row(&n)
	if n != 0 {
		t.Fatal("index rows of a deleted record remain")
	}

	// the list rule is honored by the endpoint
	col, _ := e.app.FindCollectionByNameOrId("patients")
	rule := "name = 'nobody'"
	col.ListRule = &rule
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	_, b = e.do(t, nil, "GET", "/api/crypto/lookup/patients/ssn?value=123-45", "")
	if len(b["items"].([]any)) != 0 {
		t.Fatalf("list rule ignored: %v", b)
	}
	if _, b = e.do(t, e.su, "GET", "/api/crypto/lookup/patients/ssn?value=123-45", ""); len(b["items"].([]any)) != 1 {
		t.Fatalf("superuser: %v", b)
	}
}

func TestFilterAndSortRejected(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.create(t)
	cases := []string{
		"filter=" + `diagnosis="flu"`,
		"filter=" + `name="Ann"%20%26%26%20diagnosis~"f"`,
		"filter=" + `ssn="123-45"`, // blind-index too in PR 1
		"filter=" + `(email!="")`,
		"sort=-diagnosis",
		"sort=name,%2Bssn",
		"filter=" + `diagnosis:lower="flu"`,
	}
	for _, q := range cases {
		code, b := e.do(t, e.su, "GET", "/api/collections/patients/records?"+q, "")
		if code != 400 {
			t.Fatalf("%s: status %d %v", q, code, b)
		}
		d := b["data"].(map[string]any)
		var ent map[string]any
		for _, v := range d {
			ent = v.(map[string]any)
		}
		if ent["code"] != ErrCode {
			t.Fatalf("%s: %v", q, b)
		}
	}
	// plain fields, string literals that mention the name, and other collections are fine
	for _, q := range []string{`filter=name="diagnosis"`, `filter=name="Ann"`, "sort=-name", "sort=-id"} {
		if code, b := e.do(t, e.su, "GET", "/api/collections/patients/records?"+q, ""); code != 200 {
			t.Fatalf("%s: %d %v", q, code, b)
		}
	}
	if code, _ := e.do(t, e.su, "GET", `/api/collections/users/records?filter=name="diagnosis"`, ""); code != 200 {
		t.Fatal("other collection affected")
	}
}

func TestRotationKeepsOldRowsReadable(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id1 := e.create(t)
	col, _ := e.app.FindCollectionByNameOrId("patients")
	if _, err := e.m.newKey(col.Id); err != nil { // new key, rows NOT yet re-encrypted
		t.Fatal(err)
	}
	id2 := e.create(t)
	if !strings.HasPrefix(e.raw(t, id1, "diagnosis"), Prefix+"1:") || !strings.HasPrefix(e.raw(t, id2, "diagnosis"), Prefix+"2:") {
		t.Fatal("expected v1 for the old row and v2 for the new one")
	}
	for _, id := range []string{id1, id2} {
		if _, b := e.do(t, nil, "GET", "/api/collections/patients/records/"+id, ""); b["diagnosis"] != "flu" || b["ssn"] != "123-45" {
			t.Fatalf("%s: %v", id, b)
		}
	}
	// lookup finds rows indexed under either version
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "123-45"); len(recs) != 2 {
		t.Fatalf("lookup across versions: %d", len(recs))
	}
	// retire refuses while v1 is used
	if r, err := Retire(e.app, "patients"); err != nil || len(r.Retired) != 0 || len(r.InUse) != 1 {
		t.Fatalf("retire: %+v %v", r, err)
	}

	ver, n, err := Rotate(e.app, "patients", nil)
	if err != nil || ver != 3 {
		t.Fatalf("rotate: %d %d %v", ver, n, err)
	}
	for _, id := range []string{id1, id2} {
		if !strings.HasPrefix(e.raw(t, id, "diagnosis"), Prefix+"3:") {
			t.Fatalf("not re-encrypted: %s", e.raw(t, id, "diagnosis"))
		}
	}
	if recs, _ := FindByBlindIndex(e.app, "patients", "ssn", "123-45"); len(recs) != 2 {
		t.Fatal("lookup after rotate")
	}
	r, err := Retire(e.app, "patients")
	if err != nil || len(r.Retired) != 2 || len(r.InUse) != 0 {
		t.Fatalf("retire: %+v %v", r, err)
	}
	if _, b := e.do(t, nil, "GET", "/api/collections/patients/records/"+id1, ""); b["diagnosis"] != "flu" {
		t.Fatalf("after retire: %v", b)
	}
	infos, _ := e.m.KeyInfos(col.Id)
	if len(infos) != 3 || !infos[2].Active || infos[0].RetiredAt == "" {
		t.Fatalf("key infos: %+v", infos)
	}
	// wrapped key material is destroyed
	keys, _ := e.m.keyRecords(col.Id)
	if keys[0].GetString("wrapped_dek") != "" {
		t.Fatal("retired key material kept")
	}
	if rep, err := Verify(e.app, "patients", 100); err != nil || len(rep.Failures) != 0 {
		t.Fatalf("verify: %+v %v", rep, err)
	}
}

func TestMissingMasterKey(t *testing.T) {
	e := setup(t, "-")
	if e.m.Active() {
		t.Fatal("must be inactive")
	}
	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err == nil {
		t.Fatal("enable must refuse without a master key")
	}
	if rows, _ := e.app.FindAllRecords(FieldsCollection); len(rows) != 0 {
		t.Fatal("nothing may be stored")
	}
	// no config: collections behave normally
	if code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", createBody); code != 200 || b["diagnosis"] != "flu" {
		t.Fatal(code, b)
	}
	// with the key: configure, then restart without it
	key := newKey(t)
	t.Setenv(EnvMasterKey, key)
	e.m.master, _ = LoadMasterKey()
	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	id := e.create(t)
	e.m.master = nil
	e.m.Invalidate()
	if code, _ := e.do(t, e.su, "POST", "/api/collections/patients/records", createBody); code == 200 {
		t.Fatal("plaintext must not be stored in an encrypted field without a key")
	}
	_, b := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "" {
		t.Fatalf("without a key the value must not be served as plaintext or crash: %v", b["diagnosis"])
	}
	// wrong key: unwrap fails, reads give empty
	t.Setenv(EnvMasterKey, newKey(t))
	e.m.master, _ = LoadMasterKey()
	e.m.Invalidate()
	_, b = e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "" {
		t.Fatalf("wrong key: %v", b["diagnosis"])
	}
}

func TestTamperedCiphertext(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	var mu sync.Mutex
	var events []string
	SetAuditSink(func(action, collection, record string, details map[string]any) {
		mu.Lock()
		events = append(events, action+":"+record)
		mu.Unlock()
	})
	t.Cleanup(func() { SetAuditSink(nil) })

	id := e.create(t)
	raw := e.raw(t, id, "diagnosis")
	b := []byte(raw)
	b[len(b)-3] ^= 1
	if _, err := e.app.DB().NewQuery("UPDATE patients SET diagnosis={:d} WHERE id={:id}").Bind(map[string]any{"d": string(b), "id": id}).Execute(); err != nil {
		t.Fatal(err)
	}
	code, out := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if code != 200 || out["diagnosis"] != "" || out["ssn"] != "123-45" {
		t.Fatalf("tampered: %d %v", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 1 || events[0] != ActionDecryptFailed+":"+id {
		t.Fatalf("audit events: %v", events)
	}
	if rep, _ := Verify(e.app, "patients", 10); len(rep.Failures) != 1 {
		t.Fatalf("verify must report it: %+v", rep)
	}
	// an unrelated update must not wipe the tampered value or crash
	if code, _ := e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, `{"name":"B"}`); code != 200 && code != 400 {
		t.Fatal(code)
	}
	if got := e.raw(t, id, "diagnosis"); got != string(b) {
		t.Fatalf("tampered value was overwritten: %q", got)
	}
}

func TestHooksSeeCiphertextUntilDecrypt(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	var seen, after string
	e.app.OnRecordAfterCreateSuccess("patients").BindFunc(func(ev *core.RecordEvent) error {
		seen = ev.Record.GetString("diagnosis")
		if err := Decrypt(ev.App, ev.Record); err != nil {
			t.Error(err)
		}
		after = ev.Record.GetString("diagnosis")
		return ev.Next()
	})
	e.create(t)
	if !IsCiphertext(seen) {
		t.Fatalf("hook should see ciphertext, got %q", seen)
	}
	if after != "flu" {
		t.Fatalf("Decrypt: %q", after)
	}

	// load, decrypt, edit, save through Go
	col, _ := e.app.FindCollectionByNameOrId("patients")
	r := core.NewRecord(col)
	r.Set("diagnosis", "x")
	r.Set("email", "x@example.com")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	r2, _ := e.app.FindRecordById("patients", r.Id)
	if !IsCiphertext(r2.GetString("diagnosis")) {
		t.Fatal("model load returns ciphertext")
	}
	_ = Decrypt(e.app, r2)
	r2.Set("diagnosis", r2.GetString("diagnosis")+"-edited")
	if err := e.app.Save(r2); err != nil {
		t.Fatal(err)
	}
	r3, _ := e.app.FindRecordById("patients", r.Id)
	_ = Decrypt(e.app, r3)
	if r3.GetString("diagnosis") != "x-edited" || r3.GetString("email") != "x@example.com" {
		t.Fatalf("%q %q", r3.GetString("diagnosis"), r3.GetString("email"))
	}
}

func TestAdminPlaintextOff(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.create(t)
	t.Setenv(EnvAdminPlaintext, "off")
	_, b := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	if !IsCiphertext(b["diagnosis"].(string)) || !IsCiphertext(b["email"].(string)) {
		t.Fatalf("superuser must see ciphertext: %v", b)
	}
	_, b = e.do(t, nil, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "flu" {
		t.Fatalf("public users still get plaintext: %v", b)
	}
	// the Admin UI saves the whole record back with the ciphertext: no change
	before := e.raw(t, id, "diagnosis")
	_, su := e.do(t, e.su, "GET", "/api/collections/patients/records/"+id, "")
	su["name"] = "Zed"
	body, _ := json.Marshal(su)
	if code, out := e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id, string(body)); code != 200 {
		t.Fatal(code, out)
	}
	if e.raw(t, id, "diagnosis") != before {
		t.Fatal("round-tripped ciphertext was re-encrypted")
	}
	_, b = e.do(t, nil, "GET", "/api/collections/patients/records/"+id, "")
	if b["diagnosis"] != "flu" || b["name"] != "Zed" || b["email"] != "ann@example.com" {
		t.Fatalf("%v", b)
	}
}

func TestEligibilityAndLint(t *testing.T) {
	e := setup(t, "")
	for _, c := range [][3]string{
		{"patients", "id", ModeRandom},
		{"patients", "created", ModeRandom},
		{"patients", "nope", ModeRandom},
		{"users", "email", ModeRandom},
		{"users", "password", ModeRandom},
		{"patients", "ssn", "bogus"},
		{"nocoll", "x", ModeRandom},
		{core.CollectionNameSuperusers, "email", ModeRandom},
	} {
		if _, err := Enable(e.app, c[0], c[1], c[2], nil); err == nil {
			t.Errorf("%v must be refused", c)
		}
	}
	col, _ := e.app.FindCollectionByNameOrId("patients")
	col.Fields.Add(&core.NumberField{Name: "age"})
	e.app.Save(col)
	if _, err := Enable(e.app, "patients", "age", ModeRandom, nil); err == nil {
		t.Error("number fields cannot be encrypted")
	}
	if _, err := Enable(e.app, "patients", "notes", ModeBlindIndex, nil); err == nil {
		t.Error("blind-index on json must be refused")
	}

	if _, err := Enable(e.app, "patients", "diagnosis", ModeRandom, nil); err != nil {
		t.Fatal(err)
	}
	rule := "diagnosis = 'flu'"
	col, _ = e.app.FindCollectionByNameOrId("patients")
	col.ListRule = &rule
	col.AddIndex("idx_diag", false, "diagnosis", "")
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	fs, err := Lint(e.app)
	if err != nil {
		t.Fatal(err)
	}
	where := map[string]bool{}
	for _, f := range fs {
		where[f.Where] = true
	}
	if !where["listRule"] || !where["index"] || where["viewRule"] {
		t.Fatalf("lint: %+v", fs)
	}
	st, err := Status(e.app)
	if err != nil || !st.MasterKey || len(st.Collections) != 1 || st.Collections[0].Fields["diagnosis"] != ModeRandom || len(st.Collections[0].Keys) != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
}

func TestCollectionDeleteCleansUp(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.create(t)
	col, _ := e.app.FindCollectionByNameOrId("patients")
	if err := e.app.Delete(col); err != nil {
		t.Fatal(err)
	}
	var n int
	e.app.DB().NewQuery("SELECT (SELECT COUNT(*) FROM " + IndexTable + ")+(SELECT COUNT(*) FROM " + FieldsCollection + ")+(SELECT COUNT(*) FROM " + KeysCollection + ")").Row(&n)
	if n != 0 {
		t.Fatalf("leftovers: %d", n)
	}
}

func TestMasterKeyParsing(t *testing.T) {
	t.Setenv(EnvMasterKey, "")
	t.Setenv(EnvMasterKeyFile, "")
	if _, err := LoadMasterKey(); err != ErrNoMasterKey {
		t.Fatal(err)
	}
	t.Setenv(EnvMasterKey, base64.StdEncoding.EncodeToString(make([]byte, 16)))
	if _, err := LoadMasterKey(); err == nil {
		t.Fatal("16 bytes must be refused")
	}
	k := newKey(t)
	f := t.TempDir() + "/k"
	if err := writeFile(f, k+"\n"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvMasterKey, "")
	t.Setenv(EnvMasterKeyFile, f)
	if b, err := LoadMasterKey(); err != nil || len(b) != 32 {
		t.Fatal(err)
	}
}

func TestPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	e := setup(t, "")
	col, _ := e.app.FindCollectionByNameOrId("patients")
	for i := 0; i < 2; i++ {
		col.Fields.Add(&core.TextField{Name: "x" + string(rune('a'+i))})
	}
	e.app.Save(col)
	run := func(coll string) time.Duration {
		c, _ := e.app.FindCollectionByNameOrId(coll)
		start := time.Now()
		for i := 0; i < 200; i++ {
			r := core.NewRecord(c)
			r.Set("name", "n")
			for _, f := range []string{"diagnosis", "ssn", "xa", "xb"} {
				r.Set(f, "value-"+f+"-0123456789")
			}
			r.Set("email", "a@example.com")
			if err := e.app.Save(r); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start)
	}
	base := run("patients")
	// five random-mode fields: the pure cost of AES-GCM, key and config lookups
	for _, f := range []string{"diagnosis", "ssn", "xa", "xb", "email"} {
		if _, err := Enable(e.app, "patients", f, ModeRandom, nil); err != nil {
			t.Fatal(err)
		}
	}
	enc := run("patients")
	t.Logf("200 creates: plain %v, 5 encrypted fields %v (%+.1f%%)", base, enc, 100*float64(enc-base)/float64(base))
	if enc > base*2 {
		t.Fatalf("encryption cost too high: %v vs %v", enc, base)
	}
	// blind-index adds one extra index write per create (a second SQLite write)
	if _, err := Disable(e.app, "patients", "ssn", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(e.app, "patients", "ssn", ModeBlindIndex, nil); err != nil {
		t.Fatal(err)
	}
	bi := run("patients")
	t.Logf("with one blind-index field: %v (%+.1f%% vs plain)", bi, 100*float64(bi-base)/float64(base))
}
