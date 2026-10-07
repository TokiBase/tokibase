//go:build !no_mcp

package mcp

import (
	"sync"
	"time"
)

// bucket is a token bucket: capacity = per-minute rate, refilled continuously.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	init   bool
}

// allow takes one token; perMin <= 0 falls back to DefaultRatePerMin.
func (b *bucket) allow(now time.Time, perMin int) bool {
	if perMin <= 0 {
		perMin = DefaultRatePerMin
	}
	capacity := float64(perMin)
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.init {
		b.init, b.tokens, b.last = true, capacity, now
	}
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens += el * capacity / 60
		b.last = now
	}
	if b.tokens > capacity {
		b.tokens = capacity
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
