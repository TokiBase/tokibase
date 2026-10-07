//go:build !no_replica

package walreplica

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/benbjohnson/litestream"
)

// RestoreOptions configures [Restore].
type RestoreOptions struct {
	// Timestamp restores the state as of this time (zero = latest).
	Timestamp time.Time

	// Overwrite replaces an existing data.db / auxiliary.db in the destination.
	Overwrite bool
}

// Restore rebuilds data.db and auxiliary.db from the replica at url into
// destDataDir (a pb_data directory). It refuses to run when destDataDir already
// contains data.db unless opts.Overwrite is set. auxiliary.db (logs) is skipped
// when the replica holds no copy of it; data.db is required.
func Restore(ctx context.Context, url, destDataDir string, opts RestoreOptions) error {
	dataPath := filepath.Join(destDataDir, dataDBFile)
	auxPath := filepath.Join(destDataDir, auxDBFile)

	if _, err := os.Stat(dataPath); err == nil {
		if !opts.Overwrite {
			return fmt.Errorf("walreplica: %s already exists (use overwrite to replace it)", dataPath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	// resolve both clients before touching the destination
	type target struct {
		name, path string
		replica    *litestream.Replica
		required   bool
	}
	var targets []target
	for _, t := range []struct {
		name, path string
		required   bool
	}{{dataName, dataPath, true}, {auxName, auxPath, false}} {
		sub, err := subURL(url, t.name)
		if err != nil {
			return err
		}
		client, err := litestream.NewReplicaClientFromURL(sub)
		if err != nil {
			return fmt.Errorf("walreplica: %w", err)
		}
		if err := client.Init(ctx); err != nil {
			return fmt.Errorf("walreplica: %s: %w", t.name, err)
		}
		targets = append(targets, target{t.name, t.path, litestream.NewReplicaWithClient(nil, client), t.required})
	}

	if err := os.MkdirAll(destDataDir, 0o700); err != nil {
		return err
	}

	for _, t := range targets {
		if opts.Overwrite {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Remove(t.path + suffix); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			// stale local replicator state would disagree with the restored file
			_ = os.RemoveAll(filepath.Join(destDataDir, "."+filepath.Base(t.path)+"-litestream"))
		}

		ro := litestream.NewRestoreOptions()
		ro.OutputPath = t.path
		ro.Timestamp = opts.Timestamp
		ro.IntegrityCheck = litestream.IntegrityCheckQuick

		if err := t.replica.Restore(ctx, ro); err != nil {
			if !t.required && (errors.Is(err, litestream.ErrNoSnapshots) || errors.Is(err, litestream.ErrTxNotAvailable)) {
				continue
			}
			return fmt.Errorf("walreplica: restore %s: %w", t.name, err)
		}
	}

	return nil
}
