package mobile

import (
	"context"
	"errors"
	"time"
)

// Sync wrappers (docs/SYNC_DESIGN.md §6.3, docs/EMBED.md "Sync from a Flutter
// app"). They need an instance started with the profile nano or edge and the
// env "TOKI_SYNC_ROLE":"spoke" in envJSON (and "TOKI_SYNC_INSECURE":"1" for a
// plain http hub on a private network). In a build with the no_sync tag every
// call returns an "sync is not available" error.

// syncTimeout bounds the calls that talk to the hub.
const syncTimeout = 60 * time.Second

// SyncEnroll joins the hub with a one-time code and starts the sync loop.
func (h *Handle) SyncEnroll(hubURL, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	return h.inst.Sync().Enroll(ctx, hubURL, code)
}

// SyncAddActor asks the hub for an actor grant for the user behind hubToken (a
// hub auth token from the app's normal login) and returns the grant id.
func (h *Handle) SyncAddActor(hubToken string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	return h.inst.Sync().AddActor(ctx, hubToken)
}

// SyncLocalToken mints a local auth token for the user of a grant; pass it as
// the Authorization header of Call.
func (h *Handle) SyncLocalToken(aid string) (string, error) {
	return h.inst.Sync().LocalToken(aid)
}

// SyncNow runs a sync cycle and waits for it (at most 60 s). Call it from a
// WorkManager task, a foreground service or a BGAppRefreshTask.
func (h *Handle) SyncNow() error {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	return h.inst.Sync().Now(ctx)
}

// SyncStatus returns the loop status as a JSON object.
func (h *Handle) SyncStatus() (string, error) {
	b, err := h.inst.Sync().Status()
	return string(b), err
}

// SyncSetConditions reports the device conditions: online=false stops attempts,
// metered makes automatic cycles push only, lowPower and metered use the 5
// minute interval, background marks an OS granted slot (one cycle of at most
// 20 s).
func (h *Handle) SyncSetConditions(online, metered, lowPower, background bool) {
	h.inst.Sync().SetConditions(online, metered, lowPower, background)
}

// SyncSubscribe registers cb for the sync events (JSON bytes) and returns an id
// for Unsubscribe.
func (h *Handle) SyncSubscribe(cb EventCallback) (int, error) {
	if cb == nil {
		return 0, errors.New("mobile: nil callback")
	}
	return h.register(h.inst.Sync().OnEvent(cb.OnEvent)), nil
}

// SyncNext returns the next reserved value of a sequence (fails closed when the
// local ranges are used up).
func (h *Handle) SyncNext(seq string) (int64, error) {
	return h.inst.Sync().Next(seq)
}

// SyncRebootstrap replaces the synced data with a fresh snapshot of the hub.
func (h *Handle) SyncRebootstrap() error {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	return h.inst.Sync().Rebootstrap(ctx)
}
