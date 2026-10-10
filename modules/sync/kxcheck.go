//go:build !no_sync

package sync

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"slices"
)

// lowOrderX25519 lists the encodings of the points of small order on Curve25519
// (RFC 7748 §6.1 and the table of Bernstein's "Curve25519" note), with bit 255
// cleared as the function ignores it. A node key like that makes every shared
// secret of the hub constant (all zero), so the wrapped data keys would be
// readable by anyone. crypto/ecdh already rejects an all-zero result; the
// explicit list rejects the key at enrollment, before any certificate is issued.
var lowOrderX25519 = func() [][]byte {
	var out [][]byte
	for _, h := range []string{
		"0000000000000000000000000000000000000000000000000000000000000000",
		"0100000000000000000000000000000000000000000000000000000000000000",
		"e0eb7a7c3b41b8ae1656e3faf19fc46ada098deb9c32b1fd866205165f49b800",
		"5f9c95bca3508c24b1d0b1559c83ef5b04445cc4581c8e86d8224eddd09f1157",
		"ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"edffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
		"eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f",
	} {
		b, _ := hex.DecodeString(h)
		out = append(out, b)
	}
	return out
}()

// validX25519Pub reports whether b is a usable X25519 public key: 32 bytes, not
// a point of small order (the identity included), and one that produces a
// non-zero shared secret with a fresh key.
func validX25519Pub(b []byte) bool {
	if len(b) != 32 {
		return false
	}
	masked := slices.Clone(b)
	masked[31] &= 0x7f
	for _, lo := range lowOrderX25519 {
		if bytes.Equal(masked, lo) {
			return false
		}
	}
	pub, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		return false
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return false
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return false
	}
	return !bytes.Equal(shared, make([]byte, 32))
}
