//go:build !no_nativeauth

package nativeauth

import (
	"crypto"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
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
	)
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
		h := sha256.Sum256([]byte(raw))
		v.ReplayKey = provider + ":h:" + hex.EncodeToString(h[:])
	}
	return v, nil
}

// checkNonce compares the request nonce with the token claim. The claim may be
// the nonce itself (Google) or its hex SHA-256 (the usual Apple pattern).
func checkNonce(v *verified, reqNonce string) error {
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
	h := sha256.Sum256([]byte(reqNonce))
	if subtle.ConstantTimeCompare([]byte(v.Nonce), []byte(reqNonce)) == 1 ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(v.Nonce)), []byte(hex.EncodeToString(h[:]))) == 1 {
		return nil
	}
	return fmt.Errorf("nonce mismatch")
}
