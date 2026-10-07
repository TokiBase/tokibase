//go:build !no_totp

package totp

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; SHA-1 in HMAC is not weakened by collisions
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	digits = 6
	period = 30
	// window is the accepted clock skew in steps (+-1 step = +-30 s).
	window = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// hotp computes RFC 4226 for the given counter (SHA-1, 6 digits).
func hotp(secret []byte, counter uint64) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret) //nolint:gosec
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) | (uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	var out [digits]byte
	for i := digits - 1; i >= 0; i-- {
		out[i] = byte('0' + v%10)
		v /= 10
	}
	return string(out[:])
}

// Code returns the RFC 6238 code of secret for unix time ts.
func Code(secret []byte, ts int64) string { return hotp(secret, uint64(ts/period)) }

// match checks code against the window around ts (constant time per
// candidate) and returns the matching counter, or false.
func match(secret []byte, code string, ts int64) (uint64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != digits {
		return 0, false
	}
	cur := ts / period
	var found uint64
	ok := 0
	for d := int64(-window); d <= window; d++ {
		c := cur + d
		if c < 0 {
			continue
		}
		eq := subtle.ConstantTimeCompare([]byte(hotp(secret, uint64(c))), []byte(code))
		if eq == 1 {
			found = uint64(c)
		}
		ok |= eq
	}
	return found, ok == 1
}

func newSecret() ([]byte, error) {
	s := make([]byte, 20)
	_, err := rand.Read(s)
	return s, err
}

// ---- secret encryption (AES-256-GCM, key from TOKI_TOTP_KEY or the app encryption env) ----

func seal(key []byte, plain string) (string, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "v1:" + base64.RawStdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func open(key []byte, sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, "v1:")
	if !ok {
		return "", errors.New("totp: unknown secret format")
	}
	b, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return "", err
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	if len(b) < gcm.NonceSize() {
		return "", errors.New("totp: short ciphertext")
	}
	p, err := gcm.Open(nil, b[:gcm.NonceSize()], b[gcm.NonceSize():], nil)
	return string(p), err
}

// ---- recovery codes ----

const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no i, l, o, 0, 1
const recoveryLen = 10
const recoveryCount = 10

func newRecoveryCodes() (plain, hashes []string, err error) {
	for i := 0; i < recoveryCount; i++ {
		b := make([]byte, recoveryLen)
		for j := range b {
			// rejection sampling to avoid modulo bias
			for {
				var x [1]byte
				if _, err := rand.Read(x[:]); err != nil {
					return nil, nil, err
				}
				if int(x[0]) < 256-256%len(recoveryAlphabet) {
					b[j] = recoveryAlphabet[int(x[0])%len(recoveryAlphabet)]
					break
				}
			}
		}
		plain = append(plain, string(b))
		hashes = append(hashes, hashRecovery(string(b)))
	}
	return
}

func normRecovery(c string) string {
	return strings.ToLower(strings.NewReplacer(" ", "", "-", "").Replace(strings.TrimSpace(c)))
}

func hashRecovery(c string) string {
	h := sha256.Sum256([]byte(normRecovery(c)))
	return fmt.Sprintf("%x", h[:])
}
