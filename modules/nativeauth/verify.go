//go:build !no_nativeauth

package nativeauth

import (
	"crypto"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// verified is the subset of a verified ID token the account mapping needs.
type verified struct {
	Sub           string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
	Nonce         string
	NonceUnsup    bool // apple: nonce_supported == false
	Exp           time.Time
	ReplayKey     string
}

func issuerOK(provider, iss string) bool {
	switch provider {
	case "google":
		return iss == "accounts.google.com" || iss == "https://accounts.google.com"
	case "apple":
		return iss == "https://appleid.apple.com"
	}
	return false
}

func boolClaim(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	}
	return false
}

func strClaim(c jwt.MapClaims, k string) string {
	s, _ := c[k].(string)
	return s
}

// verifyToken checks signature (RS256/ES256), iss, aud, exp/iat/nbf (60 s skew).
// Nonce and replay are checked by the caller.
func (m *Module) verifyToken(provider, raw string, audiences []string) (*verified, error) {
	if len(audiences) == 0 {
		return nil, errors.New("no audience configured")
	}
	set := m.keys[provider]
	if set == nil {
		return nil, errors.New("unsupported provider")
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256", "ES256"}),
		jwt.WithLeeway(clockSkew),
		jwt.WithTimeFunc(m.now),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithStrictDecoding(),
	)
	if err := canonicalJWT(raw); err != nil {
		return nil, err
	}
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid")
		}
		var k crypto.PublicKey
		k, err := set.key(kid)
		if err != nil {
			return nil, err
		}
		return k, nil
	})
	if err != nil {
		return nil, err
	}
	iss, _ := claims.GetIssuer()
	if !issuerOK(provider, iss) {
		return nil, errors.New("invalid issuer")
	}
	auds, _ := claims.GetAudience()
	okAud := false
	for _, a := range auds {
		for _, want := range audiences {
			if a == want {
				okAud = true
			}
		}
	}
	if !okAud {
		return nil, errors.New("invalid audience")
	}
	sub := strClaim(claims, "sub")
	if sub == "" || len(sub) > 255 {
		return nil, errors.New("missing subject")
	}
	exp, _ := claims.GetExpirationTime()
	v := &verified{
		Sub:     sub,
		Email:   strings.TrimSpace(strClaim(claims, "email")),
		Name:    strClaim(claims, "name"),
		Picture: strClaim(claims, "picture"),
		Nonce:   strClaim(claims, "nonce"),
		Exp:     exp.Time,
	}
	// Google states email_verified explicitly. Apple only issues verified emails
	// (bool or "true"); a missing claim counts as verified only for Apple.
	if ev, has := claims["email_verified"]; has {
		v.EmailVerified = boolClaim(ev)
	} else {
		v.EmailVerified = provider == "apple"
	}
	if ns, has := claims["nonce_supported"]; has && !boolClaim(ns) {
		v.NonceUnsup = true
	}
	if jti := strClaim(claims, "jti"); jti != "" {
		v.ReplayKey = provider + ":jti:" + jti
	} else {
		// key on the signed content (header.payload) only: the signature segment
		// is malleable (base64 tail bits, ECDSA s -> n-s)
		i := strings.LastIndexByte(raw, '.')
		h := sha256.Sum256([]byte(raw[:i]))
		v.ReplayKey = provider + ":h:" + hex.EncodeToString(h[:])
	}
	return v, nil
}

// canonicalJWT rejects a compact token whose segments are not canonical
// unpadded base64url (padding, stray characters, non-zero trailing bits), so
// one token has exactly one string form.
func canonicalJWT(raw string) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return errors.New("malformed token")
	}
	for _, p := range parts {
		if p == "" {
			return errors.New("malformed token")
		}
		b, err := base64.RawURLEncoding.Strict().DecodeString(p)
		if err != nil || base64.RawURLEncoding.EncodeToString(b) != p {
			return errors.New("non-canonical token encoding")
		}
	}
	return nil
}

// checkNonce compares the request nonce (the RAW value the app generated) with
// the token claim. The comparison depends on the provider and never accepts the
// claim as its own proof:
//   - apple: the claim must be the lowercase-hex SHA-256 of the raw nonce
//     (what sign_in_with_apple is given as `nonce`);
//   - google: the claim must equal the raw nonce (google_sign_in `nonce`).
//
// A holder of the token can read the claim, but for Apple it cannot derive the
// raw value from it. For Google the claim IS the raw value, so there the nonce
// only binds the token to the app instance that created it.
func checkNonce(provider string, v *verified, reqNonce string) error {
	if reqNonce == "" {
		if v.Nonce != "" {
			return errors.New("token carries a nonce but none was submitted")
		}
		return nil
	}
	if v.Nonce == "" {
		if v.NonceUnsup {
			return nil
		}
		return errors.New("token has no nonce")
	}
	want := reqNonce
	if provider == "apple" {
		h := sha256.Sum256([]byte(reqNonce))
		want = hex.EncodeToString(h[:])
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(v.Nonce)), []byte(want)) == 1 {
			return nil
		}
		return errors.New("nonce mismatch")
	}
	if subtle.ConstantTimeCompare([]byte(v.Nonce), []byte(want)) == 1 {
		return nil
	}
	return errors.New("nonce mismatch")
}
