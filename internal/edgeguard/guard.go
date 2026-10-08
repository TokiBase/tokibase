// Package edgeguard holds the small access helpers shared by the edge
// modules (printer, scanner): an allowlist of auth collections / records and
// a per-key request throttle.
package edgeguard

import (
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
)

// Allow is a parsed allowlist: entries are "collection" (every record of an
// auth collection) or "collection/id" (one record). Superusers are always
// allowed by the callers, not listed here.
type Allow struct {
	cols map[string]bool
	recs map[string]bool
}

// ParseAllow parses a comma separated list.
func ParseAllow(list string) *Allow {
	a := &Allow{cols: map[string]bool{}, recs: map[string]bool{}}
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		switch {
		case e == "":
		case strings.Contains(e, "/"):
			a.recs[e] = true
		default:
			a.cols[e] = true
		}
	}
	return a
}

// Empty reports an allowlist without entries.
func (a *Allow) Empty() bool { return a == nil || (len(a.cols) == 0 && len(a.recs) == 0) }

// MatchActor reports whether actor ("collection/id") is listed.
func (a *Allow) MatchActor(actor string) bool {
	if a == nil || actor == "" {
		return false
	}
	if a.recs[actor] {
		return true
	}
	col, _, _ := strings.Cut(actor, "/")
	return a.cols[col]
}

// Match reports whether the auth record is listed.
func (a *Allow) Match(rec *core.Record) bool {
	if rec == nil {
		return false
	}
	return a.MatchActor(ActorOf(rec))
}

// ActorOf is "collection/id" of an auth record ("" for nil).
func ActorOf(rec *core.Record) string {
	if rec == nil {
		return ""
	}
	return rec.Collection().Name + "/" + rec.Id
}

// IsSuperuser reports a record of the _superusers collection.
func IsSuperuser(rec *core.Record) bool {
	return rec != nil && rec.Collection().Name == core.CollectionNameSuperusers
}

// Throttle is a fixed window counter per key.
type Throttle struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	m      map[string]*bucket
	sweep  time.Time
}

type bucket struct {
	start time.Time
	n     int
}

// NewThrottle allows limit events per window and key; limit <= 0 disables it.
func NewThrottle(limit int, window time.Duration) *Throttle {
	return &Throttle{limit: limit, window: window, now: time.Now, m: map[string]*bucket{}}
}

// SetClock replaces the clock (tests).
func (t *Throttle) SetClock(now func() time.Time) { t.now = now }

// Allow counts one event of key and reports whether it is within the limit.
func (t *Throttle) Allow(key string) bool {
	if t == nil || t.limit <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if now.Sub(t.sweep) > t.window || len(t.m) > 10000 {
		for k, b := range t.m {
			if now.Sub(b.start) >= t.window {
				delete(t.m, k)
			}
		}
		t.sweep = now
	}
	b := t.m[key]
	if b == nil || now.Sub(b.start) >= t.window {
		b = &bucket{start: now}
		t.m[key] = b
	}
	b.n++
	return b.n <= t.limit
}
