//go:build !no_crypto

package crypto

import (
	"fmt"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Bulk operations on a synced collection (docs/SYNC_DESIGN.md §7.6).
//
//   - They run on the HUB only. A spoke takes its fields and its data keys from
//     the hub; a local rotate would create a version the hub later reaches with
//     another key (the node could not join any more), a local retire would blank
//     a version the hub still uses.
//   - The sweeps rewrite the table with raw SQL. On the hub every touched record
//     also gets a change row (kernel.SyncSweeper.RecordSweep, in the same
//     transaction), so that the nodes converge on the new ciphertext.
//   - A process that has a sync policy for the collection but no sync module
//     (TOKI_SYNC_ROLE unset in the shell that runs `toki crypto`) refuses: its
//     sweep could not be recorded.

// syncCheck refuses an operation that must not run in this process.
func (m *Module) syncCheck(col *core.Collection, op string) error {
	sw := kernel.SyncSweeperOf(m.app)
	if sw == nil {
		if m.hasSyncPolicy(col) {
			return fmt.Errorf("crypto %s: %s has a sync policy, but this process does not run the sync module (set TOKI_SYNC_ROLE=hub): the rewrite would not reach the nodes", op, col.Name)
		}
		return nil
	}
	if !sw.IsSynced(col.Id) {
		return nil
	}
	if sw.SyncRole() != "hub" {
		return fmt.Errorf("crypto %s: %s is synced from a hub and this node has sync role %s; run it on the hub (a local key or field change would stop this node from joining)", op, col.Name, sw.SyncRole())
	}
	return nil
}

// sweeperFor returns the sweeper that must record rewrites of col (the hub of a
// synced collection), or nil.
func (m *Module) sweeperFor(col *core.Collection) kernel.SyncSweeper {
	sw := kernel.SyncSweeperOf(m.app)
	if sw == nil || sw.SyncRole() != "hub" || !sw.IsSynced(col.Id) {
		return nil
	}
	return sw
}

// hasSyncPolicy reads `_sync_policies` directly: the crypto module does not
// import the sync module.
func (m *Module) hasSyncPolicy(col *core.Collection) bool {
	if !m.app.HasTable("_sync_policies") {
		return false
	}
	recs, err := m.app.FindAllRecords("_sync_policies")
	if err != nil {
		return true // cannot tell: refuse
	}
	for _, r := range recs {
		if !r.GetBool("enabled") || r.GetString("direction") == "none" {
			continue
		}
		ref := r.GetString("collection")
		if ref == col.Id || ref == col.Name {
			return true
		}
	}
	return false
}
