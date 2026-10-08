package hlc

import (
	"sync"
	"testing"
	"time"
)

type fakeNow struct {
	mu sync.Mutex
	t  time.Time
	// advanceAfter makes the clock jump 1 ms after that many reads (0 = never)
	advanceAfter int
	reads        int
}

func (f *fakeNow) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.advanceAfter > 0 && f.reads%f.advanceAfter == 0 {
		f.t = f.t.Add(time.Millisecond)
	}
	return f.t
}

func (f *fakeNow) set(t time.Time) { f.mu.Lock(); f.t = t; f.mu.Unlock() }

var t0 = time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)

func TestFormat(t *testing.T) {
	h := Make(t0.UnixMilli(), 7)
	if h.PhysicalMs() != t0.UnixMilli() || h.Logical() != 7 || !h.Physical().Equal(t0) {
		t.Fatalf("parts: %v %v", h.PhysicalMs(), h.Logical())
	}
	s := h.String()
	if len(s) != 16 {
		t.Fatalf("len %d", len(s))
	}
	p, err := Parse(s)
	if err != nil || p != h {
		t.Fatalf("roundtrip %v %v", p, err)
	}
	if _, err := Parse("zz"); err == nil {
		t.Fatal("want error")
	}
	if _, err := Parse("zzzzzzzzzzzzzzzz"); err == nil {
		t.Fatal("want error")
	}
	if int64(h) < 0 {
		t.Fatal("must fit int64 (SQLite INTEGER)")
	}
}

func TestLessTotalOrder(t *testing.T) {
	a, b := Make(100, 1), Make(100, 2)
	if !Less(a, "n2", b, "n1") || Less(b, "n1", a, "n2") {
		t.Fatal("hlc decides first")
	}
	if !Less(a, "n1", a, "n2") || Less(a, "n2", a, "n1") || Less(a, "n1", a, "n1") {
		t.Fatal("node id breaks ties")
	}
}

func TestNowMonotonicFrozenAndBackwards(t *testing.T) {
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	prev := c.Now()
	for i := 0; i < 50; i++ {
		n := c.Now()
		if n <= prev {
			t.Fatalf("not monotonic: %v <= %v", n, prev)
		}
		prev = n
	}
	if prev.PhysicalMs() != t0.UnixMilli() || prev.Logical() != 50 {
		t.Fatalf("frozen wall clock: want logical 50 got %d", prev.Logical())
	}
	f.set(t0.Add(-time.Hour)) // wall clock jumps back
	if n := c.Now(); n <= prev || n.PhysicalMs() != t0.UnixMilli() {
		t.Fatalf("backwards jump must not move the clock back: %v", n)
	}
	f.set(t0.Add(time.Second))
	if n := c.Now(); n.PhysicalMs() != t0.Add(time.Second).UnixMilli() || n.Logical() != 0 {
		t.Fatalf("wall clock ahead resets logical: %v", n)
	}
}

func TestConcurrentUnique(t *testing.T) {
	c := NewClock((&fakeNow{t: t0, advanceAfter: 7}).now, 0)
	const g, n = 8, 500
	out := make(chan HLC, g*n)
	var wg sync.WaitGroup
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < n; j++ {
				out <- c.Now()
			}
		}()
	}
	wg.Wait()
	close(out)
	seen := map[HLC]bool{}
	for h := range out {
		if seen[h] {
			t.Fatalf("duplicate %v", h)
		}
		seen[h] = true
	}
}

func TestLogicalOverflowSpinsToNextMs(t *testing.T) {
	// the wall clock moves 1 ms every 3 reads, so the spin ends by itself
	f := &fakeNow{t: t0, advanceAfter: 3}
	c := NewClock(f.now, Make(t0.UnixMilli(), logicalMask))
	got := c.Now()
	if got.Logical() != 0 || got.PhysicalMs() <= t0.UnixMilli() {
		t.Fatalf("overflow must move to a later ms with logical 0, got %d/%d", got.PhysicalMs(), got.Logical())
	}
	if got <= Make(t0.UnixMilli(), logicalMask) {
		t.Fatal("not monotonic across overflow")
	}
}

func TestLogicalOverflowStuckClockBorrows(t *testing.T) {
	f := &fakeNow{t: t0} // never advances
	c := NewClock(f.now, Make(t0.UnixMilli(), logicalMask))
	done := make(chan HLC, 1)
	go func() { done <- c.Now() }()
	select {
	case h := <-done:
		if h.PhysicalMs() != t0.UnixMilli()+1 || h.Logical() != 0 {
			t.Fatalf("got %d/%d", h.PhysicalMs(), h.Logical())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spin never ended")
	}
}

func TestObserve(t *testing.T) {
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	local := c.Now()

	// remote ahead: result is above both, same physical, logical+1
	remote := Make(t0.UnixMilli()+500, 9)
	got := c.Observe(remote)
	if got != Make(t0.UnixMilli()+500, 10) {
		t.Fatalf("remote ahead: %v", got)
	}
	// remote behind: only the local counter advances
	got2 := c.Observe(local)
	if got2 != Make(t0.UnixMilli()+500, 11) {
		t.Fatalf("remote behind: %v", got2)
	}
	// remote equal to last: still strictly greater
	if got3 := c.Observe(got2); got3 <= got2 {
		t.Fatalf("equal remote: %v", got3)
	}
	// wall clock overtakes: logical resets
	f.set(t0.Add(time.Hour))
	if got4 := c.Observe(Make(t0.UnixMilli(), 3)); got4 != Make(t0.Add(time.Hour).UnixMilli(), 0) {
		t.Fatalf("wall ahead: %v", got4)
	}
}

func TestOffsetAndWallNow(t *testing.T) {
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	c.SetOffset(48 * time.Hour)
	if c.Offset() != 48*time.Hour || !c.WallNow().Equal(t0.Add(48*time.Hour)) {
		t.Fatal("offset not applied")
	}
	if h := c.Now(); h.PhysicalMs() != t0.Add(48*time.Hour).UnixMilli() {
		t.Fatalf("Now ignores offset: %v", h.Physical())
	}
	c.SetOffset(-time.Minute)
	if !c.WallNow().Equal(t0.Add(-time.Minute)) {
		t.Fatal("negative offset")
	}
}

type memStore map[string]string

func (m memStore) Get(k string) (string, bool, error) { v, ok := m[k]; return v, ok, nil }
func (m memStore) Set(k, v string) error              { m[k] = v; return nil }

func TestFloorPersistence(t *testing.T) {
	s := memStore{}
	if f, err := LoadFloor(s); err != nil || f != 0 {
		t.Fatalf("empty: %v %v", f, err)
	}
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	c.SetFloorEvery(10)
	for i := 0; i < 9; i++ {
		c.Now()
	}
	if _, ok := c.NeedsFloor(); ok {
		t.Fatal("not due yet")
	}
	c.Now()
	h, ok := c.NeedsFloor()
	if !ok || h != c.Last() {
		t.Fatalf("due: %v %v", h, ok)
	}
	if err := SaveFloor(s, h); err != nil {
		t.Fatal(err)
	}
	c.Persisted(h)
	if _, ok := c.NeedsFloor(); ok {
		t.Fatal("counter must restart after Persisted")
	}
	// a lower floor never replaces a higher one
	if err := SaveFloor(s, h-5); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadFloor(s); got != h {
		t.Fatalf("floor went backwards: %v", got)
	}

	// restart with a pruned _changes (max 0) and a wall clock BEHIND the floor:
	// the new clock must stay above the floor
	f2 := &fakeNow{t: t0.Add(-time.Hour)}
	start, err := Boot(s, 0)
	if err != nil || start != h {
		t.Fatalf("boot: %v %v", start, err)
	}
	c2 := NewClock(f2.now, start)
	if n := c2.Now(); n <= h {
		t.Fatalf("restart went backwards: %v <= %v", n, h)
	}
	// _changes ahead of the floor wins
	if st, _ := Boot(s, h+100); st != h+100 {
		t.Fatalf("boot picks the max, got %v", st)
	}
}

func TestObserveNeverDecreasesAtMax(t *testing.T) {
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	c.Observe(HLC(1<<63 - 1)) // clamped to the int64 range
	prev := c.Last()
	if int64(prev) < 0 {
		t.Fatalf("last %v is not an int64", prev)
	}
	for i := 0; i < 5; i++ {
		n := c.Now()
		if n < prev || int64(n) < 0 {
			t.Fatalf("clock went down or out of range: %v after %v", n, prev)
		}
		prev = n
	}
	c.Observe(HLC(1 << 63)) // top bit set: clamped, still not lower
	if c.Last() < prev {
		t.Fatal("Observe decreased last")
	}
}

func TestObserveBounded(t *testing.T) {
	f := &fakeNow{t: t0}
	c := NewClock(f.now, 0)
	ok := Make(t0.Add(4*time.Minute).UnixMilli(), 0)
	if _, err := c.ObserveBounded(ok, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	before := c.Last()
	far := Make(t0.Add(time.Hour).UnixMilli(), 0)
	if _, err := c.ObserveBounded(far, 5*time.Minute); err != ErrFutureHLC {
		t.Fatalf("err %v", err)
	}
	if _, err := c.ObserveBounded(HLC(1<<63), 5*time.Minute); err != ErrFutureHLC {
		t.Fatalf("err %v", err)
	}
	if c.Last() != before {
		t.Fatal("a refused remote must not move the clock")
	}
}

func TestBootTakesMaxOfSeen(t *testing.T) {
	s := memStore{}
	if err := SaveFloor(s, 50); err != nil {
		t.Fatal(err)
	}
	if h, _ := Boot(s, 10, 70, 60); h != 70 {
		t.Fatalf("boot %v", h)
	}
	if h, _ := Boot(s, 10); h != 50 {
		t.Fatalf("boot %v", h)
	}
}
