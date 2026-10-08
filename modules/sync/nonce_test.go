//go:build !no_sync

package sync

import (
	"strconv"
	"testing"
	"time"
)

func TestNonceCache(t *testing.T) {
	var c nonceCache
	t0 := time.Unix(1_700_000_000, 0)
	if !c.Use("n1", "a", t0) {
		t.Fatal("first use must pass")
	}
	if c.Use("n1", "a", t0.Add(time.Minute)) {
		t.Fatal("replay must be rejected")
	}
	if !c.Use("n2", "a", t0) {
		t.Fatal("the nonce is per node")
	}
	if c.Use("n1", "a", t0.Add(nonceTTL)) {
		t.Fatal("still inside the ttl at exactly 10 min")
	}
	if !c.Use("n1", "a", t0.Add(nonceTTL+2*time.Second)) {
		t.Fatal("expired nonces are forgotten")
	}
}

func TestNonceCacheIsBounded(t *testing.T) {
	var c nonceCache
	now := time.Unix(1_700_000_000, 0)
	for i := 0; i < nonceMax+100; i++ {
		c.Use("n", strconv.Itoa(i), now)
	}
	if c.ll.Len() != nonceMax || len(c.m) != nonceMax {
		t.Fatalf("size %d/%d", c.ll.Len(), len(c.m))
	}
	// the oldest were evicted, the newest are kept
	if !c.Use("n", "0", now) {
		t.Fatal("oldest entry must be evicted")
	}
	if c.Use("n", strconv.Itoa(nonceMax+99), now) {
		t.Fatal("newest entry must be kept")
	}
}
