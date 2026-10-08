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
	jwksHardMaxAge  = 24 * time.Hour // beyond this a failing refresh no longer serves cached keys
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
	okAt      time.Time     // last successful fetch
	fetchedAt time.Time     // last attempt (successful or not)
	inflight  chan struct{} // non-nil while a fetch runs (single flight)
}

func newJWKS(url string, now func() time.Time) *jwks {
	return &jwks{url: url, now: now, client: &http.Client{Timeout: jwksTimeout}}
}

// key returns the key for kid. The network fetch runs outside the mutex and is
// shared by concurrent callers. Keys older than jwksMaxAge are refreshed; if
// the refresh keeps failing they are still served until jwksHardMaxAge, after
// which verification fails closed.
func (j *jwks) key(kid string) (crypto.PublicKey, error) {
	for i := 0; i < 3; i++ {
		j.mu.Lock()
		now := j.now()
		k, ok := j.keys[kid]
		age := now.Sub(j.okAt)
		if ok && age <= jwksMaxAge {
			j.mu.Unlock()
			return k, nil
		}
		if ch := j.inflight; ch != nil {
			j.mu.Unlock()
			<-ch
			continue
		}
		if !j.fetchedAt.IsZero() && now.Sub(j.fetchedAt) < jwksMinRefresh {
			j.mu.Unlock()
			if ok && age <= jwksHardMaxAge {
				return k, nil
			}
			if ok {
				return nil, errors.New("jwks: cached keys expired and the refresh failed")
			}
			return nil, errors.New("jwks: unknown key id")
		}
		j.fetchedAt = now
		ch := make(chan struct{})
		j.inflight = ch
		j.mu.Unlock()

		keys, err := j.fetch()

		j.mu.Lock()
		if err == nil {
			j.keys, j.okAt = keys, now
		}
		j.inflight = nil
		j.mu.Unlock()
		close(ch)
	}
	return nil, errors.New("jwks: unavailable")
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
