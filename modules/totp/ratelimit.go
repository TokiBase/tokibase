package totp

import (
	"sync"
	"time"
)

const (
	rateWindow  = time.Minute
	maxRateKeys = 100000
)

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

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
