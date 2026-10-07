// Package push delivers push notifications to FCM (HTTP v1) and APNs
// (token based auth) with a device registry, topics and delivery through the
// kernel job queue (kind "push.send").
package push

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// Platforms.
const (
	PlatformFCM  = "fcm"
	PlatformAPNs = "apns"
)

// Priorities.
const (
	PriorityHigh   = "high"
	PriorityNormal = "normal"
)

// Notification is the provider independent content of one push.
type Notification struct {
	Title       string            `json:"title,omitempty"`
	Body        string            `json:"body,omitempty"`
	Data        map[string]string `json:"data,omitempty"`
	TTLSeconds  int               `json:"ttl_seconds,omitempty"`
	CollapseKey string            `json:"collapse_key,omitempty"`
	Priority    string            `json:"priority,omitempty"` // high (default) | normal
}

// Provider sends one notification to one device token.
//
// Implementations return nil on success, [ErrInvalidToken] (possibly wrapped)
// when the token is dead, a [*RetryableError] for 429/5xx/auth refresh cases,
// a [*PermanentError] for rejections that a retry cannot fix, or any other
// error (network failures), which is retried.
type Provider interface {
	Name() string
	Send(ctx context.Context, n *Notification, token string) error
}

// ErrInvalidToken marks a token the provider no longer accepts; the device is disabled.
var ErrInvalidToken = errors.New("push: invalid or unregistered device token")

// RetryableError asks the job queue to retry later (rate limit, server error).
type RetryableError struct {
	Err        error
	RetryAfter time.Duration // informational; the queue applies its own backoff
}

func (e *RetryableError) Error() string { return "push: retryable: " + e.Err.Error() }
func (e *RetryableError) Unwrap() error { return e.Err }

// PermanentError is a rejection that will not succeed on retry (bad payload, bad config).
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "push: rejected: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

func retryable(format string, a ...any) error {
	return &RetryableError{Err: fmt.Errorf(format, a...)}
}

func permanent(format string, a ...any) error {
	return &PermanentError{Err: fmt.Errorf(format, a...)}
}

// FakeProvider records sends in memory (tests, `toki push test`).
type FakeProvider struct {
	mu   sync.Mutex
	sent []FakeSend
	// Errors maps a token to the error Send returns for it.
	Errors map[string]error
	Label  string
}

// FakeSend is one recorded send.
type FakeSend struct {
	Token string
	N     Notification
}

// NewFake returns an empty fake provider.
func NewFake(label string) *FakeProvider {
	return &FakeProvider{Label: label, Errors: map[string]error{}}
}

func (f *FakeProvider) Name() string {
	if f.Label == "" {
		return "fake"
	}
	return f.Label
}

func (f *FakeProvider) Send(_ context.Context, n *Notification, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Errors[token]; err != nil {
		return err
	}
	f.sent = append(f.sent, FakeSend{Token: token, N: *n})
	return nil
}

// SetError makes Send return err for token (nil clears it).
func (f *FakeProvider) SetError(token string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.Errors, token)
		return
	}
	f.Errors[token] = err
}

// Sent returns a copy of the recorded sends.
func (f *FakeProvider) Sent() []FakeSend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeSend(nil), f.sent...)
}

// ---- shared helpers ----

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jwtSegments(header, claims any) (string, error) {
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	return b64(h) + "." + b64(c), nil
}

// signRS256 returns a compact JWT signed with RSASSA-PKCS1-v1_5 SHA-256.
func signRS256(key *rsa.PrivateKey, header, claims any) (string, error) {
	signing, err := jwtSegments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// signES256 returns a compact JWT signed with ECDSA P-256 (r||s, 64 bytes).
func signES256(key *ecdsa.PrivateKey, header, claims any) (string, error) {
	signing, err := jwtSegments(header, claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64(sig), nil
}

func verifyES256(pub *ecdsa.PublicKey, token string) bool {
	var parts [3]string
	n, start := 0, 0
	for i := 0; i <= len(token); i++ {
		if i == len(token) || token[i] == '.' {
			if n > 2 {
				return false
			}
			parts[n] = token[start:i]
			n++
			start = i + 1
		}
	}
	if n != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}

func parsePEMKey(raw []byte) (any, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("no PEM block found")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("unsupported private key format")
}
