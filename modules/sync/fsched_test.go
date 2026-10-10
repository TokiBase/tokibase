//go:build !no_sync

package sync

import (
	"sort"
	stdsync "sync"
	"time"

	"github.com/tokibase/tokibase/modules/sync/client"
)

// fsched is a client.Scheduler driven by Advance (a copy of the one in the client package tests).
type fsched struct {
	mu     stdsync.Mutex
	now    time.Time
	timers []*ftimer
}

type ftimer struct {
	s      *fsched
	when   time.Time
	active bool
	ch     chan time.Time
	fn     func()
}

func newFsched() *fsched {
	return &fsched{now: time.Now()}
}

func (s *fsched) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

func (s *fsched) add(d time.Duration, fn func()) *ftimer {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := &ftimer{s: s, when: s.now.Add(d), active: true, fn: fn}
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

func (s *fsched) NewTimer(d time.Duration) client.Timer            { return s.add(d, nil) }
func (s *fsched) AfterFunc(d time.Duration, f func()) client.Timer { return s.add(d, f) }

func (t *ftimer) C() <-chan time.Time { return t.ch }
func (t *ftimer) Stop() bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	was := t.active
	t.active = false
	return was
}
func (t *ftimer) Reset(d time.Duration) bool {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	was := t.active
	t.active, t.when = true, t.s.now.Add(d)
	return was
}

// Advance moves the clock by d, firing the timers that fall due in order. The
// real sleeps give the loop goroutine time to re-arm its timer between steps.
func (s *fsched) Advance(d time.Duration) {
	s.mu.Lock()
	target := s.now.Add(d)
	s.mu.Unlock()
	for {
		s.mu.Lock()
		var due []*ftimer
		for _, t := range s.timers {
			if t.active && !t.when.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			s.now = target
			s.mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			return
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		if t.when.After(s.now) {
			s.now = t.when
		}
		t.active = false
		now := s.now
		s.mu.Unlock()
		if t.fn != nil {
			go t.fn()
		} else {
			select {
			case t.ch <- now:
			default:
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
