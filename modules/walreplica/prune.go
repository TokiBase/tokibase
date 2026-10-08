//go:build !no_replica

package walreplica

import (
	"context"
	"fmt"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"
)

// compactionLevels are the LTX levels below the snapshot level that the store compacts into.
var compactionLevels = []int{0, 1, 2, 3}

// PruneResult is what a prune removed from one database of a replica.
type PruneResult struct {
	Name      string `json:"name"`
	Snapshots int    `json:"snapshots"` // snapshots deleted
	Files     int    `json:"files"`     // all LTX files deleted, snapshots included
	Bytes     int64  `json:"bytes"`     // bytes freed
}

// pruneClient deletes the restore points older than cutoff from one replica:
// every snapshot created before cutoff (the newest snapshot is always kept)
// and the lower level files that only an older snapshot needed. It uses
// the same floor as Litestream's own retention (the max TXID of the newest
// deleted snapshot), so a restore from any remaining snapshot still finds all
// the files it needs. With dryRun nothing is deleted.
func pruneClient(ctx context.Context, name string, client litestream.ReplicaClient, cutoff time.Time, dryRun bool) (PruneResult, error) {
	res := PruneResult{Name: name}

	snaps, err := listLevel(ctx, client, litestream.SnapshotLevel)
	if err != nil {
		return res, err
	}
	var doomed []*ltx.FileInfo
	var floor ltx.TXID
	for i, s := range snaps {
		if i == len(snaps)-1 || !s.CreatedAt.Before(cutoff) {
			break // keep the newest snapshot and everything from the first one inside the window
		}
		doomed = append(doomed, s)
		floor = s.MaxTXID
	}
	res.Snapshots = len(doomed)
	for _, s := range doomed {
		res.Files++
		res.Bytes += s.Size
	}

	var all [][]*ltx.FileInfo
	all = append(all, doomed)
	if floor > 0 {
		for _, lvl := range compactionLevels[1:] {
			files, err := listLevel(ctx, client, lvl)
			if err != nil {
				return res, err
			}
			var old []*ltx.FileInfo
			for i, f := range files {
				if f.MaxTXID >= floor || i == len(files)-1 {
					break
				}
				old = append(old, f)
				res.Files++
				res.Bytes += f.Size
			}
			all = append(all, old)
		}
	}

	if dryRun {
		return res, nil
	}
	for _, files := range all {
		if len(files) == 0 {
			continue
		}
		if err := client.DeleteLTXFiles(ctx, files); err != nil {
			return res, fmt.Errorf("walreplica: %s: delete ltx files: %w", name, err)
		}
	}
	return res, nil
}

func listLevel(ctx context.Context, client litestream.ReplicaClient, level int) ([]*ltx.FileInfo, error) {
	itr, err := client.LTXFiles(ctx, level, 0, false)
	if err != nil {
		return nil, fmt.Errorf("walreplica: list level %d: %w", level, err)
	}
	defer itr.Close()
	var out []*ltx.FileInfo
	for itr.Next() {
		out = append(out, itr.Item())
	}
	if err := itr.Err(); err != nil {
		return nil, fmt.Errorf("walreplica: list level %d: %w", level, err)
	}
	return out, nil
}

// PruneOptions configures [Prune].
type PruneOptions struct {
	// Retention keeps the restore points newer than now-Retention. Zero keeps
	// only the newest snapshot and the files after it.
	Retention time.Duration
	// DryRun reports what would be deleted without deleting.
	DryRun bool
}

// Prune removes expired snapshots and the LTX files only they needed from the
// replica at url (no running app needed; safe next to a running server, which
// deletes the same files itself). The newest snapshot is never deleted.
func Prune(ctx context.Context, url string, opts PruneOptions) ([]PruneResult, error) {
	var out []PruneResult
	cutoff := time.Now().Add(-opts.Retention)
	for _, name := range []string{dataName, auxName} {
		client, err := openClient(ctx, url, name)
		if err != nil {
			return nil, err
		}
		res, err := pruneClient(ctx, name, client, cutoff, opts.DryRun)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

func openClient(ctx context.Context, url, name string) (litestream.ReplicaClient, error) {
	sub, err := subURL(url, name)
	if err != nil {
		return nil, err
	}
	client, err := litestream.NewReplicaClientFromURL(sub)
	if err != nil {
		return nil, fmt.Errorf("walreplica: %w", err)
	}
	if err := client.Init(ctx); err != nil {
		return nil, fmt.Errorf("walreplica: %s: %w", name, err)
	}
	return client, nil
}
