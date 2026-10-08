//go:build !no_sync

package sync

import (
	"container/list"
	stdsync "sync"
	"time"
)

const (
	nonceTTL = 10 * time.Minute
	nonceMax = 50000
)

// nonceCache remembers signed-request nonces for nonceTTL. It is a bounded
// LRU: when full the oldest entry is evicted. It lives in memory, so a hub
// restart forgets nonces; the +-5 min timestamp window still bounds a replay.
type nonceCache struct {
	mu stdsync.Mutex
	ll *list.List // front = newest; values *nonceEntry
	m  map[string]*list.Element
}

type nonceEntry struct {
	key string
	at  time.Time
}

// Use records the nonce of a node and reports false when it was already used
// within the TTL.
func (c *nonceCache) Use(node, nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ll == nil {
		c.ll, c.m = list.New(), map[string]*list.Element{}
	}
	// expire from the back (oldest first)
	for e := c.ll.Back(); e != nil; e = c.ll.Back() {
		if now.Sub(e.Value.(*nonceEntry).at) <= nonceTTL {
			break
		}
		delete(c.m, e.Value.(*nonceEntry).key)
		c.ll.Remove(e)
	}
	k := node + "|" + nonce
	if _, ok := c.m[k]; ok {
		return false
	}
	c.m[k] = c.ll.PushFront(&nonceEntry{key: k, at: now})
	for c.ll.Len() > nonceMax {
		e := c.ll.Back()
		delete(c.m, e.Value.(*nonceEntry).key)
		c.ll.Remove(e)
	}
	return true
}
