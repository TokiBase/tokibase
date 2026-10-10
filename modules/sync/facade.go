//go:build !no_sync

package sync

import (
	"context"
	"errors"
	stdsync "sync"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
)

// Module level pieces of the embed and mobile facade (docs/SYNC_DESIGN.md §6.3,
// package embed: Instance.Sync()). They need a spoke; every method says so.

// ErrNotSpoke is returned by the facade methods on a node that is not a spoke.
var ErrNotSpoke = errors.New("sync: this node is not a spoke (TOKI_SYNC_ROLE=spoke)")

// ErrNotEnrolled is returned when the node has not joined a hub yet.
var ErrNotEnrolled = errors.New("sync: this node is not enrolled (Enroll first)")

// FromApp returns the sync module registered on app, or nil when the role is off.
func FromApp(app core.App) *Module { return moduleOf(app) }

// facade holds the listeners that outlive the client loop (the loop is created
// at Enroll or at serve time, the app registers its listener earlier).
type facade struct {
	mu  stdsync.Mutex
	seq int
	fns map[int]func(client.Event)
}

func (m *Module) requireSpoke() error {
	if m == nil || m.role != RoleSpoke || !m.ready.Load() {
		return ErrNotSpoke
	}
	return nil
}

// Enroll joins the hub at hubURL with a one-time code and starts the loop.
// profile is the node profile the hub records (nano, edge, ...).
func (m *Module) Enroll(ctx context.Context, hubURL, code, profile string) error {
	if err := m.requireSpoke(); err != nil {
		return err
	}
	if profile == "" {
		profile = profileFromEnv()
	}
	if _, err := client.Join(ctx, m.app, client.EnrollParams{
		HubURL: hubURL, Code: code, Identity: m.spoke, Profile: profile, AppVersion: appVersion(),
	}); err != nil {
		return err
	}
	return m.StartLoop(context.Background())
}

// Enrolled reports whether the node has joined a hub.
func (m *Module) Enrolled() bool {
	cur, err := client.LoadCursor(m.app)
	return err == nil && cur != nil
}

// SetConditions forwards the device conditions to the loop. Before the loop
// exists they are remembered and applied when it starts.
func (m *Module) SetConditions(c client.Conditions) {
	m.condMu.Lock()
	defer m.condMu.Unlock()
	m.cond, m.condSet = c, true
	if cl := m.Client(); cl != nil {
		cl.SetConditions(c)
	}
}

// Now runs a sync cycle and waits for it. In a background slot (Conditions.
// Background) the cycle is cut after 20 s: it then returns client.ErrPartial when
// it made progress and a deadline error when it did not, never nil.
func (m *Module) Now(ctx context.Context) error {
	if err := m.requireSpoke(); err != nil {
		return err
	}
	cl := m.Client()
	if cl == nil {
		return ErrNotEnrolled
	}
	select {
	case r := <-cl.SyncNow():
		return r.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StatusJSON is the status of the loop as JSON.
func (m *Module) StatusJSON() ([]byte, error) {
	if err := m.requireSpoke(); err != nil {
		return nil, err
	}
	cl := m.Client()
	if cl == nil {
		return nil, ErrNotEnrolled
	}
	return cl.StatusJSON()
}

// OnEvent registers fn for the events of the loop (also those of a loop that
// starts later). fn must not block.
func (m *Module) OnEvent(fn func(client.Event)) (cancel func()) {
	f := &m.fac
	f.mu.Lock()
	f.seq++
	id := f.seq
	if f.fns == nil {
		f.fns = map[int]func(client.Event){}
	}
	f.fns[id] = fn
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		delete(f.fns, id)
		f.mu.Unlock()
	}
}

func (m *Module) dispatchEvent(ev client.Event) {
	f := &m.fac
	f.mu.Lock()
	fns := make([]func(client.Event), 0, len(f.fns))
	for _, fn := range f.fns {
		fns = append(fns, fn)
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn(ev)
	}
}

// Rebootstrap schedules a snapshot bootstrap (what `toki sync rebootstrap` does
// on a spoke) and wakes the loop.
func (m *Module) Rebootstrap(reason string) error {
	if err := m.requireSpoke(); err != nil {
		return err
	}
	if cl := m.Client(); cl != nil {
		return cl.Rebootstrap(reason)
	}
	if !m.Enrolled() {
		return ErrNotEnrolled
	}
	return client.ScheduleRebootstrap(m.app, reason)
}

// LocalActorToken is LocalToken for the facade (a spoke check first).
func (m *Module) LocalActorToken(aid string) (string, error) {
	if err := m.requireSpoke(); err != nil {
		return "", err
	}
	return m.LocalToken(aid)
}
