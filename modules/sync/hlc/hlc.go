// Package hlc implements the 64-bit hybrid logical clock used by modules/sync:
// 48 bits of unix milliseconds and 16 bits of logical counter. The package
// depends on the standard library only.
package hlc

import (
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// FloorKey is the `_sync_state` key that holds the persisted clock floor.
const FloorKey = "hlc_floor"

// DefaultFloorEvery is the number of ticks between two floor persists.
const DefaultFloorEvery = 1000

const (
	logicalBits = 16
	logicalMask = 1<<logicalBits - 1
	maxPhysical = 1<<48 - 1
)

// HLC is [48 bit unix ms][16 bit logical].
type HLC uint64

// Make builds an HLC from a physical millisecond timestamp and a logical counter.
func Make(physMs int64, logical uint16) HLC {
	if physMs < 0 {
		physMs = 0
	}
	if physMs > maxPhysical {
		physMs = maxPhysical
	}
	return HLC(uint64(physMs)<<logicalBits | uint64(logical))
}

// PhysicalMs returns the physical part in unix milliseconds.
func (h HLC) PhysicalMs() int64 { return int64(uint64(h) >> logicalBits) }

// Physical returns the physical part as a time.
func (h HLC) Physical() time.Time { return time.UnixMilli(h.PhysicalMs()).UTC() }

// Logical returns the logical counter.
func (h HLC) Logical() uint16 { return uint16(uint64(h) & logicalMask) }

// String returns 16 lowercase hex chars (the wire form, JS safe).
func (h HLC) String() string { return fmt.Sprintf("%016x", uint64(h)) }

// Parse parses the String form.
func Parse(s string) (HLC, error) {
	if len(s) != 16 {
		return 0, fmt.Errorf("hlc: want 16 hex chars, got %d", len(s))
	}
	v, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("hlc: %w", err)
	}
	return HLC(v), nil
}

// Less is the total order of changes: (hlc, node id).
func Less(a HLC, an string, b HLC, bn string) bool {
	if a != b {
		return a < b
	}
	return an < bn
}

// Clock issues monotonic HLCs. Safe for concurrent use.
type Clock struct {
	mu   sync.Mutex
	last HLC
	now  func() time.Time

	offset atomic.Int64 // ns

	// floor persistence bookkeeping (see NeedsFloor)
	every     uint64
	ticks     uint64
	persisted HLC
}

// NewClock returns a clock whose next value is greater than last. now may be
// nil (time.Now).
func NewClock(now func() time.Time, last HLC) *Clock {
	if now == nil {
		now = time.Now
	}
	return &Clock{now: now, last: last, persisted: last, every: DefaultFloorEvery}
}

// SetFloorEvery changes the number of ticks between two floor persists.
func (c *Clock) SetFloorEvery(n uint64) {
	c.mu.Lock()
	if n == 0 {
		n = DefaultFloorEvery
	}
	c.every = n
	c.mu.Unlock()
}

// WallNow returns now()+offset: the hub-corrected wall time.
func (c *Clock) WallNow() time.Time {
	return c.now().Add(time.Duration(c.offset.Load()))
}

// SetOffset sets the hub time correction (spoke).
func (c *Clock) SetOffset(d time.Duration) { c.offset.Store(int64(d)) }

// Offset returns the hub time correction.
func (c *Clock) Offset() time.Duration { return time.Duration(c.offset.Load()) }

// Last returns the most recent value issued or observed.
func (c *Clock) Last() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// Now issues the HLC of a local event. When the 16 bit logical counter is
// exhausted inside one millisecond it spins until the wall clock reaches the
// next millisecond.
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick(0)
}

// Observe merges a remote HLC (pull apply, push accept) and returns the new
// local value, which is greater than both the previous local value and remote.
func (c *Clock) Observe(remote HLC) HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tick(remote)
}

// tick implements the HLC receive/send algorithm. remote == 0 is a local event.
func (c *Clock) tick(remote HLC) HLC {
	spins := 0
	for {
		wall := c.WallNow().UnixMilli()
		base := c.last
		if remote > base {
			base = remote
		}
		var next HLC
		switch {
		case wall > base.PhysicalMs():
			next = Make(wall, 0)
		case base.Logical() == logicalMask:
			// logical overflow in this millisecond: wait for the next one
			spins++
			if spins > 200 {
				// the wall clock is stuck or went backwards: borrow the next ms
				next = Make(base.PhysicalMs()+1, 0)
				break
			}
			time.Sleep(50 * time.Microsecond)
			continue
		default:
			next = Make(base.PhysicalMs(), base.Logical()+1)
		}
		c.last = next
		c.ticks++
		return next
	}
}

// NeedsFloor reports whether the floor should be persisted now (every N
// ticks) and the value to persist. The caller writes it in its transaction
// and calls Persisted once that transaction committed.
func (c *Clock) NeedsFloor() (HLC, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ticks >= c.every {
		return c.last, true
	}
	return 0, false
}

// Persisted records that floor h is durable.
func (c *Clock) Persisted(h HLC) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h > c.persisted {
		c.persisted = h
	}
	c.ticks = 0
}

// Store is the key/value table `_sync_state` seen by the persistence helpers.
type Store interface {
	Get(key string) (string, bool, error)
	Set(key, value string) error
}

// LoadFloor reads the persisted floor (0 when missing).
func LoadFloor(s Store) (HLC, error) {
	v, ok, err := s.Get(FloorKey)
	if err != nil || !ok || v == "" {
		return 0, err
	}
	return Parse(v)
}

// SaveFloor writes the floor; a lower value never replaces a higher one.
func SaveFloor(s Store, h HLC) error {
	cur, err := LoadFloor(s)
	if err != nil {
		return err
	}
	if cur >= h {
		return nil
	}
	return s.Set(FloorKey, h.String())
}

// Boot computes the starting value of a clock: max(maxChanges, floor).
func Boot(s Store, maxChanges HLC) (HLC, error) {
	f, err := LoadFloor(s)
	if err != nil {
		return 0, err
	}
	if maxChanges > f {
		return maxChanges, nil
	}
	return f, nil
}
