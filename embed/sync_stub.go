//go:build no_sync

package embed

import "context"

// Sync is the sync facade of an Instance. In a build with the no_sync tag every
// method returns ErrSyncUnavailable.
type Sync struct{ i *Instance }

// Sync returns the sync facade.
func (i *Instance) Sync() *Sync { return &Sync{i: i} }

// Enroll returns ErrSyncUnavailable.
func (s *Sync) Enroll(ctx context.Context, hubURL, code string) error { return ErrSyncUnavailable }

// AddActor returns ErrSyncUnavailable.
func (s *Sync) AddActor(ctx context.Context, hubToken string) (string, error) {
	return "", ErrSyncUnavailable
}

// LocalToken returns ErrSyncUnavailable.
func (s *Sync) LocalToken(aid string) (string, error) { return "", ErrSyncUnavailable }

// Now returns ErrSyncUnavailable.
func (s *Sync) Now(ctx context.Context) error { return ErrSyncUnavailable }

// Status returns ErrSyncUnavailable.
func (s *Sync) Status() ([]byte, error) { return nil, ErrSyncUnavailable }

// SetConditions does nothing.
func (s *Sync) SetConditions(online, metered, lowPower, background bool) {}

// OnEvent returns a no-op cancel function.
func (s *Sync) OnEvent(fn func(ev []byte)) (cancel func()) { return func() {} }

// Next returns ErrSyncUnavailable.
func (s *Sync) Next(sequence string) (int64, error) { return 0, ErrSyncUnavailable }

// Rebootstrap returns ErrSyncUnavailable.
func (s *Sync) Rebootstrap(ctx context.Context) error { return ErrSyncUnavailable }
