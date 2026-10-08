package kernel

import (
	"sync"
	"time"
)

// SyncState is the coarse state of the sync loop.
type SyncState string

// Sync states.
const (
	SyncStateUnknown SyncState = ""
	SyncStateOnline  SyncState = "online"
	SyncStateOffline SyncState = "offline"
	SyncStatePaused  SyncState = "paused"
	// SyncStateHub means this process is the hub (always reachable by itself).
	SyncStateHub SyncState = "hub"
)

// SyncStatus is a point-in-time view of replication, for offline indicators.
type SyncStatus struct {
	State SyncState
	// HubReachable is true when the last cycle reached the hub.
	HubReachable bool
	// Pending is the number of local changes the hub has not acknowledged.
	Pending int64
	// LastSync is the time of the last successful cycle (zero when never).
	LastSync time.Time
}

// SyncStatusProvider returns the current [SyncStatus].
type SyncStatusProvider func() SyncStatus

var syncStatusProviders sync.Map // App -> SyncStatusProvider

// SetSyncStatusProvider registers the status provider of app (nil removes it).
func SetSyncStatusProvider(app App, p SyncStatusProvider) {
	if app == nil {
		return
	}
	if p == nil {
		syncStatusProviders.Delete(app)
		return
	}
	syncStatusProviders.Store(app, p)
}

// SyncStatusOf returns the sync status of app. ok is false when no provider is
// registered (sync is off or compiled out); callers then omit the fields.
func SyncStatusOf(app App) (st SyncStatus, ok bool) {
	if app == nil {
		return SyncStatus{}, false
	}
	v, _ := syncStatusProviders.Load(app)
	p, _ := v.(SyncStatusProvider)
	if p == nil {
		return SyncStatus{}, false
	}
	return p(), true
}
