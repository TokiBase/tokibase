package proto

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Identity is the key material of a spoke: an Ed25519 signing key and an
// X25519 key-exchange key.
type Identity struct {
	Ed ed25519.PrivateKey
	X  *ecdh.PrivateKey
}

// DeriveID is prefix + base32(sha256(pub))[:14], lowercase, 15 chars in all
// (the length of a PocketBase record id).
func DeriveID(prefix string, pub []byte) string {
	h := sha256.Sum256(pub)
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(h[:]))[:14]
}

// NodeID is the id bound to a node key ("n" + 14 chars).
func NodeID(pub []byte) string { return DeriveID("n", pub) }

// HubID is the id bound to the hub key ("h" + 14 chars).
func HubID(pub []byte) string { return DeriveID("h", pub) }

// GenerateIdentity creates fresh keys.
func GenerateIdentity() (*Identity, error) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Ed: ed, X: x}, nil
}

// Encode is base64(ed25519 seed (32) || x25519 private (32)).
func (i *Identity) Encode() string {
	b := append(append([]byte{}, i.Ed.Seed()...), i.X.Bytes()...)
	return base64.StdEncoding.EncodeToString(b)
}

// ParseIdentity is the inverse of Encode.
func ParseIdentity(blob string) (*Identity, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		return nil, fmt.Errorf("sync node key: %w", err)
	}
	if len(b) != ed25519.SeedSize+32 {
		return nil, errors.New("sync node key: wrong length")
	}
	x, err := ecdh.X25519().NewPrivateKey(b[ed25519.SeedSize:])
	if err != nil {
		return nil, fmt.Errorf("sync node key: %w", err)
	}
	return &Identity{Ed: ed25519.NewKeyFromSeed(b[:ed25519.SeedSize]), X: x}, nil
}

// LoadOrCreateIdentity returns the key from envBlob when set (never written to
// disk), else from the file at path, creating it with mode 0600 on first use.
func LoadOrCreateIdentity(path, envBlob string) (*Identity, error) {
	if strings.TrimSpace(envBlob) != "" {
		return ParseIdentity(envBlob)
	}
	if b, err := os.ReadFile(path); err == nil {
		return ParseIdentity(string(b))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	id, err := GenerateIdentity()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) { // lost a race with another process
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, rerr
			}
			return ParseIdentity(string(b))
		}
		return nil, err
	}
	_, werr := f.WriteString(id.Encode() + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
		return nil, werr
	}
	return id, nil
}

// Pub is the Ed25519 public key.
func (i *Identity) Pub() ed25519.PublicKey { return i.Ed.Public().(ed25519.PublicKey) }

// KX is the X25519 public key.
func (i *Identity) KX() []byte { return i.X.PublicKey().Bytes() }

// NodeID is the id derived from the signing key.
func (i *Identity) NodeID() string { return NodeID(i.Pub()) }

// SigningDigest is sha256(method|path|ts|nonce|sha256(body)), the message of a
// signed request (docs/SYNC_DESIGN.md §1.5). The body hash is hex.
func SigningDigest(method, path, ts, nonce string, body []byte) []byte {
	bh := sha256.Sum256(body)
	h := sha256.Sum256([]byte(strings.ToUpper(method) + "|" + path + "|" + ts + "|" + nonce + "|" + hex.EncodeToString(bh[:])))
	return h[:]
}

// SignRequest returns the base64 signature header value.
func SignRequest(priv ed25519.PrivateKey, method, path, ts, nonce string, body []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SigningDigest(method, path, ts, nonce, body)))
}

// VerifyRequest checks a signature header value.
func VerifyRequest(pub ed25519.PublicKey, method, path, ts, nonce string, body []byte, sig string) bool {
	if len(pub) != ed25519.PublicKeySize {
		return false
	}
	s, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(s) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(pub, SigningDigest(method, path, ts, nonce, body), s)
}

// NewNonce returns a random request nonce.
func NewNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// CertValidity is the lifetime of a device certificate.
const CertValidity = 365 * 24 * time.Hour

// CertClaims are the claims of a device certificate (§1.4): iss=hub id,
// sub=node id, pub/kx=base64 public keys, params=partition params, ser=serial.
type CertClaims struct {
	jwt.RegisteredClaims
	Pub    string         `json:"pub"`
	KX     string         `json:"kx"`
	Params map[string]any `json:"params,omitempty"`
	Ser    string         `json:"ser"`
}

// SignCert issues a device certificate (compact JWS, EdDSA).
func SignCert(hubPriv ed25519.PrivateKey, c *CertClaims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, c).SignedString(hubPriv)
}

// VerifyCert checks the signature (EdDSA only), issuer, expiry and returns the
// claims. now is the verification time.
func VerifyCert(hubPub ed25519.PublicKey, token string, now time.Time) (*CertClaims, error) {
	if len(hubPub) != ed25519.PublicKeySize {
		return nil, errors.New("bad hub key")
	}
	c := &CertClaims{}
	_, err := jwt.ParseWithClaims(token, c, func(*jwt.Token) (any, error) { return hubPub, nil },
		jwt.WithValidMethods([]string{"EdDSA"}),
		jwt.WithTimeFunc(func() time.Time { return now }),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer(HubID(hubPub)),
	)
	if err != nil {
		return nil, err
	}
	if c.Subject == "" {
		return nil, errors.New("certificate without subject")
	}
	return c, nil
}
