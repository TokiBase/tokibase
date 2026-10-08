//go:build !no_nativeauth

package nativeauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

const path = "/api/collections/clients/auth-with-native"

type env struct {
	app   *tests.TestApp
	m     *Module
	mux   http.Handler
	rsa   *rsa.PrivateKey
	ec    *ecdsa.PrivateKey
	mu    sync.Mutex
	fails int
	audit []string
	now   time.Time
	hits  int
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func setup(t *testing.T) *env {
	t.Helper()
	t.Setenv(AudienceEnv("apple", "clients"), "bundle.id")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{app: app, now: time.Now()}
	t.Cleanup(func() { SetFailureSink(nil); SetLockedSink(nil); SetAuditSink(nil); app.Cleanup() })
	SetFailureSink(func(string, *core.Record) { e.mu.Lock(); e.fails++; e.mu.Unlock() })
	SetAuditSink(func(a, _, _ string, _ map[string]any) { e.mu.Lock(); e.audit = append(e.audit, a); e.mu.Unlock() })

	e.rsa, _ = rsa.GenerateKey(rand.Reader, 2048)
	e.ec, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits++
		ecx, ecy := e.ec.X.FillBytes(make([]byte, 32)), e.ec.Y.FillBytes(make([]byte, 32))
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": "rk", "n": b64u(e.rsa.N.Bytes()), "e": b64u([]byte{1, 0, 1})},
			{"kty": "EC", "kid": "ek", "crv": "P-256", "x": b64u(ecx), "y": b64u(ecy)},
		}})
	}))
	t.Cleanup(srv.Close)

	e.m = Register(app)
	e.m.now = func() time.Time { return e.now }
	e.m.keys["google"].url = srv.URL
	e.m.keys["apple"].url = srv.URL

	col, _ := app.FindCollectionByNameOrId("clients")
	col.OAuth2.Enabled = true
	col.OAuth2.Providers = []core.OAuth2ProviderConfig{
		{Name: "google", ClientId: "gid", ClientSecret: "s"},
		{Name: "apple", ClientId: "aid", ClientSecret: "s"},
	}
	col.CreateRule = types.Pointer("")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		e.mux = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) token(t *testing.T, iss, aud string, mod func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": iss, "aud": aud, "sub": "sub-1", "email": "native@example.com", "email_verified": true,
		"name": "Native User", "iat": e.now.Add(-time.Minute).Unix(), "exp": e.now.Add(time.Hour).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	tk := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tk.Header["kid"] = "rk"
	s, err := tk.SignedString(e.rsa)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) raw(p string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", p, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *env) doAt(t *testing.T, p string, body any, status int) map[string]any {
	t.Helper()
	rec := e.raw(p, body)
	if rec.Code != status {
		t.Fatalf("status %d want %d: %s", rec.Code, status, rec.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func (e *env) do(t *testing.T, body any, status int) map[string]any {
	t.Helper()
	return e.doAt(t, path, body, status)
}

func google(e *env, t *testing.T, mod func(jwt.MapClaims)) map[string]any {
	return map[string]any{"provider": "google", "idToken": e.token(t, "https://accounts.google.com", "gid", mod)}
}

func TestGoogleNewUserThenLogin(t *testing.T) {
	e := setup(t)
	out := e.do(t, map[string]any{
		"provider": "google", "idToken": e.token(t, "accounts.google.com", "gid", nil),
		"createData": map[string]any{"password": "1234567890", "passwordConfirm": "1234567890"},
	}, 200)
	if out["token"] == "" || out["record"] == nil {
		t.Fatalf("no auth response: %v", out)
	}
	meta := out["meta"].(map[string]any)
	if meta["isNew"] != true || meta["id"] != "sub-1" {
		t.Fatalf("meta: %v", meta)
	}
	rec, err := e.app.FindAuthRecordByEmail("clients", "native@example.com")
	if err != nil || !rec.Verified() {
		t.Fatalf("record: %v verified=%v", err, rec != nil && rec.Verified())
	}
	ext, err := e.app.FindAllExternalAuthsByRecord(rec)
	if err != nil || len(ext) != 1 || ext[0].Provider() != "google" || ext[0].ProviderId() != "sub-1" {
		t.Fatalf("external auths: %v %v", ext, err)
	}
	// second sign-in with a fresh token for the same sub maps to the same record
	out = e.do(t, google(e, t, func(c jwt.MapClaims) { c["jti"] = "other"; c["email"] = "changed@example.com" }), 200)
	if out["meta"].(map[string]any)["isNew"] != false || out["record"].(map[string]any)["id"] != rec.Id {
		t.Fatalf("second login: %v", out)
	}
	if len(e.audit) != 2 || e.audit[0] != ActionLogin {
		t.Fatalf("audit: %v", e.audit)
	}
}

func TestUnverifiedEmailIsNotUsed(t *testing.T) {
	e := setup(t)
	unverified := func(c jwt.MapClaims) { c["email_verified"] = false }
	// the clients collection requires an email, and the unverified one is not used
	e.do(t, google(e, t, unverified), 400)
	body := google(e, t, func(c jwt.MapClaims) { c["email_verified"] = false; c["jti"] = "j2" })
	body["createData"] = map[string]any{"email": "typed@example.com", "password": "1234567890", "passwordConfirm": "1234567890"}
	// the collection auth rule may still refuse an unverified record; it must exist either way
	e.do(t, body, 403)
	rec, err := e.app.FindAuthRecordByEmail("clients", "typed@example.com")
	if err != nil || rec.Verified() {
		t.Fatalf("record must exist and stay unverified: %v", err)
	}
	if _, err := e.app.FindAuthRecordByEmail("clients", "native@example.com"); err == nil {
		t.Fatal("unverified token email must not be stored")
	}
}

func TestRejections(t *testing.T) {
	e := setup(t)
	cases := map[string]map[string]any{
		"wrong aud":      {"provider": "google", "idToken": e.token(t, "accounts.google.com", "other", nil)},
		"wrong iss":      {"provider": "google", "idToken": e.token(t, "https://evil.example", "gid", nil)},
		"expired":        {"provider": "google", "idToken": e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) { c["exp"] = e.now.Add(-2 * time.Minute).Unix() })},
		"future iat":     {"provider": "google", "idToken": e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) { c["iat"] = e.now.Add(time.Hour).Unix() })},
		"bad provider":   {"provider": "github", "idToken": "x"},
		"missing token":  {"provider": "google"},
		"garbage token":  {"provider": "google", "idToken": "a.b.c"},
		"nonce expected": {"provider": "google", "nonce": "n1", "idToken": e.token(t, "accounts.google.com", "gid", nil)},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { e.do(t, body, 400) })
	}
	// skew: expired 30 s ago is still accepted (60 s leeway)
	e.do(t, google(e, t, func(c jwt.MapClaims) { c["exp"] = e.now.Add(-30 * time.Second).Unix() }), 200)
	// HS256 signed token is refused
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"iss": "accounts.google.com", "aud": "gid", "sub": "s2", "exp": e.now.Add(time.Hour).Unix(),
	}).SignedString([]byte("secret"))
	e.do(t, map[string]any{"provider": "google", "idToken": hs}, 400)
}

func TestExtraAudienceAndES256(t *testing.T) {
	e := setup(t)
	t.Setenv(AudienceEnv("google", "clients"), "android-cid, ios-cid")
	c := jwt.MapClaims{"iss": "accounts.google.com", "aud": []string{"zzz", "ios-cid"}, "sub": "ec-sub",
		"email": "ec@example.com", "email_verified": true, "exp": e.now.Add(time.Hour).Unix()}
	tk := jwt.NewWithClaims(jwt.SigningMethodES256, c)
	tk.Header["kid"] = "ek"
	s, err := tk.SignedString(e.ec)
	if err != nil {
		t.Fatal(err)
	}
	e.do(t, map[string]any{"provider": "google", "idToken": s}, 200)
}

func TestReplay(t *testing.T) {
	e := setup(t)
	body := google(e, t, nil)
	e.do(t, body, 200)
	e.do(t, body, 400)
	// a replay says nothing about the account owner: no lockout strike
	if e.fails != 0 {
		t.Fatalf("failure sink calls: %d", e.fails)
	}
}

func TestFailedCreateDoesNotBurnToken(t *testing.T) {
	e := setup(t)
	body := google(e, t, nil)
	body["createData"] = map[string]any{"password": "short", "passwordConfirm": "different"}
	e.do(t, body, 400)
	delete(body, "createData")
	e.do(t, body, 200)
}

func TestAppleNonce(t *testing.T) {
	e := setup(t)
	raw := "raw-nonce-123"
	h := sha256.Sum256([]byte(raw))
	mk := func(nonce string) map[string]any {
		return map[string]any{"provider": "apple", "nonce": raw, "idToken": e.token(t, "https://appleid.apple.com", "bundle.id", func(c jwt.MapClaims) {
			c["nonce"] = nonce
			c["nonce_supported"] = true
			c["email_verified"] = "true"
			c["jti"] = nonce
		})}
	}
	e.do(t, mk("wrong"), 400)
	e.do(t, mk(hex.EncodeToString(h[:])), 200)
	// Apple: a claim equal to the raw nonce is NOT accepted (the claim must be sha256-hex(raw))
	body := map[string]any{"provider": "apple", "nonce": raw, "idToken": e.token(t, "https://appleid.apple.com", "aid", func(c jwt.MapClaims) {
		c["nonce"] = raw
		c["sub"] = "apple-2"
		c["email"] = "apple2@example.com"
	})}
	e.do(t, body, 400)
	// stolen-token attack: the request nonce copied from the token payload
	claim := hex.EncodeToString(h[:])
	e.do(t, map[string]any{"provider": "apple", "nonce": claim, "idToken": e.token(t, "https://appleid.apple.com", "aid", func(c jwt.MapClaims) {
		c["nonce"] = claim
		c["sub"] = "apple-3"
	})}, 400)
	// wrong issuer for provider
	e.do(t, map[string]any{"provider": "apple", "idToken": e.token(t, "accounts.google.com", "aid", nil)}, 400)
}

func TestProviderDisabled(t *testing.T) {
	e := setup(t)
	col, _ := e.app.FindCollectionByNameOrId("clients")
	col.OAuth2.Providers = []core.OAuth2ProviderConfig{{Name: "apple", ClientId: "aid", ClientSecret: "s"}}
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	e.do(t, google(e, t, nil), 400)
	col.OAuth2.Enabled = false
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	e.do(t, map[string]any{"provider": "apple", "idToken": "x"}, 403)
}

func TestJWKSRefreshLimit(t *testing.T) {
	e := setup(t)
	bad := func() map[string]any {
		tk := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "accounts.google.com", "aud": "gid", "sub": "x", "exp": e.now.Add(time.Hour).Unix()})
		tk.Header["kid"] = "unknown"
		s, _ := tk.SignedString(e.rsa)
		return map[string]any{"provider": "google", "idToken": s}
	}
	e.now = e.now.Add(2 * time.Minute)
	e.do(t, google(e, t, nil), 200)
	before := e.hits
	e.now = e.now.Add(2 * time.Minute)
	e.do(t, bad(), 400) // unknown kid triggers one refresh
	e.do(t, bad(), 400) // second within a minute does not
	if e.hits != before+1 {
		t.Fatalf("jwks fetches: %d -> %d", before, e.hits)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.do(t, bad(), 400)
	if e.hits != before+2 {
		t.Fatalf("jwks fetches after a minute: %d", e.hits)
	}
}

func TestEnabledSwitch(t *testing.T) {
	t.Setenv(EnvSwitch, "off")
	if Enabled() {
		t.Fatal("expected disabled")
	}
	t.Setenv(EnvSwitch, "")
	if !Enabled() {
		t.Fatal("expected enabled")
	}
}

func TestGoogleNonceIsRaw(t *testing.T) {
	e := setup(t)
	mk := func(claim, req, jti string) map[string]any {
		return map[string]any{"provider": "google", "nonce": req, "idToken": e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) {
			c["nonce"] = claim
			c["jti"] = jti
		})}
	}
	e.do(t, mk("abc", "xyz", "a"), 400)
	h := sha256.Sum256([]byte("abc"))
	e.do(t, mk(hex.EncodeToString(h[:]), "abc", "b"), 400) // hash form is Apple only
	e.do(t, mk("abc", "abc", "c"), 200)
}

func flipTail(s string) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	i := strings.IndexByte(alpha, s[len(s)-1])
	return s[:len(s)-1] + string(alpha[i^1])
}

func TestSignatureTailCannotReplay(t *testing.T) {
	e := setup(t)
	tok := e.token(t, "accounts.google.com", "gid", nil) // no jti
	mutated := flipTail(tok)
	if mutated == tok {
		t.Fatal("mutation is a no-op")
	}
	// the jwt library alone would accept the mutated tail (same signature bytes)
	e.do(t, map[string]any{"provider": "google", "idToken": mutated}, 400)
	e.do(t, map[string]any{"provider": "google", "idToken": tok}, 200)
	e.do(t, map[string]any{"provider": "google", "idToken": mutated}, 400)
	e.do(t, map[string]any{"provider": "google", "idToken": tok}, 400)
	// padded variant of the original
	e.do(t, map[string]any{"provider": "google", "idToken": tok + "=="}, 400)
}

func TestCanonicalJWT(t *testing.T) {
	ok := b64u([]byte("abcd")) + "." + b64u([]byte("x")) + "." + b64u([]byte("sig"))
	if err := canonicalJWT(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "a.b", "a..c", ok + ".x", b64u([]byte("abcd")) + "=.x.y", "AB.x.y"} {
		if canonicalJWT(bad) == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAudiencesPerCollection(t *testing.T) {
	e := setup(t)
	t.Setenv(AudienceEnv("google", "clients"), "android-cid")
	t.Setenv("TOKI_NATIVEAUTH_GOOGLE_AUDIENCES", "global-cid") // legacy global list is ignored
	users, _ := e.app.FindCollectionByNameOrId("users")
	users.OAuth2.Enabled = true
	users.OAuth2.Providers = []core.OAuth2ProviderConfig{{Name: "google", ClientId: "uid", ClientSecret: "s"}}
	if err := e.app.Save(users); err != nil {
		t.Fatal(err)
	}
	other := "/api/collections/users/auth-with-native"
	mk := func(aud, jti string) map[string]any {
		return map[string]any{"provider": "google", "idToken": e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) {
			c["aud"] = aud
			c["jti"] = jti
		})}
	}
	e.doAt(t, other, mk("android-cid", "1"), 400) // token for collection A refused on collection B
	e.do(t, mk("android-cid", "2"), 200)
	e.do(t, mk("global-cid", "3"), 400)

	// provider config extra.audiences
	col, _ := e.app.FindCollectionByNameOrId("clients")
	col.OAuth2.Providers[0].Extra = map[string]any{"audiences": "extra-cid, extra2"}
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	e.do(t, mk("extra2", "4"), 200)
	e.doAt(t, other, mk("extra2", "5"), 400)
}

func TestMoreRejections(t *testing.T) {
	e := setup(t)
	post := func(name string, tok string) {
		t.Run(name, func(t *testing.T) { e.do(t, map[string]any{"provider": "google", "idToken": tok}, 400) })
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": "accounts.google.com", "aud": "gid", "sub": "s", "exp": e.now.Add(time.Hour).Unix()}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	post("alg none", none)
	// key confusion: RS256 header pointing at the EC key and vice versa
	rsTok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": "accounts.google.com", "aud": "gid", "sub": "s", "exp": e.now.Add(time.Hour).Unix()})
	rsTok.Header["kid"] = "ek"
	s1, _ := rsTok.SignedString(e.rsa)
	post("rs256 with ec kid", s1)
	esTok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{"iss": "accounts.google.com", "aud": "gid", "sub": "s", "exp": e.now.Add(time.Hour).Unix()})
	esTok.Header["kid"] = "rk"
	s2, _ := esTok.SignedString(e.ec)
	post("es256 with rsa kid", s2)
	post("future nbf", e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) { c["nbf"] = e.now.Add(time.Hour).Unix() }))
	post("azp without aud", e.token(t, "accounts.google.com", "gid", func(c jwt.MapClaims) { delete(c, "aud"); c["azp"] = "gid" }))
	post("oversized", strings.Repeat("a", 16385))
}

func TestGoogleEmailVerifiedString(t *testing.T) {
	e := setup(t)
	e.do(t, google(e, t, func(c jwt.MapClaims) { c["email_verified"] = "true" }), 200)
	rec, err := e.app.FindAuthRecordByEmail("clients", "native@example.com")
	if err != nil || !rec.Verified() {
		t.Fatalf("record: %v", err)
	}
}

func TestRateLimit429(t *testing.T) {
	e := setup(t)
	for i := 0; i < maxFailures; i++ {
		e.do(t, map[string]any{"provider": "google", "idToken": "a.b.c"}, 400)
	}
	rec := e.raw(path, google(e, t, nil))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("status %d retry-after %q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestLockedSink(t *testing.T) {
	e := setup(t)
	e.do(t, google(e, t, func(c jwt.MapClaims) { c["jti"] = "1" }), 200)
	SetLockedSink(func(string, *core.Record) bool { return true })
	e.do(t, google(e, t, func(c jwt.MapClaims) { c["jti"] = "2" }), 400)
	if e.fails != 1 {
		t.Fatalf("lockout strikes: %d", e.fails)
	}
	// the locked token was not consumed as a login
	if len(e.audit) != 2 || e.audit[1] != ActionFailed {
		t.Fatalf("audit: %v", e.audit)
	}
}

func TestConcurrentSameToken(t *testing.T) {
	e := setup(t)
	body := google(e, t, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e.raw(path, body).Code == 200 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("%d concurrent sign-ins succeeded with one token", ok)
	}
}

func TestRateKeyAndEviction(t *testing.T) {
	if rateKey("2001:db8::1") != rateKey("2001:db8::ffff") || rateKey("2001:db8::1") == rateKey("2001:db9::1") {
		t.Fatal("ipv6 /64 keying")
	}
	if rateKey("203.0.113.5") != "203.0.113.5" {
		t.Fatal("ipv4 key")
	}
	l := newLimiter()
	now := time.Now()
	for i := 0; i < 10; i++ {
		l.hits[string(rune('a'+i))] = []time.Time{now.Add(time.Duration(i) * time.Second)}
	}
	l.evictOldest(3)
	if len(l.hits) != 7 || l.hits["a"] != nil || l.hits["j"] == nil {
		t.Fatalf("eviction: %v", len(l.hits))
	}
}
