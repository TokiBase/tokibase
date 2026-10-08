//go:build !no_sync

package sync

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// ackHandler is POST /api/sync/ack (docs/SYNC_DESIGN.md §3.6, ack part only;
// compaction is PR6). The digest of a collection is compared with the hub's
// only when the node has pulled everything, otherwise the comparison would
// measure the lag.
func (m *Module) ackHandler(e *core.RequestEvent) error {
	nodeID := NodeFrom(e)
	var req proto.AckRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil || req.PulledThrough < 0 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	through := m.ackPulled(nodeID, req.PulledThrough)
	if req.SnapshotID != "" {
		// the node finished a snapshot bootstrap (§3.9): active again, pulled up to start_seq
		if _, err := m.completeSnapshot(nodeID, req.SnapshotID); err != nil {
			return err
		}
	}
	resp := proto.AckResponse{OK: true, DigestMismatch: []string{}}
	if len(req.Digest) > 0 && through >= m.headSeq() {
		resp.DigestChecked = true
		keys := make([]string, 0, len(req.Digest))
		for k := range req.Digest {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			col, err := e.App.FindCachedCollectionByNameOrId(k)
			if err != nil {
				resp.DigestMismatch = append(resp.DigestMismatch, k)
				continue
			}
			if p, perr := m.pol.For(col); perr == nil && p != nil && (p.PartField != "" || p.PullViewRule) {
				continue // the node holds a subset: its digest can not equal the hub's
			}
			d, _, err := metaDigest(e.App.DB(), col.Id)
			if err != nil {
				return err
			}
			if d != req.Digest[k] {
				resp.DigestMismatch = append(resp.DigestMismatch, k)
			}
		}
	}
	return e.JSON(http.StatusOK, resp)
}
