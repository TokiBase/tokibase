//go:build !no_sync

package sync

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
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
		ok, err := m.completeSnapshot(nodeID, req.SnapshotID)
		if err != nil {
			return err
		}
		if !ok {
			return syncErr(e, http.StatusGone, proto.CodeSnapshotExpired, "The snapshot id is invalid, expired or void; start a new snapshot.", nil)
		}
	}
	resp := proto.AckResponse{OK: true, DigestMismatch: []string{}}
	if len(req.Digest) > 0 && through >= m.headSeq() {
		vw := newViewer(e.App, nodeID, serviceActor(e.App, nodeID))
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
			if p, perr := m.pol.For(col); perr == nil && p != nil {
				if p.PartField != "" || m.viewScoped(col, p) {
					continue // the node holds a subset: its digest can not equal the hub's
				}
				// fieldperm hides fields from the node: it stores reduced rows
				if hid, err := m.hasHiddenFields(e.App, vw, col, p); err != nil || hid {
					continue
				}
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

// digestHiddenProbe bounds how many rows hasHiddenFields reads. A collection
// bigger than that is judged by its first rows (a fieldperm rule that hides a
// field only from later rows would then be missed, and the auto-heal limit
// of the node, not the digest, protects it).
const digestHiddenProbe = 2000

// hasHiddenFields tells whether fieldperm hides any synced field of col from the
// service actor of the node on one of the first rows: such a node stores
// reduced rows, whose hash differs from the hub's.
func (m *Module) hasHiddenFields(app kernel.App, vw *viewer, col *core.Collection, p *policy) (bool, error) {
	var recs []*core.Record
	if err := app.RecordQuery(col).OrderBy("id ASC").Limit(digestHiddenProbe).All(&recs); err != nil {
		return false, err
	}
	for _, rec := range recs {
		vr, err := m.visibleForNode(vw, p, rec)
		if err != nil {
			return false, err
		}
		if len(vr.hidden) > 0 {
			return true, nil
		}
	}
	return false, nil
}
