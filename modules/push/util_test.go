//go:build !no_push

package push

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
)

func verifyPKCS1(pub *rsa.PublicKey, signing string, sig []byte) error {
	sum := sha256.Sum256([]byte(signing))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
}
