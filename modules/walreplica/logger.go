package walreplica

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const errorLogInterval = 30 * time.Second

// captureHandler receives the Litestream logs. Warnings and errors are
// forwarded to the app logger (errors at most once per errorLogInterval),
// the rest is dropped. Errors are also remembered for Status.
type captureHandler struct {
	app     slog.Handler
	onError func(msg string, at time.Time) // optional

	lim *limiter
}

type limiter struct {
	mu      sync.Mutex
	lastLog time.Time
}

func (h *captureHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }

func (h *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		if h.onError != nil {
			msg := r.Message
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == "error" || a.Key == "err" {
					msg += ": " + a.Value.String()
					return false
				}
				return true
			})
			h.onError(msg, r.Time)
		}

		h.lim.mu.Lock()
		skip := !h.lim.lastLog.IsZero() && time.Since(h.lim.lastLog) < errorLogInterval
		if !skip {
			h.lim.lastLog = time.Now()
		}
		h.lim.mu.Unlock()
		if skip {
			return nil
		}
	}
	return h.app.Handle(ctx, r)
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{app: h.app.WithAttrs(attrs), onError: h.onError, lim: h.lim}
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{app: h.app.WithGroup(name), onError: h.onError, lim: h.lim}
}
