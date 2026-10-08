//go:build !no_sync

package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// Hub epoch (docs/SYNC_DESIGN.md §3.9). The epoch is a random id kept in
// `_sync_state`. It changes whenever the hub may have lost changes it had
// handed out: after a backup restore, after a walreplica promote, and when the
// hub head is below the highest head it ever saw. A spoke that sees a new epoch
// follows docs/SYNC_DESIGN.md §3.3: it re-bootstraps unless it can prove that
// its pull cursor is at or below the head at which EVERY later epoch began (the
// hub kept all seqs up to there), see epochRequiresRebootstrap. Hub-origin
// writes that a restore lost are reconciled by that re-bootstrap; spoke-origin
// changes the hub lost are sent again from the spoke's acked rows.
const (
	keyEpochSeq    = "epoch_seq"
	keyEpochHist   = "epoch_history"
	keyMaxSeqSeen  = "max_seq_seen"
	keyPromoteSeen = "promote_seen"

	// RestoreMarker is written into the data dir by the OnBackupRestore hook. A
	// restore replaces data.db, so the epoch cannot be changed in the old
	// database: the marker survives the swap (it is excluded from the move) and
	// the next boot renews the epoch.
	RestoreMarker = ".toki-sync-restored"

	// stateSidecar keeps max_seq_seen and the epoch OUTSIDE data.db, so a restore
	// that swaps the database (also a manual one: copying data.db, a VM rollback)
	// cannot take the high-water mark back with it.
	stateSidecar = ".toki-sync-state.json"

	// maxEpochHist bounds the epoch history.
	maxEpochHist = 32

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
	if side := readSidecar(dir); side.MaxSeqSeen > seenMax {
		seenMax = side.MaxSeqSeen // the database was swapped for an older one behind our back
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
		hist, herr := loadEpochHist(st, old, h.epochSeq)
		if herr != nil {
			return herr
		}
		hist = append(hist, epochEntry{Epoch: e, Seq: head})
		if len(hist) > maxEpochHist {
			hist = hist[len(hist)-maxEpochHist:]
		}
		if err := saveEpochHist(st, hist); err != nil {
			return err
		}
		h.epoch, h.epochSeq, h.epochHist = e, head, hist
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
	if h.epochHist == nil {
		hist, err := loadEpochHist(st, h.epoch, h.epochSeq)
		if err != nil {
			return err
		}
		h.epochHist = hist
		if err := saveEpochHist(st, hist); err != nil {
			return err
		}
	}
	top := max(seenMax, head)
	m.seenHead.Store(top)
	writeSidecar(dir, sidecar{MaxSeqSeen: top, Epoch: h.epoch})
	return st.Set(keyMaxSeqSeen, strconv.FormatInt(top, 10))
}

// epochEntry is one epoch of the history: its id and the head when it began.
type epochEntry struct {
	Epoch string `json:"e"`
	Seq   int64  `json:"s"`
}

// loadEpochHist reads the epoch history and makes sure it ends with `cur`
// (a hub from before the history existed starts with its current epoch).
func loadEpochHist(st dbState, cur string, curSeq int64) ([]epochEntry, error) {
	var hist []epochEntry
	if v, ok, err := st.Get(keyEpochHist); err != nil {
		return nil, err
	} else if ok {
		_ = json.Unmarshal([]byte(v), &hist)
	}
	if len(hist) == 0 || hist[len(hist)-1].Epoch != cur {
		hist = append(hist, epochEntry{Epoch: cur, Seq: curSeq})
	}
	return hist, nil
}

func saveEpochHist(st dbState, hist []epochEntry) error {
	b, _ := json.Marshal(hist)
	return st.Set(keyEpochHist, string(b))
}

// epochRequiresRebootstrap tells whether a spoke that last saw epoch
// `spokeEpoch` and pulled up to `pullAfter` has to re-bootstrap (§3.3). Only
// when the cursor is at or below the head at which every later epoch began is
// the hub's log up to the cursor the same one the spoke applied; anything else
// (an epoch the history does not know, a cursor inside lost history, two epoch
// changes that the cursor straddles) could leave data the hub never had.
func (m *Module) epochRequiresRebootstrap(spokeEpoch string, pullAfter int64) bool {
	h := m.hub
	if h == nil || spokeEpoch == "" || spokeEpoch == h.epoch {
		return false
	}
	idx := -1
	for i, e := range h.epochHist {
		if e.Epoch == spokeEpoch {
			idx = i
		}
	}
	if idx < 0 || idx == len(h.epochHist)-1 {
		return true
	}
	for _, e := range h.epochHist[idx+1:] {
		if pullAfter > e.Seq {
			return true
		}
	}
	return false
}

// sidecar is the content of the state file next to data.db.
type sidecar struct {
	MaxSeqSeen int64  `json:"max_seq_seen"`
	Epoch      string `json:"epoch"`
}

func readSidecar(dir string) sidecar {
	var s sidecar
	if b, err := os.ReadFile(filepath.Join(dir, stateSidecar)); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func writeSidecar(dir string, s sidecar) {
	b, _ := json.Marshal(s)
	tmp := filepath.Join(dir, stateSidecar+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, stateSidecar))
	}
}

var lastSidecarWrite atomic.Int64

// noteHead raises max_seq_seen to the current head, in the database and in the
// sidecar file next to it (called on handshakes, pushes, pulls and compaction;
// cheap when the head did not move). The boot check compares it with the head.
func (m *Module) noteHead() {
	if m.role != RoleHub || m.hub == nil {
		return
	}
	head := m.headSeq()
	if head <= m.seenHead.Load() {
		return
	}
	m.seenHead.Store(head)
	if now := time.Now().UnixNano(); now-lastSidecarWrite.Load() > int64(250*time.Millisecond) {
		lastSidecarWrite.Store(now)
		writeSidecar(m.app.DataDir(), sidecar{MaxSeqSeen: head, Epoch: m.hub.epoch})
	}
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
				e.Exclude = append(e.Exclude, RestoreMarker, stateSidecar) // survive the data dir swap
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
