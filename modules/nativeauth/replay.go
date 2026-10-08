//go:build !no_nativeauth

package nativeauth

import (
	"container/list"
	"sync"
	"time"
)

// replayCache remembers used tokens until their expiry (bounded LRU).
type replayCache struct {
	mu  sync.Mutex
	max int
	ll  *list.List
	m   map[string]*list.Element
}

type replayEntry struct {
	key string
	exp time.Time
}

func newReplayCache(max int) *replayCache {
	return &replayCache{max: max, ll: list.New(), m: map[string]*list.Element{}}
}

// markUsed records key until exp. It returns false when key is already present and unexpired.
func (c *replayCache) markUsed(key string, exp, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		if el.Value.(*replayEntry).exp.After(now) {
			return false
		}
		c.ll.Remove(el)
		delete(c.m, key)
	}
	// drop expired entries from the cold end
	for el := c.ll.Back(); el != nil; el = c.ll.Back() {
		if en := el.Value.(*replayEntry); en.exp.After(now) && c.ll.Len() < c.max {
			break
		}
		c.ll.Remove(el)
		delete(c.m, el.Value.(*replayEntry).key)
	}
	c.m[key] = c.ll.PushFront(&replayEntry{key: key, exp: exp})
	return true
}

func (c *replayCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.Remove(el)
		delete(c.m, key)
	}
}
