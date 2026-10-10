//go:build !no_sync

package embed

import (
	"context"
	"errors"
	stdsync "sync"

	"github.com/tokibase/tokibase/modules/sync"
	"github.com/tokibase/tokibase/modules/sync/client"
)

// Sync is the spoke side sync facade of an Instance (docs/SYNC_DESIGN.md §6.3).
// Obtain it with Instance.Sync. Every method fails with an error when the
// instance is not a spoke (set Options.Sync, or TOKI_SYNC_ROLE=spoke).
type Sync struct{ i *Instance }

// Sync returns the sync facade. It is cheap and may be called any time.
func (i *Instance) Sync() *Sync { return &Sync{i: i} }

func (s *Sync) mod() (*sync.Module, error) {
	m := sync.FromApp(s.i.app)
	if m == nil || m.Role() != sync.RoleSpoke {
		return nil, sync.ErrNotSpoke
	}
	return m, nil
}

// Enroll joins the hub with a one-time code (created on the hub with
// `toki sync enroll`) and starts the sync loop. An empty hubURL uses
// Options.Sync.HubURL.
func (s *Sync) Enroll(ctx context.Context, hubURL, code string) error {
	if err := s.i.enter(); err != nil {
		return err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return err
	}
	if hubURL == "" {
		hubURL = s.i.syncHub
	}
	if hubURL == "" {
		return errors.New("embed: no hub URL (pass one or set Options.Sync.HubURL)")
	}
	return m.Enroll(ctx, hubURL, code, s.i.profile)
}

// AddActor asks the hub for an actor grant for the user behind hubToken (a hub
// auth token from the normal login) and returns the grant id. The writes of
// that user are then attributed to them on the hub (LocalToken).
func (s *Sync) AddActor(ctx context.Context, hubToken string) (aid string, err error) {
	if err := s.i.enter(); err != nil {
		return "", err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return "", err
	}
	if !m.Enrolled() {
		return "", sync.ErrNotEnrolled
	}
	r, err := m.AddActor(ctx, hubToken)
	if err != nil {
		return "", err
	}
	return r.AID, nil
}

// LocalToken mints a local auth token for the user of a grant. Use it as the
// Authorization header of Call.
func (s *Sync) LocalToken(aid string) (string, error) {
	if err := s.i.enter(); err != nil {
		return "", err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return "", err
	}
	return m.LocalActorToken(aid)
}

// Now runs a sync cycle and waits for it. It works while the loop backs off, and
// pulls also on a metered link (see SetConditions).
func (s *Sync) Now(ctx context.Context) error {
	if err := s.i.enter(); err != nil {
		return err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return err
	}
	return m.Now(ctx)
}

// Status returns the loop status as JSON: state, online, metered, low_power,
// background, background_done, paused, running, pending, conflicts, pull_after,
// acked_origin, last_ok, last_partial, pull_deferred, last_error, offset_ms, failures, next_attempt,
// apply_errors, heal, digest_mismatch.
func (s *Sync) Status() ([]byte, error) {
	if err := s.i.enter(); err != nil {
		return nil, err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return nil, err
	}
	return m.StatusJSON()
}

// SetConditions tells the loop about the device (connectivity_plus and the
// power manager of the host feed it). online=false stops every attempt; metered
// makes automatic cycles push only (SyncNow still pulls, in pages of at most
// 100 changes) and uses the 5 minute interval, as does lowPower; background marks
// an OS granted slot: one cycle of at most 20 s, then the loop stays quiet until
// the conditions change or Now is called. It never blocks and never fails.
func (s *Sync) SetConditions(online, metered, lowPower, background bool) {
	if err := s.i.enter(); err != nil {
		return // stopped: nothing to tell
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return
	}
	m.SetConditions(client.Conditions{Online: online, Metered: metered, LowPower: lowPower, Background: background})
}

// OnEvent registers fn for the sync events as JSON ({"type":"applied|pushed|
// rejected|superseded|parked|error|rebootstrap|revoked|digest_mismatch|synced|
// partial|epoch","time":...,"collection":...,"record":...,"code":...,"message":...}). fn
// runs on its own goroutine, slow handlers lose events, a panic is recovered.
// cancel is idempotent. Events of a loop that starts later (after Enroll) are
// delivered too. Every handler is cancelled by Instance.Stop, and OnEvent on a
// stopped instance registers nothing.
func (s *Sync) OnEvent(fn func(ev []byte)) (cancel func()) {
	if err := s.i.enter(); err != nil {
		return func() {}
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil || fn == nil {
		return func() {}
	}
	ch := make(chan []byte, 64)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case b := <-ch:
				func() {
					defer func() { _ = recover() }()
					fn(b)
				}()
			case <-done:
				return
			}
		}
	}()
	stop := m.OnEvent(func(ev client.Event) {
		select {
		case ch <- ev.JSON():
		default: // slow handler: drop
		}
	})
	var once stdsync.Once
	cancel = func() { once.Do(func() { stop(); close(done) }) }
	s.i.trackEvent(cancel)
	return cancel
}

// Next returns the next reserved value of a sequence (a ticket number, an
// invoice number). It fails closed when the local ranges are used up: a number
// is never issued twice (the loop asks the hub for a new range when it is
// online).
func (s *Sync) Next(sequence string) (int64, error) {
	if err := s.i.enter(); err != nil {
		return 0, err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return 0, err
	}
	return m.Next(nil, sequence)
}

// Rebootstrap replaces the synced data of this node with a fresh snapshot of
// the hub (unpushed local changes are kept and replayed). The loop does the work
// in the background.
func (s *Sync) Rebootstrap(ctx context.Context) error {
	if err := s.i.enter(); err != nil {
		return err
	}
	defer s.i.calls.Done()
	m, err := s.mod()
	if err != nil {
		return err
	}
	return m.Rebootstrap("rebootstrap requested by the app")
}

// trackEvent remembers an OnEvent cancel so that Stop can end the handler
// goroutine of a host that never cancels.
func (i *Instance) trackEvent(cancel func()) {
	i.evMu.Lock()
	i.evCancels = append(i.evCancels, cancel)
	i.evMu.Unlock()
}

// cancelEvents cancels every OnEvent handler (Stop).
func (i *Instance) cancelEvents() {
	i.evMu.Lock()
	cs := i.evCancels
	i.evCancels = nil
	i.evMu.Unlock()
	for _, c := range cs {
		c()
	}
}

// stopSyncLoop stops the sync loop before Stop waits for in-flight calls: a
// Sync().Now that waits for a cycle is answered with ErrStopped at once instead
// of holding Stop for its own timeout.
func (i *Instance) stopSyncLoop(ctx context.Context) {
	if m := sync.FromApp(i.app); m != nil {
		_ = m.StopLoop(ctx)
	}
}
