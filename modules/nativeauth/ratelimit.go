//go:build !no_nativeauth

package nativeauth

import (
	"net/netip"
	"sort"
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
			l.evictOldest(maxRateKeys / 10)
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

// peek reports whether key is still under max hits without counting a new one.
func (l *limiter) peek(key string, now time.Time, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, t := range l.hits[key] {
		if now.Sub(t) <= rateWindow {
			n++
		}
	}
	return n < max
}

// evictOldest drops the n keys whose last hit is the oldest (never the whole table).
func (l *limiter) evictOldest(n int) {
	type kv struct {
		k string
		t time.Time
	}
	all := make([]kv, 0, len(l.hits))
	for k, v := range l.hits {
		var t time.Time
		if len(v) > 0 {
			t = v[len(v)-1]
		}
		all = append(all, kv{k, t})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].t.Before(all[j].t) })
	if n > len(all) {
		n = len(all)
	}
	for _, e := range all[:n] {
		delete(l.hits, e.k)
	}
}

// rateKey normalises a client address: IPv6 addresses are keyed by their /64
// (one subscriber owns a whole /64), IPv4 and unparsable values are kept as is.
func rateKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}
