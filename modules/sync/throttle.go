//go:build !no_sync

package sync

import (
	"net"
	"net/netip"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"
)

// Built-in per-IP limits of the unauthenticated sync routes. They apply
// regardless of Settings > Rate limits (which is off by default).
const (
	throttleWindow       = time.Minute
	throttleMaxKeys      = 50000
	defaultEnrollPerMin  = 20
	defaultHandshakeMin  = 120
	maxNodeHeaderLen     = 64
	throttleRetryAfterSc = "60"
)

// hubGuards is the abuse protection state of the hub routes.
type hubGuards struct {
	enroll    ipThrottle
	handshake ipThrottle

	// enrollPerMin and handshakePerMin override the defaults (tests).
	enrollPerMin    int
	handshakePerMin int

	// unknownHS counts handshakes for node ids that do not exist. They are not
	// audited (an anonymous caller could fill the audit log).
	unknownHS atomic.Int64
}

func (g *hubGuards) enrollMax() int {
	if g.enrollPerMin > 0 {
		return g.enrollPerMin
	}
	return defaultEnrollPerMin
}

func (g *hubGuards) handshakeMax() int {
	if g.handshakePerMin > 0 {
		return g.handshakePerMin
	}
	return defaultHandshakeMin
}

// UnknownHandshakes is the number of handshakes for unknown node ids so far.
func (m *Module) UnknownHandshakes() int64 { return m.guards.unknownHS.Load() }

// ipThrottle is a fixed window counter per client address.
type ipThrottle struct {
	mu   stdsync.Mutex
	hits map[string]*ipWindow
}

type ipWindow struct {
	start time.Time
	n     int
}

// allow counts one hit of key and reports whether it is within max per window.
func (t *ipThrottle) allow(key string, now time.Time, max int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.hits == nil {
		t.hits = map[string]*ipWindow{}
	}
	if len(t.hits) >= throttleMaxKeys {
		for k, v := range t.hits {
			if now.Sub(v.start) > throttleWindow {
				delete(t.hits, k)
			}
		}
		for k := range t.hits { // still full: make room
			if len(t.hits) < throttleMaxKeys {
				break
			}
			delete(t.hits, k)
		}
	}
	w := t.hits[key]
	if w == nil || now.Sub(w.start) > throttleWindow {
		w = &ipWindow{start: now}
		t.hits[key] = w
	}
	w.n++
	return w.n <= max
}

// throttleKey is the bucket of an address; IPv6 clients share one per /64.
func throttleKey(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		ip = ip.Unmap()
		if ip.Is6() {
			if p, err := ip.Prefix(64); err == nil {
				return p.String()
			}
		}
		return ip.String()
	}
	return host
}
