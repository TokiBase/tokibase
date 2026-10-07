// Package denylog gives every 401, 403 and 429 response a structured log
// entry (Warn, attribute toki.deny=true) with the reason, so denials can be
// queried from the ordinary _logs table.
package denylog

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

const (
	hookId       = "__tokiDenyLog__"
	middlewareId = "__tokiDenyLogMiddleware__"

	// Attr is the attribute that marks denial log entries.
	Attr = "toki.deny"

	// Message is the log message of every denial entry.
	Message = "denylog: request denied"

	// MaxPerMinute is the sampling limit per (status, route) key.
	MaxPerMinute = 60

	window  = time.Minute
	maxKeys = 5000
)

// Enabled reports whether the module is on (env TOKI_DENYLOG=off disables).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_DENYLOG"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// Register binds the middleware on OnServe.
func Register(app core.App) {
	if !Enabled() {
		return
	}
	s := newSampler(time.Now)
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(e *core.ServeEvent) error {
			e.Router.Bind(&hook.Handler[*core.RequestEvent]{
				Id: middlewareId,
				// outer to the upstream activity logger so rate limit and auth errors are seen
				Priority: apis.DefaultActivityLoggerMiddlewarePriority - 1,
				Func: func(re *core.RequestEvent) error {
					err := re.Next()
					record(re, err, s)
					return err
				},
			})
			return e.Next()
		},
	})
}

var rulePath = regexp.MustCompile(`^/api/collections/[^/]+/records(/[^/]+)?$`)

// ruleKind derives the collection rule that produced a 403 from path+method.
func ruleKind(method, path string) string {
	m := rulePath.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	hasID := m[1] != ""
	switch method {
	case http.MethodGet:
		if hasID {
			return "view"
		}
		return "list"
	case http.MethodPost:
		if !hasID {
			return "create"
		}
	case http.MethodPatch:
		if hasID {
			return "update"
		}
	case http.MethodDelete:
		if hasID {
			return "delete"
		}
	}
	return ""
}

func record(e *core.RequestEvent, err error, s *sampler) {
	status := e.Status()
	var apiErr *router.ApiError
	hasAPIErr := err != nil && errors.As(err, &apiErr)
	if status == 0 && hasAPIErr {
		status = apiErr.Status
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden && status != http.StatusTooManyRequests {
		return
	}

	route := e.Request.Pattern
	if route == "" {
		route = e.Request.URL.Path
	}
	switch s.allow(status, route) {
	case sampleDrop:
		return
	case sampleCap:
		e.App.Logger().Warn("denylog: sampling, further denials for this status and route are summarized",
			Attr, true, "status", status, "route", route, "limit_per_minute", MaxPerMinute)
		return
	}
	for _, sum := range s.takeSummaries() {
		e.App.Logger().Warn("denylog: suppressed denials in the previous window",
			Attr, true, "status", sum.status, "route", sum.route, "suppressed", sum.count)
	}

	method := e.Request.Method
	path := e.Request.URL.Path

	authKind, authID := "guest", ""
	if e.Auth != nil {
		authKind, authID = "user", e.Auth.Id
		if e.Auth.IsSuperuser() {
			authKind = "superuser"
		}
	}

	reason := ""
	if hasAPIErr {
		reason = apiErr.Message
	} else if err != nil {
		reason = err.Error()
	}

	attrs := []any{
		slog.Bool(Attr, true),
		slog.Int("status", status),
		slog.String("method", method),
		slog.String("path", path),
		slog.String("ip", e.RealIP()),
		slog.String("auth_kind", authKind),
		slog.String("auth_id", authID),
		slog.String("reason", reason),
	}
	if c := e.Request.PathValue("collection"); c != "" {
		attrs = append(attrs, slog.String("collection", c))
	}
	if status == http.StatusForbidden {
		if k := ruleKind(method, path); k != "" {
			attrs = append(attrs, slog.String("rule_kind", k))
		}
	}
	if status == http.StatusTooManyRequests {
		attrs = append(attrs, slog.Bool("rate_limited", true))
	}
	e.App.Logger().Warn(Message, attrs...)
}

// ---------------------------------------------------------------------

type sampleResult int

const (
	sampleLog sampleResult = iota
	sampleCap              // this call hit the limit: log the one-time sampling notice
	sampleDrop
)

type sampleKey struct {
	status int
	route  string
}

type bucket struct {
	start      time.Time
	count      int
	suppressed int
}

type summary struct {
	status int
	route  string
	count  int
}

type sampler struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[sampleKey]*bucket
	pending []summary
}

func newSampler(now func() time.Time) *sampler {
	return &sampler{now: now, buckets: map[sampleKey]*bucket{}}
}

// allow reports what to do with one more denial for the key: log it, log the
// sampling notice (the first one over the limit) or drop it. When a window
// ends with suppressed denials, a summary is queued for takeSummaries.
func (s *sampler) allow(status int, route string) sampleResult {
	now := s.now()
	k := sampleKey{status, route}
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.buckets) > maxKeys {
		for key, b := range s.buckets {
			if now.Sub(b.start) >= window {
				if b.suppressed > 0 {
					s.pending = append(s.pending, summary{key.status, key.route, b.suppressed})
				}
				delete(s.buckets, key)
			}
		}
	}

	b := s.buckets[k]
	if b == nil || now.Sub(b.start) >= window {
		if b != nil && b.suppressed > 0 {
			s.pending = append(s.pending, summary{status, route, b.suppressed})
		}
		b = &bucket{start: now}
		s.buckets[k] = b
	}
	b.count++
	switch {
	case b.count <= MaxPerMinute:
		return sampleLog
	case b.count == MaxPerMinute+1:
		b.suppressed++
		return sampleCap
	default:
		b.suppressed++
		return sampleDrop
	}
}

func (s *sampler) takeSummaries() []summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending
	s.pending = nil
	return p
}
