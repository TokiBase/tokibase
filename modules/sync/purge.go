//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

// Purge and legal tombstones (docs/SYNC_DESIGN.md §7.3). A purge is the one
// operation that erases data from the replication log as well: the record is
// deleted, a `legal` tombstone is written (immutable: database triggers refuse
// to update or delete it, and it is never pruned), every `_changes` row of the
// record loses its patch, and an op `p` row carries the erasure to the spokes.

// AuditPurge is the audit action of a purge.
const AuditPurge = "sync.purge"

// PurgeResult describes what a purge did.
type PurgeResult struct {
	Collection string `json:"collection"`
	Record     string `json:"record"`
	// Deleted is true when the record still existed.
	Deleted bool `json:"deleted"`
	// Already is true when a legal tombstone existed (the call only repeated the erasure).
	Already bool `json:"already"`
	// Blanked is the number of `_changes` rows whose patch was erased.
	Blanked int64 `json:"blanked"`
	// Seq is the hub seq of the `p` row (0 when Already).
	Seq int64 `json:"seq"`
	// Scrubbed is true when the WAL was checkpointed and truncated after the
	// purge. Copies in walreplica streams, backups and file system snapshots
	// are NOT reached (docs/modules/sync.md, "Purge").
	Scrubbed bool `json:"scrubbed"`
}

// secureDelete switches PRAGMA secure_delete on for the connection of db and
// returns the function that restores the previous setting.
func secureDelete(db dbx.Builder) func() {
	var old int
	if err := db.NewQuery("PRAGMA secure_delete").Row(&old); err != nil {
		return func() {}
	}
	if _, err := db.NewQuery("PRAGMA secure_delete=ON").Execute(); err != nil {
		return func() {}
	}
	return func() {
		_, _ = db.NewQuery("PRAGMA secure_delete=" + strconv.Itoa(old)).Execute()
	}
}

// scrubFreePages returns freed space of an incremental-vacuum database to the OS
// and truncates the WAL so that old page images of the erased rows leave the
// live database files. It reports whether the checkpoint completed.
func (m *Module) scrubFreePages() bool {
	db := m.app.NonconcurrentDB()
	_, _ = db.NewQuery("PRAGMA incremental_vacuum").Execute()
	var busy, logFrames, ckpt int
	if err := db.NewQuery("PRAGMA wal_checkpoint(TRUNCATE)").Row(&busy, &logFrames, &ckpt); err != nil || busy != 0 {
		m.app.Logger().Warn("sync: purge could not truncate the WAL (readers active); run `toki sync compact --vacuum` later", "busy", busy, "error", err)
		return false
	}
	return true
}

// ErrPurgeInput marks a purge request that is wrong (HTTP 400).
var ErrPurgeInput = errors.New("sync: invalid purge request")

// putLegalTombstone writes the legal tombstone, replacing a delete tombstone.
// An existing legal tombstone is left alone (the trigger would abort an update).
func putLegalTombstone(db dbx.Builder, colId, id string, h int64, node, actor, reason, created string) error {
	_, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, 'legal', {:h}, {:n}, {:a}, {:why}, {:t})
  ON CONFLICT(collection, record) DO UPDATE SET kind='legal', hlc=excluded.hlc, node=excluded.node,
    actor=excluded.actor, reason=excluded.reason, created=excluded.created
  WHERE _sync_tombstones.kind='delete'`).
		Bind(dbx.Params{"c": colId, "r": id, "h": h, "n": node, "a": actor, "why": reason, "t": created}).Execute()
	return err
}

// blankChanges erases the patch and hash of every `_changes` row of a record
// (own rows, rejected and parked rows, reverts) and returns the row count.
//
// Rows are matched by collection id AND name: a node can push the name, and the
// hub stores refused and parked rows as pushed (P56-7). colName may be "".
func blankChanges(db dbx.Builder, colId, colName, id string) (int64, error) {
	if colName == "" {
		colName = colId
	}
	res, err := db.NewQuery("UPDATE _changes SET patch='{}', hash=NULL WHERE collection IN ({:c},{:n}) AND record={:r} AND (patch!='{}' OR hash IS NOT NULL)").
		Bind(dbx.Params{"c": colId, "n": colName, "r": id}).Execute()
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// blankConflicts erases the copies of the record data that `_sync_conflicts`
// rows keep (every JSON field and the note).
func blankConflicts(tx kernel.App, colId, id string) error {
	cc, err := tx.FindCachedCollectionByNameOrId(ConflictsCollection)
	if err != nil {
		return nil // not a hub, or no conflicts yet
	}
	var sets []string
	for _, f := range cc.Fields {
		switch f.Type() {
		case kernel.FieldTypeJSON:
			sets = append(sets, "[["+f.GetName()+"]]=NULL")
		}
	}
	if cc.Fields.GetByName("note") != nil {
		sets = append(sets, "[[note]]=''")
	}
	if len(sets) == 0 {
		return nil
	}
	ids := []string{colId}
	if col, err := tx.FindCachedCollectionByNameOrId(colId); err == nil {
		ids = append(ids, col.Name)
	}
	for _, ref := range ids {
		if _, err := tx.NonconcurrentDB().NewQuery("UPDATE {{" + ConflictsCollection + "}} SET " + strings.Join(sets, ", ") +
			" WHERE [[collection]]={:c} AND [[record]]={:r}").Bind(dbx.Params{"c": ref, "r": id}).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// Purge erases a record for good (hub only). reason is kept in the tombstone
// and in the `p` row; actor identifies the operator in the audit entry.
func (m *Module) Purge(collection, id, reason, actor string, cli bool) (*PurgeResult, error) {
	if m.role != RoleHub || !m.hubReady() {
		return nil, errors.New("sync: purge needs a ready hub (TOKI_SYNC_ROLE=hub)")
	}
	collection, id, reason = strings.TrimSpace(collection), strings.TrimSpace(id), strings.TrimSpace(reason)
	if collection == "" || id == "" {
		return nil, fmt.Errorf("%w: collection and record are required", ErrPurgeInput)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required", ErrPurgeInput)
	}
	if len(reason) > 1000 {
		return nil, fmt.Errorf("%w: the reason is longer than 1000 characters", ErrPurgeInput)
	}
	col, err := m.app.FindCachedCollectionByNameOrId(collection)
	if err != nil {
		return nil, fmt.Errorf("%w: collection %q not found", ErrPurgeInput, collection)
	}
	if !eligible(col) {
		return nil, fmt.Errorf("%w: collection %q holds no synced data (system collection or view)", ErrPurgeInput, col.Name)
	}
	// purge is hub-local: it also works for a collection whose policy is none,
	// disabled or removed (old `_changes` rows still hold patches). p may be nil.
	p, err := m.pol.For(col)
	if err != nil {
		return nil, err
	}

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	res := &PurgeResult{Collection: col.Id, Record: id}
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		// the first statement is a write, so the SQLite write lock is taken before any read
		if _, err := db.NewQuery("UPDATE _sync_state SET value=value WHERE key={:k}").Bind(dbx.Params{"k": keyNodeID}).Execute(); err != nil {
			return err
		}
		// freed cells are zeroed instead of left in free pages (P56-8)
		defer secureDelete(db)()
		h := int64(m.Clock().Now())
		partOld := ""
		if tombstoneKind(db, col.Id, id) == "legal" {
			res.Already = true
		} else {
			if rec, _ := tx.FindRecordById(col.Id, id); rec != nil {
				partOld = partValue(rec, p)
				// origin "push": the capture hook stays out of the way, this function
				// writes the log row itself (as the hub apply does)
				ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{
					Mode: kernel.SyncModePush, Node: m.hub.id, HLC: uint64(h), ChangeID: "purge", Actor: actor,
				})
				if err := tx.DeleteWithContext(ctx, rec); err != nil {
					return err
				}
				res.Deleted = true
			}
			if err := putLegalTombstone(db, col.Id, id, h, m.hub.id, actor, reason, m.created()); err != nil {
				return err
			}
			if err := deleteMeta(db, col.Id, id); err != nil {
				return err
			}
		}
		n, err := blankChanges(db, col.Id, col.Name, id)
		if err != nil {
			return err
		}
		res.Blanked = n
		if err := blankConflicts(tx, col.Id, id); err != nil {
			return err
		}
		if _, err := db.NewQuery("DELETE FROM _sync_sent WHERE collection={:c} AND record={:r}").Bind(dbx.Params{"c": col.Id, "r": id}).Execute(); err != nil {
			return err
		}
		if res.Already {
			return nil
		}
		patch, _ := json.Marshal(map[string]any{"reason": reason})
		seq, err := m.insertHubRow(db, &hubRow{
			node: m.hub.id, hlc: h, col: col.Id, rec: id, op: OpPurge, patch: string(patch),
			actor: actor, status: StatusApplied, partOld: partOld,
		})
		res.Seq = seq
		return err
	})
	if err != nil {
		return nil, err
	}
	res.Scrubbed = m.scrubFreePages()
	if !res.Already {
		m.notifyHead()
	}
	emit(AuditPurge, col.Id, id, map[string]any{
		"legal": true, "reason": truncate(reason, 300), "deleted": res.Deleted, "already": res.Already,
		"blanked": res.Blanked, "actor": actor, "cli": cli,
	})
	return res, nil
}

// purgeRequest is the body of POST /api/sync/purge.
type purgeRequest struct {
	Collection string `json:"collection"`
	Record     string `json:"record"`
	Reason     string `json:"reason"`
	Legal      bool   `json:"legal"`
}

// purgeHandler is POST /api/sync/purge (superusers only).
func (m *Module) purgeHandler(e *core.RequestEvent) error {
	var req purgeRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	if !req.Legal {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "only a legal purge is supported: set \"legal\": true", nil)
	}
	actor := "rec:superuser"
	if e.Auth != nil {
		actor = "rec:" + e.Auth.Collection().Id + ":" + e.Auth.Id
	}
	res, err := m.Purge(req.Collection, req.Record, req.Reason, actor, false)
	if err != nil {
		if errors.Is(err, ErrPurgeInput) {
			return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, err.Error(), nil)
		}
		e.App.Logger().Error("sync: purge failed", "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "purge failed", nil)
	}
	return e.JSON(http.StatusOK, res)
}
