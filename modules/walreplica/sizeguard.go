//go:build !no_replica

package walreplica

import (
	"context"
	"log/slog"
	"time"

	"github.com/benbjohnson/litestream"
)

// Size guard timings.
const (
	sizeGuardFirstCheck = time.Minute
	sizeGuardInterval   = 5 * time.Minute
)

// replicaBytes sums the size of every LTX file of every database.
func replicaBytes(ctx context.Context, clients map[string]litestream.ReplicaClient) (int64, error) {
	var total int64
	for _, c := range clients {
		for _, lvl := range append(append([]int{}, compactionLevels...), litestream.SnapshotLevel) {
			files, err := listLevel(ctx, c, lvl)
			if err != nil {
				return 0, err
			}
			for _, f := range files {
				total += f.Size
			}
		}
	}
	return total, nil
}

// guardSize warns when the replica is larger than maxBytes and then removes
// expired restore points, shrinking the retention window step by step
// (retention/2, /4, ... down to the newest snapshot only) until it fits.
// It returns the size afterwards.
func guardSize(ctx context.Context, logger *slog.Logger, clients map[string]litestream.ReplicaClient, maxBytes int64, retention time.Duration, now time.Time) (int64, error) {
	size, err := replicaBytes(ctx, clients)
	if err != nil || maxBytes <= 0 || size <= maxBytes {
		return size, err
	}
	logger.Warn("walreplica: replica is larger than "+EnvMaxMB+", pruning expired restore points",
		slog.Int64("bytes", size), slog.Int64("maxBytes", maxBytes), slog.String("retention", retention.String()))

	for window := retention / 2; ; window /= 2 {
		if window < time.Minute {
			window = 0
		}
		for name, c := range clients {
			res, err := pruneClient(ctx, name, c, now.Add(-window), false)
			if err != nil {
				return size, err
			}
			if res.Files > 0 {
				logger.Warn("walreplica: emergency prune", slog.String("replica", name),
					slog.String("keeps", window.String()), slog.Int("files", res.Files), slog.Int64("freedBytes", res.Bytes))
			}
		}
		if size, err = replicaBytes(ctx, clients); err != nil || size <= maxBytes {
			return size, err
		}
		if window == 0 {
			logger.Error("walreplica: replica still larger than "+EnvMaxMB+" with only the newest snapshot kept; raise the limit or shrink the database (logs?)",
				slog.Int64("bytes", size), slog.Int64("maxBytes", maxBytes))
			return size, nil
		}
	}
}

// runSizeGuard checks the replica size periodically until ctx ends.
func (r *replicator) runSizeGuard(ctx context.Context, logger *slog.Logger) {
	if r.cfg.MaxMB <= 0 {
		return
	}
	clients := map[string]litestream.ReplicaClient{}
	for _, st := range r.dbs {
		clients[st.name] = st.replica.Client
	}
	maxBytes := r.cfg.MaxMB * 1024 * 1024

	timer := time.NewTimer(sizeGuardFirstCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := guardSize(ctx, logger, clients, maxBytes, r.cfg.Retention, time.Now()); err != nil && ctx.Err() == nil {
			logger.Error("walreplica: size check failed", slog.String("error", err.Error()))
		}
		timer.Reset(sizeGuardInterval)
	}
}
