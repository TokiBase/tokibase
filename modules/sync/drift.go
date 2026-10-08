//go:build !no_sync

package sync

import (
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Clock drift and schema pre-checks of a push (docs/SYNC_DESIGN.md §3.4, §3.7).
//
// The handshake only reports the verdict (`clock.ok`). Enforcement is here: a
// node whose last handshake was out of tolerance, or whose push carries a
// client_time out of tolerance, is refused with 409 sync_clock_drift until a
// handshake with corrected time succeeds. The spoke then sets its offset,
// re-stamps the pending changes that are ahead of the corrected clock and
// handshakes again. `future_hlc` (hub_replay.go) catches a spoke that ignores
// the correction and forges HLCs from the future, change by change.

func absMs(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 0 {
		return -ms
	}
	return ms
}

// recordSkew raises the max-skew metric of a node (health block).
func (m *Module) recordSkew(nodeID string, skewMs int64) {
	if skewMs <= 0 {
		return
	}
	_, _ = m.app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET max_skew_ms={:s} WHERE id={:id} AND COALESCE(max_skew_ms,0)<{:s}").
		Bind(dbx.Params{"s": skewMs, "id": nodeID}).Execute()
}

// pushPrechecks runs the whole-request checks of a push: clock drift and
// schema version. It reports whether it already answered.
func (m *Module) pushPrechecks(e *core.RequestEvent, nodeID string, req *proto.PushRequest) (bool, error) {
	now := m.now()
	drift := maxDrift()
	var off float64
	_ = e.App.DB().NewQuery("SELECT COALESCE(clock_offset_ms,0) FROM " + NodesCollection + " WHERE id={:id}").Bind(dbx.Params{"id": nodeID}).Row(&off)
	skew := int64(off)
	if skew < 0 {
		skew = -skew
	}
	if ct, err := time.Parse(time.RFC3339Nano, req.ClientTime); err == nil {
		skew = max(skew, absMs(now.Sub(ct)))
		if s := absMs(now.Sub(ct)); s > 0 {
			m.recordSkew(nodeID, s)
		}
	}
	if skew > drift.Milliseconds() {
		return true, syncErr(e, http.StatusConflict, proto.CodeClockDrift,
			"The clock of this node is out of tolerance; handshake again with a corrected clock.",
			map[string]any{"offset_ms": int64(off), "skew_ms": skew, "max_drift_ms": drift.Milliseconds(), "server_time": now.UTC().Format(proto.TimeLayout)})
	}
	if hub := m.schemaVersion(); req.SchemaVersion != hub {
		return true, syncErr(e, http.StatusConflict, proto.CodeSchemaBehind,
			"The schema version of this node differs from the hub; handshake to apply the bundles.",
			map[string]any{"schema_version": hub, "node_schema_version": req.SchemaVersion})
	}
	return false, nil
}
