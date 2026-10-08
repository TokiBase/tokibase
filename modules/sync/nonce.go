//go:build !no_sync

package sync

import (
	"container/list"
	stdsync "sync"
	"time"
)

const (
	nonceTTL = 10 * time.Minute
	// nonceMaxPerNode bounds the live nonces of one node. Only a node with a
	// valid signature can insert, so one node can evict nothing but its own
	// entries.
	nonceMaxPerNode = 256
	// nonceMaxNodes bounds the number of nodes with live nonces.
	nonceMaxNodes = 20000
)

// nonceCache remembers signed-request nonces per node for nonceTTL. It lives
// in memory; the persisted per-node timestamp floor (`_sync_nodes.sig_ts_floor`)
// is what stops a replay after a restart or a failover.
type nonceCache struct {
	mu    stdsync.Mutex
	nodes map[string]*nodeNonces
}

type nodeNonces struct {
	ll *list.List // front = newest; values *nonceEntry
	m  map[string]*list.Element
}

type nonceEntry struct {
	nonce string
	at    time.Time
}

func (n *nodeNonces) expire(now time.Time) {
	for e := n.ll.Back(); e != nil; e = n.ll.Back() {
		if now.Sub(e.Value.(*nonceEntry).at) <= nonceTTL {
			break
		}
		delete(n.m, e.Value.(*nonceEntry).nonce)
		n.ll.Remove(e)
	}
}

// Use records the nonce of a node and reports false when it was already used
// within the TTL.
func (c *nonceCache) Use(node, nonce string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nodes == nil {
		c.nodes = map[string]*nodeNonces{}
	}
	n := c.nodes[node]
	if n == nil {
		if len(c.nodes) >= nonceMaxNodes {
			for k, v := range c.nodes {
				v.expire(now)
				if v.ll.Len() == 0 {
					delete(c.nodes, k)
				}
			}
		}
		n = &nodeNonces{ll: list.New(), m: map[string]*list.Element{}}
		c.nodes[node] = n
	}
	n.expire(now)
	if _, ok := n.m[nonce]; ok {
		return false
	}
	n.m[nonce] = n.ll.PushFront(&nonceEntry{nonce: nonce, at: now})
	for n.ll.Len() > nonceMaxPerNode {
		e := n.ll.Back()
		delete(n.m, e.Value.(*nonceEntry).nonce)
		n.ll.Remove(e)
	}
	return true
}

// size is the number of live entries of a node (tests).
func (c *nonceCache) size(node string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.nodes[node]; n != nil {
		return n.ll.Len()
	}
	return 0
}
