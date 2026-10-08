package proto

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newCert(t *testing.T, hub ed25519.PrivateKey, sub string, iat time.Time, ttl time.Duration) string {
	t.Helper()
	hubPub := hub.Public().(ed25519.PublicKey)
	tok, err := SignCert(hub, &CertClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: HubID(hubPub), Subject: sub, IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(iat.Add(ttl)),
		},
		Pub: "p", KX: "k", Ser: "s1", Params: map[string]any{"branch": "B12"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestNodeIDDerivation(t *testing.T) {
	a, _ := GenerateIdentity()
	b, _ := GenerateIdentity()
	id := a.NodeID()
	if len(id) != 15 || id[0] != 'n' || id != NodeID(a.Pub()) {
		t.Fatalf("bad id %q", id)
	}
	if strings.Trim(id[1:], "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Fatalf("id must be lowercase base32: %q", id)
	}
	if id == b.NodeID() {
		t.Fatal("different keys must give different ids")
	}
	if h := HubID(a.Pub()); h[0] != 'h' || h[1:] != id[1:] || len(h) != 15 {
		t.Fatalf("hub id %q", h)
	}
}

func TestIdentityEncodeParseAndFile(t *testing.T) {
	a, _ := GenerateIdentity()
	b, err := ParseIdentity(a.Encode())
	if err != nil || b.NodeID() != a.NodeID() || string(b.KX()) != string(a.KX()) {
		t.Fatalf("roundtrip: %v", err)
	}
	if _, err := ParseIdentity("not-base64!"); err == nil {
		t.Fatal("garbage must fail")
	}
	if _, err := ParseIdentity("AAAA"); err == nil {
		t.Fatal("short blob must fail")
	}
	p := filepath.Join(t.TempDir(), "sub", "sync_node.key")
	k1, err := LoadOrCreateIdentity(p, "")
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode %v", st.Mode().Perm())
		}
	}
	k2, err := LoadOrCreateIdentity(p, "")
	if err != nil || k2.NodeID() != k1.NodeID() {
		t.Fatal("the file must be reused")
	}
	// the env blob wins and is never written
	p2 := filepath.Join(t.TempDir(), "x.key")
	k3, err := LoadOrCreateIdentity(p2, a.Encode())
	if err != nil || k3.NodeID() != a.NodeID() {
		t.Fatal("env blob ignored")
	}
	if _, err := os.Stat(p2); err == nil {
		t.Fatal("env key must not be written to disk")
	}
	if _, err := LoadOrCreateIdentity(p2, "garbage"); err == nil {
		t.Fatal("invalid env blob must fail closed")
	}
}

func TestCertSignVerify(t *testing.T) {
	_, hub, _ := ed25519.GenerateKey(rand.Reader)
	hubPub := hub.Public().(ed25519.PublicKey)
	now := time.Now()
	tok := newCert(t, hub, "nabc", now, CertValidity)
	c, err := VerifyCert(hubPub, tok, now)
	if err != nil || c.Subject != "nabc" || c.Ser != "s1" || c.Issuer != HubID(hubPub) || c.Params["branch"] != "B12" {
		t.Fatalf("verify: %v %+v", err, c)
	}
	if d := c.ExpiresAt.Sub(c.IssuedAt.Time); d != CertValidity {
		t.Fatalf("validity %v", d)
	}

	// tampered payload
	parts := strings.Split(tok, ".")
	forged := newCert(t, hub, "nother", now, CertValidity)
	if _, err := VerifyCert(hubPub, parts[0]+"."+strings.Split(forged, ".")[1]+"."+parts[2], now); err == nil {
		t.Fatal("tampered claims must fail")
	}
	// signed by another key
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := VerifyCert(hubPub, newCert(t, other, "nabc", now, time.Hour), now); err == nil {
		t.Fatal("foreign signer must fail")
	}
	// expired
	if _, err := VerifyCert(hubPub, tok, now.Add(CertValidity+time.Minute)); err == nil {
		t.Fatal("expired cert must fail")
	}
	// HS256 / none must not be accepted
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "nabc", "iss": HubID(hubPub), "exp": now.Add(time.Hour).Unix()}).SignedString(hubPub)
	if _, err := VerifyCert(hubPub, hs, now); err == nil {
		t.Fatal("HS256 keyed by the public key must be rejected")
	}
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "nabc", "iss": HubID(hubPub), "exp": now.Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := VerifyCert(hubPub, none, now); err == nil {
		t.Fatal("alg none must be rejected")
	}
	// wrong issuer
	if _, err := VerifyCert(other.Public().(ed25519.PublicKey), tok, now); err == nil {
		t.Fatal("wrong hub key must fail")
	}
	// no subject
	if _, err := VerifyCert(hubPub, newCert(t, hub, "", now, time.Hour), now); err == nil {
		t.Fatal("empty sub must fail")
	}
}

func TestRequestSignature(t *testing.T) {
	id, _ := GenerateIdentity()
	body := []byte(`{"a":1}`)
	const host, hub = "hub.example.com", "habcdefghijklmn"
	sig := SignRequest(id.Ed, "POST", "/api/sync/handshake", host, hub, "1700000000000", "n1", body)
	ok := func(method, path, host, hub, ts, nonce string, b []byte, s string) bool {
		return VerifyRequest(id.Pub(), method, path, host, hub, ts, nonce, b, s)
	}
	if !ok("POST", "/api/sync/handshake", host, hub, "1700000000000", "n1", body, sig) {
		t.Fatal("valid signature rejected")
	}
	// host is normalized: case, default ports and a trailing dot do not matter
	for _, h := range []string{"HUB.example.com", "hub.example.com:443", "hub.example.com."} {
		if !ok("POST", "/api/sync/handshake", h, hub, "1700000000000", "n1", body, sig) {
			t.Fatalf("host %q must normalize to the signed one", h)
		}
	}
	for name, v := range map[string]bool{
		"method": ok("GET", "/api/sync/handshake", host, hub, "1700000000000", "n1", body, sig),
		"path":   ok("POST", "/api/sync/other", host, hub, "1700000000000", "n1", body, sig),
		"host":   ok("POST", "/api/sync/handshake", "evil.example.com", hub, "1700000000000", "n1", body, sig),
		"port":   ok("POST", "/api/sync/handshake", "hub.example.com:8443", hub, "1700000000000", "n1", body, sig),
		"hub id": ok("POST", "/api/sync/handshake", host, "hzzzzzzzzzzzzzz", "1700000000000", "n1", body, sig),
		"ts":     ok("POST", "/api/sync/handshake", host, hub, "1700000000001", "n1", body, sig),
		"nonce":  ok("POST", "/api/sync/handshake", host, hub, "1700000000000", "n2", body, sig),
		"body":   ok("POST", "/api/sync/handshake", host, hub, "1700000000000", "n1", []byte(`{"a":2}`), sig),
		"junk":   ok("POST", "/api/sync/handshake", host, hub, "1700000000000", "n1", body, "###"),
	} {
		if v {
			t.Fatalf("tampered %s accepted", name)
		}
	}
	other, _ := GenerateIdentity()
	if VerifyRequest(other.Pub(), "POST", "/api/sync/handshake", host, hub, "1700000000000", "n1", body, sig) {
		t.Fatal("wrong key accepted")
	}
	// the separator keeps fields apart: moving a byte between ts and nonce changes the digest
	if string(SigningDigest("POST", "/p", "h", "i", "12", "3", nil)) == string(SigningDigest("POST", "/p", "h", "i", "1", "23", nil)) {
		t.Fatal("field boundaries are ambiguous")
	}
	// the canonical form is exactly sha256(METHOD|path|host|hub_id|ts|nonce|hex(sha256(body)))
	bh := sha256.Sum256(body)
	want := sha256.Sum256([]byte("POST|/p|hub.example.com|hX|7|nn|" + hex.EncodeToString(bh[:])))
	if string(SigningDigest("post", "/p", "HUB.example.com:443", "hX", "7", "nn", body)) != string(want[:]) {
		t.Fatal("canonical form changed")
	}
}

func TestServerTimeSignature(t *testing.T) {
	hubPub, hub, _ := ed25519.GenerateKey(nil)
	sig := SignServerTime(hub, "nnode", "100", "nonce1", "2030-01-01T00:00:00.000Z")
	if !VerifyServerTime(hubPub, "nnode", "100", "nonce1", "2030-01-01T00:00:00.000Z", sig) {
		t.Fatal("valid signature rejected")
	}
	for name, v := range map[string]bool{
		"time":  VerifyServerTime(hubPub, "nnode", "100", "nonce1", "2031-01-01T00:00:00.000Z", sig),
		"node":  VerifyServerTime(hubPub, "nother", "100", "nonce1", "2030-01-01T00:00:00.000Z", sig),
		"ts":    VerifyServerTime(hubPub, "nnode", "101", "nonce1", "2030-01-01T00:00:00.000Z", sig),
		"nonce": VerifyServerTime(hubPub, "nnode", "100", "nonce2", "2030-01-01T00:00:00.000Z", sig),
		"junk":  VerifyServerTime(hubPub, "nnode", "100", "nonce1", "2030-01-01T00:00:00.000Z", "###"),
	} {
		if v {
			t.Fatalf("tampered %s accepted", name)
		}
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if VerifyServerTime(otherPub, "nnode", "100", "nonce1", "2030-01-01T00:00:00.000Z", sig) {
		t.Fatal("foreign hub key accepted")
	}
}
