//go:build !no_webhooks

package webhooks

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

// pollInterval is how often idle workers look for due deliveries.
var pollInterval = time.Second

const pruneEvery = time.Hour

func retention() time.Duration {
	if h, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_WEBHOOK_RETENTION_HOURS"))); err == nil && h > 0 {
		return time.Duration(h) * time.Hour
	}
	return 7 * 24 * time.Hour
}

// deadRetention is how long failed/dead deliveries are kept (default 30 days,
// env TOKI_WEBHOOK_DEAD_RETENTION_HOURS).
func deadRetention() time.Duration {
	if h, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_WEBHOOK_DEAD_RETENTION_HOURS"))); err == nil && h > 0 {
		return time.Duration(h) * time.Hour
	}
	return 30 * 24 * time.Hour
}

func (m *Module) worker(ctx context.Context, janitor bool) {
	defer m.wg.Done()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	lastPrune := time.Time{}
	for {
		if ctx.Err() != nil {
			return
		}
		if id, ok := claimNext(m.app, nowFn()); ok {
			if err := deliver(ctx, m.app, id); err != nil {
				m.app.Logger().Warn("webhooks: delivery error", "delivery", id, "error", err)
			}
			continue
		}
		if janitor && time.Since(lastPrune) > pruneEvery {
			lastPrune = time.Now()
			pruneDelivered(m.app, nowFn().Add(-retention()))
			pruneUnfinished(m.app, nowFn().Add(-deadRetention()))
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-tick.C:
		}
	}
}
