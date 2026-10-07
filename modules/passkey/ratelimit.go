//go:build !no_passkey

package passkey

import (
	"sync"
	"time"
)

const (
	rateLimitPerMinute = 10
	rateWindow         = time.Minute
	maxRateKeys        = 100000
)

// limiter is a small in-memory sliding-window counter keyed by an arbitrary
// string (client IP or identity). Memory is bounded: expired keys are swept
// and, in the worst case, the whole map is reset when it exceeds maxRateKeys.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

// allow records one hit for key and reports whether it is within max per rateWindow.
func (l *limiter) allow(key string, now time.Time, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) >= maxRateKeys {
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > rateWindow {
				delete(l.hits, k)
			}
		}
		if len(l.hits) >= maxRateKeys {
			l.hits = map[string][]time.Time{}
		}
	}
	cut := now.Add(-rateWindow)
	v := l.hits[key]
	i := 0
	for i < len(v) && v[i].Before(cut) {
		i++
	}
	v = v[i:]
	if len(v) >= max {
		l.hits[key] = v
		return false
	}
	l.hits[key] = append(v, now)
	return true
}
