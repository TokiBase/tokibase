//go:build !no_replica

package walreplica

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pocketbase/dbx"
	_ "modernc.org/sqlite"
)

// PromotedMarker is the file written into a promoted pb_data directory.
const PromotedMarker = ".toki-promoted.json"

// PromoteOptions configures [Promote].
type PromoteOptions struct {
	// Timestamp restores the state as of this time (zero = latest).
	Timestamp time.Time

	// Force allows promoting into a directory that already holds data.db.
	// The existing directory is moved to <dir>.pre-promote-<unixts>, never deleted.
	Force bool
}

// PromoteResult describes a finished promotion.
type PromoteResult struct {
	FromURL      string            `json:"from_url"`
	Dir          string            `json:"dir"`
	MovedTo      string            `json:"moved_to,omitempty"`
	RestoredTXID map[string]uint64 `json:"restored_txid,omitempty"` // newest replica txid per database (latest restore)
	Timestamp    *time.Time        `json:"timestamp,omitempty"`     // set for a point in time restore
	Collections  int               `json:"collections"`
	PromotedAt   time.Time         `json:"promoted_at"`
	Hostname     string            `json:"hostname"`
}

var auditSink func(PromoteResult)

// SetAuditSink registers a function called after every successful [Promote]
// (used to write the `replica.promote` audit entry).
func SetAuditSink(fn func(PromoteResult)) { auditSink = fn }

// Promote turns a replica into a standalone pb_data directory: it restores both
// databases, verifies them (PRAGMA integrity_check, collection count) and writes
// <dir>/.toki-promoted.json. The caller then starts the server on dir with a NEW
// replica url; see docs/modules/walreplica.md.
func Promote(ctx context.Context, replicaURL, dir string, opts PromoteOptions) (*PromoteResult, error) {
	res := &PromoteResult{FromURL: redactURL(replicaURL), Dir: dir, PromotedAt: time.Now().UTC()}
	res.Hostname, _ = os.Hostname()
	if !opts.Timestamp.IsZero() {
		t := opts.Timestamp.UTC()
		res.Timestamp = &t
	}

	// fail fast on a bad url or an empty replica before touching the directory
	infos, err := Inspect(ctx, replicaURL)
	if err != nil {
		return nil, err
	}
	if len(infos) == 0 || infos[0].LatestTXID == 0 {
		return nil, errors.New("walreplica: the replica holds no data.db yet, nothing to promote")
	}
	res.RestoredTXID = map[string]uint64{}
	for _, i := range infos {
		res.RestoredTXID[i.Name] = i.LatestTXID
	}

	if _, err := os.Stat(filepath.Join(dir, dataDBFile)); err == nil {
		if !opts.Force {
			return nil, fmt.Errorf("walreplica: %s already exists; promote into an empty directory or pass --force to move %s aside", filepath.Join(dir, dataDBFile), dir)
		}
		res.MovedTo = fmt.Sprintf("%s.pre-promote-%d", filepath.Clean(dir), time.Now().Unix())
		if err := os.Rename(dir, res.MovedTo); err != nil {
			return nil, fmt.Errorf("walreplica: move existing dir aside: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := Restore(ctx, replicaURL, dir, RestoreOptions{Timestamp: opts.Timestamp}); err != nil {
		return nil, err
	}

	for _, f := range []string{dataDBFile, auxDBFile} {
		p := filepath.Join(dir, f)
		if _, err := os.Stat(p); os.IsNotExist(err) && f == auxDBFile {
			continue
		}
		if err := integrityCheck(ctx, p); err != nil {
			return nil, fmt.Errorf("walreplica: %s failed verification: %w", f, err)
		}
	}
	if res.Collections, err = countCollections(ctx, filepath.Join(dir, dataDBFile)); err != nil {
		return nil, fmt.Errorf("walreplica: %w", err)
	}
	if res.Collections == 0 {
		return nil, errors.New("walreplica: restored data.db has no collections")
	}
	if res.Timestamp != nil {
		res.RestoredTXID = nil // latest txid does not describe a point in time restore
	}

	raw, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, PromotedMarker), raw, 0o600); err != nil {
		return nil, err
	}

	if auditSink != nil {
		auditSink(*res)
	}
	return res, nil
}

func openRO(p string) (*dbx.DB, error) {
	return dbx.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro&_pragma=busy_timeout(5000)")
}

func integrityCheck(ctx context.Context, p string) error {
	db, err := openRO(p)
	if err != nil {
		return err
	}
	defer db.Close()
	var rows []string
	if err := db.NewQuery("PRAGMA integrity_check").WithContext(ctx).Column(&rows); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if len(rows) != 1 || rows[0] != "ok" {
		return fmt.Errorf("integrity_check: %v", rows)
	}
	return nil
}

func countCollections(ctx context.Context, p string) (int, error) {
	db, err := openRO(p)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM _collections").WithContext(ctx).Row(&n); err != nil {
		return 0, fmt.Errorf("count collections: %w", err)
	}
	return n, nil
}
