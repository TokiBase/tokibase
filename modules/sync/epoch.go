//go:build !no_sync

package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// Hub epoch (docs/SYNC_DESIGN.md §3.9). The epoch is a random id kept in
// `_sync_state`. It changes whenever the hub may have lost changes it had
// handed out: after a backup restore, after a walreplica promote, and when the
// hub head is below the highest head it ever saw. A spoke that sees a new epoch
// takes its pull cursor back to `epoch_seq` (the head when the epoch began) and
// sends again the acked changes it still keeps.
const (
	keyEpochSeq    = "epoch_seq"
	keyMaxSeqSeen  = "max_seq_seen"
	keyPromoteSeen = "promote_seen"

	// RestoreMarker is written into the data dir by the OnBackupRestore hook. A
	// restore replaces data.db, so the epoch cannot be changed in the old
	// database: the marker survives the swap (it is excluded from the move) and
	// the next boot renews the epoch.
	RestoreMarker = ".toki-sync-restored"

	// promotedMarker is the file walreplica.Promote writes into a promoted pb_data.
	promotedMarker = ".toki-promoted.json"

	// AuditEpoch is emitted when the epoch changes.
	AuditEpoch = "sync.epoch"
)

// initEpoch decides at boot whether the epoch is renewed and loads epoch_seq.
// It runs after the hub identity is loaded (m.hub is set).
func (m *Module) initEpoch(st dbState) error {
	h := m.hub
	head := m.headSeq()
	dir := m.app.DataDir()
	reason := ""

	marker := filepath.Join(dir, RestoreMarker)
	if _, err := os.Stat(marker); err == nil {
		reason = "backup_restore"
		_ = os.Remove(marker)
	}
	if b, err := os.ReadFile(filepath.Join(dir, promotedMarker)); err == nil {
		sum := sha256.Sum256(b)
		seen, _, gerr := st.Get(keyPromoteSeen)
		if gerr != nil {
			return gerr
		}
		if hexSum := hex.EncodeToString(sum[:]); seen != hexSum {
			if reason == "" {
				reason = "replica_promote"
			}
			if err := st.Set(keyPromoteSeen, hexSum); err != nil {
				return err
			}
		}
	}
	seenMax := int64(0)
	if v, ok, err := st.Get(keyMaxSeqSeen); err != nil {
		return err
	} else if ok {
		seenMax, _ = strconv.ParseInt(v, 10, 64)
	}
	if seenMax > head && reason == "" {
		reason = "head_regressed"
	}

	if reason != "" {
		e, err := randHex(12)
		if err != nil {
			return err
		}
		if err := st.Set(keyEpoch, e); err != nil {
			return err
		}
		if err := st.Set(keyEpochSeq, strconv.FormatInt(head, 10)); err != nil {
			return err
		}
		old := h.epoch
		h.epoch, h.epochSeq = e, head
		seenMax = head // the old high-water mark is void in the new epoch
		m.app.Logger().Warn("sync: the hub epoch changed", "reason", reason, "head", head, "max_seq_seen", seenMax)
		emit(AuditEpoch, "", h.id, map[string]any{"reason": reason, "old": old, "new": e, "head": head})
	} else if v, ok, err := st.Get(keyEpochSeq); err != nil {
		return err
	} else if ok {
		h.epochSeq, _ = strconv.ParseInt(v, 10, 64)
	} else {
		// an epoch created before epoch_seq existed: cursors are only safe up to the head now
		h.epochSeq = head
		if err := st.Set(keyEpochSeq, strconv.FormatInt(head, 10)); err != nil {
			return err
		}
	}
	return st.Set(keyMaxSeqSeen, strconv.FormatInt(max(seenMax, head), 10))
}

// noteHead raises max_seq_seen to the current head (cheap; called on handshakes,
// compaction and shutdown). The boot check compares it with the head.
func (m *Module) noteHead() {
	if m.role != RoleHub || m.hub == nil {
		return
	}
	head := m.headSeq()
	_, _ = m.app.NonconcurrentDB().NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, CAST({:v} AS TEXT)) " +
		"ON CONFLICT(key) DO UPDATE SET value=CAST({:v} AS TEXT) WHERE CAST(value AS INTEGER) < {:v}").
		Bind(map[string]any{"k": keyMaxSeqSeen, "v": head}).Execute()
}

// bindEpoch (hub) hooks the backup restore: a restore swaps data.db for an older
// one, so the marker file tells the next boot to renew the epoch.
func (m *Module) bindEpoch() {
	if m.role != RoleHub {
		return
	}
	m.app.OnBackupRestore().Bind(&hook.Handler[*kernel.BackupEvent]{
		Id: hookId + "restore",
		Func: func(e *kernel.BackupEvent) error {
			marker := filepath.Join(e.App.DataDir(), RestoreMarker)
			werr := os.WriteFile(marker, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
			if werr == nil {
				e.Exclude = append(e.Exclude, RestoreMarker) // survives the data dir swap
			} else {
				e.App.Logger().Error("sync: could not write the restore marker, spokes will not notice the restore", "error", werr)
			}
			if err := e.Next(); err != nil {
				_ = os.Remove(marker)
				return err
			}
			return nil
		},
	})
}
