//go:build !no_mcp

package mcp

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// HTTPPath is where the streamable HTTP transport is mounted.
const HTTPPath = "/api/mcp"

const (
	// EnvSessionTimeout overrides the idle timeout of HTTP sessions (Go duration).
	EnvSessionTimeout = "TOKI_MCP_SESSION_TIMEOUT"

	defaultSessionTimeout = 30 * time.Minute
	httpHookID            = "__tokiMCPHTTP__"
	failWindow            = time.Minute
	failMax               = 30 // bad keys per client address and window
)

type (
	agentCtxKey struct{}
	addrCtxKey  struct{}
)

func sessionTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(EnvSessionTimeout))); err == nil && d > 0 {
		return d
	}
	return defaultSessionTimeout
}

// RegisterHTTP mounts the MCP streamable HTTP transport at /api/mcp when
// TOKI_MCP=on. Agents authenticate with `Authorization: Bearer tka_...`.
func RegisterHTTP(app core.App) {
	if !HTTPEnabled() {
		return
	}
	h := NewHTTPHandler(app)
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: httpHookID,
		Func: func(e *core.ServeEvent) error {
			e.Router.Any(HTTPPath, func(re *core.RequestEvent) error {
				ctx := context.WithValue(re.Request.Context(), addrCtxKey{}, re.RealIP())
				h.ServeHTTP(re.Response, re.Request.WithContext(ctx))
				return nil
			})
			return e.Next()
		},
	})
}

type failState struct {
	start time.Time
	n     int
}

// httpHandler authenticates agent keys and serves one MCP server per agent.
type httpHandler struct {
	app   core.App
	inner http.Handler
	now   func() time.Time

	mu      sync.Mutex
	servers map[string]*Server

	fmu   sync.Mutex
	fails map[string]*failState
}

// NewHTTPHandler returns the authenticated streamable HTTP handler (exported
// for tests and custom mounting).
func NewHTTPHandler(app core.App) http.Handler {
	h := &httpHandler{app: app, now: time.Now, servers: map[string]*Server{}, fails: map[string]*failState{}}
	stream := sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		a, _ := r.Context().Value(agentCtxKey{}).(*Agent)
		if a == nil {
			return nil
		}
		return h.server(a).SDK()
	}, &sdk.StreamableHTTPOptions{SessionTimeout: sessionTimeout()})

	// binds every session to the agent id: another key can not reuse it
	verifier := func(ctx context.Context, _ string, r *http.Request) (*auth.TokenInfo, error) {
		a, _ := r.Context().Value(agentCtxKey{}).(*Agent)
		if a == nil {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: a.ID, Expiration: h.now().Add(time.Minute)}, nil
	}
	h.inner = auth.RequireBearerToken(verifier, nil)(stream)
	return h
}

func (h *httpHandler) server(a *Agent) *Server {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.servers[a.ID]
	if s == nil {
		s = NewServer(h.app, a, "")
		s.transport = "http"
		h.servers[a.ID] = s
	}
	return s
}

func bearer(r *http.Request) string {
	f := strings.Fields(r.Header.Get("Authorization"))
	if len(f) == 2 && strings.EqualFold(f[0], "bearer") {
		return f[1]
	}
	return ""
}

// clientAddr is the address the app router resolved (trusted proxy headers
// included); RemoteAddr when mounted elsewhere.
func clientAddr(r *http.Request) string {
	if v, _ := r.Context().Value(addrCtxKey{}).(string); v != "" {
		return v
	}
	return r.RemoteAddr
}

func (h *httpHandler) throttled(addr string) bool {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	st := h.fails[addr]
	if st == nil || h.now().Sub(st.start) > failWindow {
		return false
	}
	return st.n >= failMax
}

func (h *httpHandler) fail(addr string) {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	now := h.now()
	if len(h.fails) > 4096 {
		for k, v := range h.fails {
			if now.Sub(v.start) > failWindow {
				delete(h.fails, k)
			}
		}
	}
	st := h.fails[addr]
	if st == nil || now.Sub(st.start) > failWindow {
		st = &failState{start: now}
		h.fails[addr] = st
	}
	st.n++
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	if h.throttled(addr) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
		return
	}
	key := bearer(r)
	if key == "" || !strings.HasPrefix(key, keyPfx) {
		h.fail(addr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "missing or invalid agent key", http.StatusUnauthorized)
		return
	}
	a, err := Authenticate(h.app, key)
	switch {
	case err == nil:
	case errors.Is(err, errBadKey):
		h.fail(addr)
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "missing or invalid agent key", http.StatusUnauthorized)
		return
	default: // revoked, expired, malformed allowlist
		h.fail(addr)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	h.inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
}
