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

func TestNonceCacheIsBoundedPerNode(t *testing.T) {
	var c nonceCache
	now := time.Unix(1_700_000_000, 0)
	if !c.Use("victim", "v-1", now) {
		t.Fatal("first use must pass")
	}
	// one node floods: only its own entries are evicted
	for i := 0; i < nonceMaxPerNode*4; i++ {
		c.Use("flood", strconv.Itoa(i), now)
	}
	if got := c.size("flood"); got != nonceMaxPerNode {
		t.Fatalf("flooding node holds %d entries, want %d", got, nonceMaxPerNode)
	}
	if c.Use("victim", "v-1", now) {
		t.Fatal("a flooding node must not evict the entries of another node (replay)")
	}
	// the oldest of the flooding node are gone, the newest are kept
	if !c.Use("flood", "0", now) {
		t.Fatal("oldest entry of the flooding node must be evicted")
	}
	if c.Use("flood", strconv.Itoa(nonceMaxPerNode*4-1), now) {
		t.Fatal("newest entry must be kept")
	}
}
