//go:build !no_sync

package client

import (
	"sort"
	"sync"
	"time"
)

// FakeSched is a Scheduler driven by Advance. It lives in a _test file of the
// client package; modules/sync has its own copy (a _test file cannot be imported).
type fakeSched struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// loopPasses (optional) lets Advance wait for the loop goroutine instead of sleeping.
	loopPasses func() int64
}

type fakeTimer struct {
	s      *fakeSched
	when   time.Time
	active bool
	ch     chan time.Time
	fn     func()
}

func newFakeSched() *fakeSched {
	return &fakeSched{now: time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)}
}

func (s *fakeSched) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *fakeSched) add(d time.Duration, fn func()) *fakeTimer {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &fakeTimer{s: s, when: s.now.Add(d), active: true, fn: fn}
	if fn == nil {
		t.ch = make(chan time.Time, 1)
		if d <= 0 {
			t.ch <- s.now
			t.active = false
		}
	}
	s.timers = append(s.timers, t)
	return t
}

func (s *fakeSched) NewTimer(d time.Duration) Timer            { return s.add(d, nil) }
func (s *fakeSched) AfterFunc(d time.Duration, f func()) Timer { return s.add(d, f) }

func (t *fakeTimer) C() <-chan time.Time { return t.ch }
func (t *fakeTimer) Stop() bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	was := t.active
	t.active = false
	return was
}
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	was := t.active
	t.active, t.when = true, t.s.now.Add(d)
	if d <= 0 && t.fn == nil { // a real timer fires at once for d <= 0
		select {
		case t.ch <- t.s.now:
		default:
		}
		t.active = false
	}
	return was
}

// Advance moves the clock by d, firing the timers that fall due in order. A
// timer the loop waits on is followed by waiting until the loop finished the
// wake-up (loopPasses), so that it has re-armed its timer before the next step;
// AfterFunc callbacks run synchronously (they only kick the loop).
func (s *fakeSched) Advance(d time.Duration) {
	s.mu.Lock()
	target := s.now.Add(d)
	s.mu.Unlock()
	for {
		s.mu.Lock()
		var due []*fakeTimer
		for _, t := range s.timers {
			if t.active && !t.when.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			s.now = target
			s.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		if t.when.After(s.now) {
			s.now = t.when
		}
		t.active = false
		now := s.now
		hook := s.loopPasses
		s.mu.Unlock()
		if t.fn != nil {
			t.fn()
			continue
		}
		var before int64
		if hook != nil {
			before = hook()
		}
		select {
		case t.ch <- now:
		default:
		}
		if hook != nil {
			for i := 0; hook() <= before && i < 5000; i++ {
				time.Sleep(time.Millisecond)
			}
		}
	}
}

// pending reports a fired timer token the loop has not consumed yet, or a timer
// that is due now.
func (s *fakeSched) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.timers {
		if t.ch != nil && len(t.ch) > 0 {
			return true
		}
		if t.active && !t.when.After(s.now) {
			return true
		}
	}
	return false
}
