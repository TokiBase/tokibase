//go:build !no_nativeauth

package nativeauth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

const (
	jwksTimeout     = 5 * time.Second
	jwksMinRefresh  = time.Minute
	jwksMaxAge      = 6 * time.Hour
	jwksMaxBodySize = 1 << 20
)

// jwks is a kid-indexed JWKS cache. A refresh happens when the kid is unknown
// or the cache is older than jwksMaxAge, at most once per jwksMinRefresh.
type jwks struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	keys      map[string]crypto.PublicKey
	fetchedAt time.Time // last attempt (successful or not)
}

func newJWKS(url string, now func() time.Time) *jwks {
	return &jwks{url: url, now: now, client: &http.Client{Timeout: jwksTimeout}}
}

func (j *jwks) key(kid string) (crypto.PublicKey, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.now()
	k, ok := j.keys[kid]
	stale := j.keys == nil || now.Sub(j.fetchedAt) > jwksMaxAge
	if ok && !stale {
		return k, nil
	}
	if (j.keys == nil || !ok || stale) && (j.fetchedAt.IsZero() || now.Sub(j.fetchedAt) >= jwksMinRefresh) {
		j.fetchedAt = now
		if keys, err := j.fetch(); err == nil {
			j.keys = keys
			k, ok = keys[kid]
		} else if !ok {
			return nil, fmt.Errorf("jwks: %w", err)
		}
	}
	if !ok {
		return nil, errors.New("jwks: unknown key id")
	}
	return k, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (j *jwks) fetch() (map[string]crypto.PublicKey, error) {
	res, err := j.client.Get(j.url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, jwksMaxBodySize)).Decode(&doc); err != nil {
		return nil, err
	}
	out := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kid == "" {
			continue
		}
		if pub, err := k.public(); err == nil {
			out[k.Kid] = pub
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no usable keys")
	}
	return out, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func (k jwk) public() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		n, err := b64(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64(k.E)
		if err != nil {
			return nil, err
		}
		ei := new(big.Int).SetBytes(e)
		if !ei.IsInt64() || ei.Int64() < 3 || ei.Int64() > 1<<31-1 {
			return nil, errors.New("bad exponent")
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(ei.Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("rsa key too small")
		}
		return pub, nil
	case "EC":
		if k.Crv != "P-256" {
			return nil, errors.New("unsupported curve")
		}
		x, err := b64(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
			return nil, errors.New("point not on curve")
		}
		return pub, nil
	}
	return nil, errors.New("unsupported key type")
}
