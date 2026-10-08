//go:build !no_scanner

package scanner

import "container/list"

// lru is a small bounded map that evicts the least recently used key.
// It is not safe for concurrent use; the module serializes access.
type lru[V any] struct {
	cap   int
	ll    *list.List
	items map[string]*list.Element
}

type lruItem[V any] struct {
	key string
	val V
}

func newLRU[V any](capacity int) *lru[V] {
	return &lru[V]{cap: capacity, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *lru[V]) get(key string) (V, bool) {
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruItem[V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lru[V]) put(key string, val V) {
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem[V]).val = val
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&lruItem[V]{key, val})
	for c.ll.Len() > c.cap {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*lruItem[V]).key)
	}
}

func (c *lru[V]) len() int { return c.ll.Len() }
