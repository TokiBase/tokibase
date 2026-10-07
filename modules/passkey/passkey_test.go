package passkey

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

const (
	testRPID   = "example.com"
	testOrigin = "https://example.com"
	base       = "/api/collections/clients/passkeys"
)

// softAuth is a minimal software authenticator (ES256, attestation "none").
type softAuth struct {
	key    *ecdsa.PrivateKey
	credID []byte
	count  uint32
	handle []byte
}

func newSoftAuth(t *testing.T) *softAuth {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &softAuth{key: k, credID: id}
}

func (a *softAuth) authData(flags byte, attested bool) []byte {
	h := sha256.Sum256([]byte(testRPID))
	out := append([]byte{}, h[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.count)
	if attested {
		out = append(out, make([]byte, 16)...) // aaguid
		out = binary.BigEndian.AppendUint16(out, uint16(len(a.credID)))
		out = append(out, a.credID...)
		pub, _ := a.key.PublicKey.ECDH()
		raw := pub.Bytes() // 0x04 || X || Y
		cose, _ := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: raw[1:33], -3: raw[33:65]})
		out = append(out, cose...)
	}
	return out
}

func clientData(typ, challenge string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": testOrigin, "crossOrigin": false})
	return b
}

func (a *softAuth) register(t *testing.T, opts map[string]any) json.RawMessage {
	t.Helper()
	cd := clientData("webauthn.create", opts["challenge"].(string))
	user := opts["user"].(map[string]any)
	a.handle = []byte(user["id"].(string)) // base64url string; only echoed back on login
	att, _ := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": a.authData(0x01|0x04|0x40|0x08|0x10, true)})
	return mustJSON(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cd), "attestationObject": b64.EncodeToString(att),
			"transports": []string{"internal"},
		},
		"clientExtensionResults": map[string]any{},
	})
}

func (a *softAuth) assert(t *testing.T, opts map[string]any, userHandle string) json.RawMessage {
	t.Helper()
	cd := clientData("webauthn.get", opts["challenge"].(string))
	ad := a.authData(0x01|0x04|0x08|0x10, false)
	ch := sha256.Sum256(cd)
	sum := sha256.Sum256(append(append([]byte{}, ad...), ch[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return mustJSON(map[string]any{
		"id": b64.EncodeToString(a.credID), "rawId": b64.EncodeToString(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64.EncodeToString(cd), "authenticatorData": b64.EncodeToString(ad),
			"signature": b64.EncodeToString(sig), "userHandle": userHandle,
		},
		"clientExtensionResults": map[string]any{},
	})
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type env struct {
	app *tests.TestApp
	m   *Module
	mux http.Handler
}

func setup(t *testing.T, active bool) *env {
	t.Helper()
	if active {
		t.Setenv(EnvRPID, testRPID)
		t.Setenv(EnvOrigins, testOrigin+",android:apk-key-hash:abc123")
	} else {
		t.Setenv(EnvRPID, "")
	}
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		h = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return &env{app: app, m: m, mux: h}
}

func (e *env) do(t *testing.T, method, url string, body any, token string, status int) map[string]any {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(mustJSON(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: status %d want %d: %s", method, url, rec.Code, status, rec.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func (e *env) password(t *testing.T) string {
	out := e.do(t, "POST", "/api/collections/clients/auth-with-password", map[string]any{"identity": "test@example.com", "password": "1234567890"}, "", 200)
	return out["token"].(string)
}

// enroll registers a passkey for the test user and returns the authenticator.
func (e *env) enroll(t *testing.T, tok string) (*softAuth, map[string]any) {
	a := newSoftAuth(t)
	opts := e.do(t, "POST", base+"/register/options", nil, tok, 200)
	if sel := opts["authenticatorSelection"].(map[string]any); sel["residentKey"] != "required" || sel["userVerification"] != "preferred" {
		t.Fatalf("selection %v", sel)
	}
	res := e.do(t, "POST", base+"/register/verify", map[string]any{"credential": a.register(t, opts), "name": "Laptop"}, tok, 200)
	return a, res
}

func (e *env) loginOpts(t *testing.T, identity string) map[string]any {
	var body any
	if identity != "" {
		body = map[string]any{"identity": identity}
	}
	return e.do(t, "POST", base+"/login/options", body, "", 200)
}

func userHandle(e *env) string {
	r, _ := e.app.FindAuthRecordByEmail("clients", "test@example.com")
	return b64.EncodeToString([]byte(r.Id))
}

func TestRegisterThenLogin(t *testing.T) {
	e := setup(t, true)
	var audits []string
	SetAuditSink(func(action, _, _ string, _ map[string]any) { audits = append(audits, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	tok := e.password(t)
	a, saved := e.enroll(t, tok)
	if saved["name"] != "Laptop" || saved["backupEligible"] != true {
		t.Fatalf("%v", saved)
	}
	list := e.do(t, "GET", base, nil, tok, 200)["items"].([]any)
	if len(list) != 1 {
		t.Fatalf("%v", list)
	}
	if _, leaked := list[0].(map[string]any)["public_key"]; leaked {
		t.Fatal("key material leaked")
	}

	// discoverable login, no identity
	opts := e.loginOpts(t, "")
	if _, has := opts["allowCredentials"]; has && len(opts["allowCredentials"].([]any)) > 0 {
		t.Fatalf("discoverable options must not list credentials: %v", opts)
	}
	a.count++
	out := e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, opts, userHandle(e))}, "", 200)
	if out["token"] == "" || out["token"] == nil || out["record"].(map[string]any)["email"] != "test@example.com" {
		t.Fatalf("%v", out)
	}
	// the token works
	e.do(t, "POST", "/api/collections/clients/auth-refresh", nil, out["token"].(string), 200)

	// identity-restricted login lists the credential
	opts = e.loginOpts(t, "test@example.com")
	if len(opts["allowCredentials"].([]any)) != 1 {
		t.Fatalf("%v", opts)
	}
	a.count++
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, opts, userHandle(e))}, "", 200)

	rows, _ := e.app.FindAllRecords(CollectionName, dbx.NewExp("1=1"))
	if rows[0].GetInt("sign_count") != 2 || rows[0].GetString("last_used") == "" || rows[0].GetBool("clone_suspected") {
		t.Fatalf("%v", rows[0].PublicExport())
	}
	want := []string{ActionRegister, ActionLogin, ActionLogin}
	if strings.Join(audits, ",") != strings.Join(want, ",") {
		t.Fatalf("audits %v", audits)
	}
}

func TestLoginUnknownCredentialFails(t *testing.T) {
	e := setup(t, true)
	var failures int
	SetFailureSink(func(string, string) { failures++ })
	t.Cleanup(func() { SetFailureSink(nil) })

	stranger := newSoftAuth(t)
	opts := e.loginOpts(t, "")
	out := e.do(t, "POST", base+"/login/verify", map[string]any{"credential": stranger.assert(t, opts, userHandle(e))}, "", 400)
	if out["message"] != "Failed to authenticate." {
		t.Fatalf("%v", out)
	}
	if failures != 0 {
		t.Fatal("unknown credential cannot be attributed to a record")
	}
}

func TestBadSignatureCountsFailure(t *testing.T) {
	e := setup(t, true)
	var got []string
	SetFailureSink(func(c, id string) { got = append(got, c+":"+id) })
	t.Cleanup(func() { SetFailureSink(nil) })

	a, _ := e.enroll(t, e.password(t))
	opts := e.loginOpts(t, "")
	a.key, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader) // wrong key
	a.count++
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, opts, userHandle(e))}, "", 400)
	if len(got) != 1 || !strings.HasPrefix(got[0], "clients:") {
		t.Fatalf("failure sink: %v", got)
	}
}

func TestSignCountRegressionFlagged(t *testing.T) {
	e := setup(t, true)
	var audits []string
	SetAuditSink(func(action, _, _ string, _ map[string]any) { audits = append(audits, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	a, _ := e.enroll(t, e.password(t))
	a.count = 10
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), userHandle(e))}, "", 200)
	a.count = 5 // regression
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), userHandle(e))}, "", 200) // policy warn
	rows, _ := e.app.FindAllRecords(CollectionName, dbx.NewExp("1=1"))
	if !rows[0].GetBool("clone_suspected") || rows[0].GetInt("sign_count") != 10 {
		t.Fatalf("%v", rows[0].PublicExport())
	}
	found := false
	for _, x := range audits {
		found = found || x == ActionClone
	}
	if !found {
		t.Fatalf("no clone audit: %v", audits)
	}

	// deny policy rejects
	e.m.cfg.ClonePolicy = "deny"
	a.count = 3
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), userHandle(e))}, "", 400)
}

func TestOtherUserCannotDelete(t *testing.T) {
	e := setup(t, true)
	tok := e.password(t)
	_, saved := e.enroll(t, tok)
	id := saved["id"].(string)

	// a different user of the same collection
	other, err := e.app.FindAuthRecordByEmail("clients", "test2@example.com")
	if err != nil {
		t.Skip("no second client in test data")
	}
	otherTok, _ := other.NewAuthToken()
	e.do(t, "DELETE", base+"/"+id, nil, otherTok, 404)
	e.do(t, "DELETE", base+"/"+id, nil, "", 401)
	if n := len(e.do(t, "GET", base, nil, otherTok, 200)["items"].([]any)); n != 0 {
		t.Fatalf("other user sees %d passkeys", n)
	}
	e.do(t, "DELETE", base+"/"+id, nil, tok, 204)
	if n := len(e.do(t, "GET", base, nil, tok, 200)["items"].([]any)); n != 0 {
		t.Fatal("not deleted")
	}
}

func TestChallengeSingleUseAndTTL(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))

	opts := e.loginOpts(t, "")
	a.count++
	cred := a.assert(t, opts, userHandle(e))
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": cred}, "", 200)
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": cred}, "", 400) // replay

	// expired challenge
	opts = e.loginOpts(t, "")
	if _, err := e.app.AuxDB().NewQuery("UPDATE {{_passkey_challenges}} SET [[expires]]=1").Execute(); err != nil {
		t.Fatal(err)
	}
	a.count++
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, opts, userHandle(e))}, "", 400)
}

func TestRegisterChallengeBoundToUser(t *testing.T) {
	e := setup(t, true)
	tok := e.password(t)
	opts := e.do(t, "POST", base+"/register/options", nil, tok, 200)
	other, err := e.app.FindAuthRecordByEmail("clients", "test2@example.com")
	if err != nil {
		t.Skip("no second client in test data")
	}
	otherTok, _ := other.NewAuthToken()
	a := newSoftAuth(t)
	e.do(t, "POST", base+"/register/verify", map[string]any{"credential": a.register(t, opts)}, otherTok, 400)
	e.do(t, "POST", base+"/register/options", nil, "", 401)
}

func TestInactiveWithoutEnvIs404(t *testing.T) {
	e := setup(t, false)
	if e.m != nil {
		t.Fatal("module must be nil")
	}
	for _, p := range []string{"/register/options", "/login/options", "/login/verify", "/register/verify"} {
		e.do(t, "POST", base+p, map[string]any{}, "", 404)
	}
	e.do(t, "GET", base, nil, "", 404)
	if _, err := e.app.FindCollectionByNameOrId(CollectionName); err == nil {
		t.Fatal("_passkeys must not be created when inactive")
	}
}

func TestConfig(t *testing.T) {
	t.Setenv(EnvRPID, "fitgymrun.com")
	t.Setenv(EnvOrigins, " https://fitgymrun.com , android:apk-key-hash:xyz ,,")
	c := LoadConfig()
	if !c.Active() || c.RPName != "fitgymrun.com" || len(c.Origins) != 2 || c.ClonePolicy != "warn" {
		t.Fatalf("%+v", c)
	}
	if _, err := New(nil, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOrigins, "")
	if c := LoadConfig(); len(c.Origins) != 1 || c.Origins[0] != "https://fitgymrun.com" {
		t.Fatalf("%+v", c)
	}
}

func TestAuthRuleApplies(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	col, _ := e.app.FindCollectionByNameOrId("clients")
	rule := `email != "test@example.com"`
	col.AuthRule = &rule
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	a.count++
	e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), userHandle(e))}, "", 403)
}

func TestMFAAppliesToPasskeyLogin(t *testing.T) {
	e := setup(t, true)
	a, _ := e.enroll(t, e.password(t))
	col, _ := e.app.FindCollectionByNameOrId("clients")
	col.OTP.Enabled = true // upstream requires 2 enabled methods for MFA (passkey is not counted)
	col.MFA.Enabled = true
	col.MFA.Rule = ""
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	a.count++
	out := e.do(t, "POST", base+"/login/verify", map[string]any{"credential": a.assert(t, e.loginOpts(t, ""), userHandle(e))}, "", 401)
	mfaId, _ := out["mfaId"].(string)
	if mfaId == "" {
		t.Fatalf("%v", out)
	}
	e.do(t, "POST", "/api/collections/clients/auth-with-password", map[string]any{"identity": "test@example.com", "password": "1234567890", "mfaId": mfaId}, "", 200)
}
