package security

import (
	cryptoRand "crypto/rand"
	"math/big"
	mathRand "math/rand/v2"
	"sync"
	"sync/atomic"
)

// pseudorandomIntN is the source used by [PseudorandomStringWithAlphabet]
// (a seam to allow deterministic output in tests of dependent packages).
var pseudorandomIntN atomic.Pointer[func(int) int]

func init() {
	f := mathRand.IntN
	pseudorandomIntN.Store(&f)
}

// SeedPseudorandomForTest makes [PseudorandomString] deterministic
// (a seeded PCG source) until the returned restore func is called.
//
// It is intended ONLY for tests that need to compare two code paths
// byte-for-byte; the caller must not run other goroutines that consume
// pseudorandom strings meanwhile.
func SeedPseudorandomForTest(seed uint64) (restore func()) {
	var mu sync.Mutex
	r := mathRand.New(mathRand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	f := func(n int) int {
		mu.Lock()
		defer mu.Unlock()
		return r.IntN(n)
	}

	prev := pseudorandomIntN.Load()
	pseudorandomIntN.Store(&f)

	return func() { pseudorandomIntN.Store(prev) }
}

const defaultRandomAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// RandomString generates a cryptographically random string with the specified length.
//
// The generated string matches [A-Za-z0-9]+ and it's transparent to URL-encoding.
func RandomString(length int) string {
	return RandomStringWithAlphabet(length, defaultRandomAlphabet)
}

// RandomStringWithAlphabet generates a cryptographically random string
// with the specified length and characters set.
//
// It panics if for some reason rand.Int returns a non-nil error.
func RandomStringWithAlphabet(length int, alphabet string) string {
	b := make([]byte, length)
	max := big.NewInt(int64(len(alphabet)))

	for i := range b {
		n, err := cryptoRand.Int(cryptoRand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[n.Int64()]
	}

	return string(b)
}

// PseudorandomString generates a pseudorandom string with the specified length.
//
// The generated string matches [A-Za-z0-9]+ and it's transparent to URL-encoding.
//
// For a cryptographically random string (but a little bit slower) use RandomString instead.
func PseudorandomString(length int) string {
	return PseudorandomStringWithAlphabet(length, defaultRandomAlphabet)
}

// PseudorandomStringWithAlphabet generates a pseudorandom string
// with the specified length and characters set.
//
// For a cryptographically random (but a little bit slower) use RandomStringWithAlphabet instead.
func PseudorandomStringWithAlphabet(length int, alphabet string) string {
	b := make([]byte, length)
	max := len(alphabet)

	intN := pseudorandomIntN.Load()

	for i := range b {
		b[i] = alphabet[(*intN)(max)]
	}

	return string(b)
}
