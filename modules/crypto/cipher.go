//go:build !no_crypto

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"crypto/hkdf"
	"crypto/hmac"
)

// Prefix marks a value produced by this module: `tkc1:<keyver>:<base64 nonce||ct||tag>`.
const Prefix = "tkc1:"

var (
	errNotCiphertext = errors.New("crypto: value is not a ciphertext")
	errKeyVersion    = errors.New("crypto: key version unavailable (retired or unknown)")
)

func aadFor(collectionId, field, recordId string) []byte {
	return []byte(collectionId + "\x00" + field + "\x00" + recordId)
}

func gcm(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// sealRaw returns base64(nonce||ct||tag).
func sealRaw(key, aad, plain []byte) (string, error) {
	g, err := gcm(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := g.Seal(nonce, nonce, plain, aad)
	return base64.StdEncoding.EncodeToString(out), nil
}

func openRaw(key, aad []byte, b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	g, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < g.NonceSize()+g.Overhead() {
		return nil, errors.New("crypto: ciphertext too short")
	}
	return g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], aad)
}

// seal produces the stored form of a value.
func seal(key []byte, ver int, aad, plain []byte) (string, error) {
	b, err := sealRaw(key, aad, plain)
	if err != nil {
		return "", err
	}
	return Prefix + strconv.Itoa(ver) + ":" + b, nil
}

// parseCT splits a stored ciphertext into its key version and base64 body.
func parseCT(s string) (ver int, body string, ok bool) {
	if !strings.HasPrefix(s, Prefix) {
		return 0, "", false
	}
	rest := s[len(Prefix):]
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return 0, "", false
	}
	v, err := strconv.Atoi(rest[:i])
	if err != nil || v <= 0 {
		return 0, "", false
	}
	return v, rest[i+1:], true
}

// IsCiphertext reports whether s has the ciphertext format.
func IsCiphertext(s string) bool {
	_, _, ok := parseCT(s)
	return ok
}

// open decrypts s; keyFor returns the DEK of a version.
func open(s string, aad []byte, keyFor func(ver int) ([]byte, error)) ([]byte, error) {
	ver, body, ok := parseCT(s)
	if !ok {
		return nil, errNotCiphertext
	}
	k, err := keyFor(ver)
	if err != nil {
		return nil, err
	}
	return openRaw(k, aad, body)
}

// indexKey derives the per-collection HMAC key from a DEK.
func indexKey(dek []byte, collectionId string) ([]byte, error) {
	return hkdf.Key(sha256.New, dek, []byte("tokibase-crypto-v1"), "blind-index:"+collectionId, 32)
}

func blindHMAC(ik []byte, field, value string) string {
	h := hmac.New(sha256.New, ik)
	h.Write([]byte(field))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return base64.RawStdEncoding.EncodeToString(h.Sum(nil))
}

// ----- value helpers: text columns store the string, json columns a JSON string -----

// ctOf extracts the ciphertext from a stored column value.
func ctOf(stored string, isJSON bool) (string, bool) {
	if isJSON && strings.HasPrefix(stored, `"`) {
		var s string
		if json.Unmarshal([]byte(stored), &s) != nil {
			return "", false
		}
		stored = s
	}
	return stored, IsCiphertext(stored)
}

// storedFromCT is the column value that holds ct.
func storedFromCT(ct string, isJSON bool) string {
	if isJSON {
		b, _ := json.Marshal(ct)
		return string(b)
	}
	return ct
}

func emptyValue(s string, isJSON bool) bool {
	if s == "" {
		return true
	}
	return isJSON && (s == "null" || s == `""`)
}

func fmtVer(v int) string { return fmt.Sprintf("v%d", v) }

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := io.ReadFull(rand.Reader, b)
	return b, err
}

func b64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func b64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func timeNow() time.Time { return time.Now().UTC() }
