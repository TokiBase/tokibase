//go:build !no_mcp

package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
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
	failMax               = 30 // bad keys per client address (IPv6: /64) and window
	failMaxEntries        = 8192
	sweepEvery            = 30 * time.Second

	// EnvAllowedHosts lists extra Host header values (comma separated, no port)
	// accepted on loopback sockets, besides loopback names and the host of the
	// app URL setting.
	EnvAllowedHosts = "TOKI_MCP_ALLOWED_HOSTS"
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
			if len(app.Settings().TrustedProxy.Headers) == 0 {
				app.Logger().Warn("mcp: no trusted proxy headers configured; behind a reverse proxy all clients share one failed-key throttle bucket (configure Settings > trusted proxy)")
			}
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

	fmu       sync.Mutex
	fails     map[string]*failState
	lastSweep time.Time
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
	}, &sdk.StreamableHTTPOptions{SessionTimeout: sessionTimeout(), DisableLocalhostProtection: true})

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

// failKeyOf is the throttle bucket of an address: IPv6 clients share a bucket
// per /64 so one prefix can not mint unlimited buckets.
func failKeyOf(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		ip = ip.Unmap()
		if ip.Is6() {
			if p, err := ip.Prefix(64); err == nil {
				return "ip:" + p.String()
			}
		}
		return "ip:" + ip.String()
	}
	return "ip:" + host
}

func (h *httpHandler) throttled(bucket string) bool {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	st := h.fails[bucket]
	if st == nil || h.now().Sub(st.start) > failWindow {
		return false
	}
	return st.n >= failMax
}

func (h *httpHandler) fail(bucket string) {
	h.fmu.Lock()
	defer h.fmu.Unlock()
	now := h.now()
	// sweep on a timer (not per request) and hard-cap the map
	if now.Sub(h.lastSweep) > sweepEvery || len(h.fails) >= failMaxEntries {
		h.lastSweep = now
		for k, v := range h.fails {
			if now.Sub(v.start) > failWindow {
				delete(h.fails, k)
			}
		}
		for k := range h.fails { // still full: evict arbitrary entries
			if len(h.fails) < failMaxEntries {
				break
			}
			delete(h.fails, k)
		}
	}
	st := h.fails[bucket]
	if st == nil || now.Sub(st.start) > failWindow {
		st = &failState{start: now}
		h.fails[bucket] = st
	}
	st.n++
}

// reject counts a failed attempt in bucket and answers 429 once it is full,
// otherwise status/msg.
func (h *httpHandler) reject(w http.ResponseWriter, bucket string, status int, msg string) {
	if h.throttled(bucket) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
		return
	}
	h.fail(bucket)
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	http.Error(w, msg, status)
}

// hostAllowed is the DNS-rebinding guard. It only applies to connections that
// arrive on a loopback socket (the default bind, and every reverse proxy in
// front of it): the Host must be loopback, the host of the configured app URL
// or listed in TOKI_MCP_ALLOWED_HOSTS. Other binds are not restricted (Bearer
// auth is mandatory either way).
func (h *httpHandler) hostAllowed(r *http.Request) bool {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return true
	}
	lh, _, err := net.SplitHostPort(la.String())
	if err != nil {
		return true
	}
	if ip, err := netip.ParseAddr(lh); err != nil || !ip.IsLoopback() {
		return true
	}
	host := strings.ToLower(r.Host)
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return true
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return true
	}
	if u, err := url.Parse(h.app.Settings().Meta.AppURL); err == nil && u.Hostname() != "" && strings.EqualFold(u.Hostname(), host) {
		return true
	}
	for _, a := range strings.Split(os.Getenv(EnvAllowedHosts), ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" && a == host {
			return true
		}
	}
	return false
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.hostAllowed(r) {
		http.Error(w, "Forbidden: invalid Host header (set the app URL in the settings or TOKI_MCP_ALLOWED_HOSTS)", http.StatusForbidden)
		return
	}
	key := bearer(r)
	if key == "" { // not an authentication attempt: not counted
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "missing or invalid agent key", http.StatusUnauthorized)
		return
	}
	ipBucket := failKeyOf(clientAddr(r))
	if !strings.HasPrefix(key, keyPfx) {
		h.reject(w, ipBucket, http.StatusUnauthorized, "missing or invalid agent key")
		return
	}
	a, err := Authenticate(h.app, key)
	switch {
	case err == nil:
	case errors.Is(err, errBadKey):
		h.reject(w, ipBucket, http.StatusUnauthorized, "missing or invalid agent key")
		return
	default: // revoked, expired, malformed allowlist: the key is real, bucket by its hash
		h.reject(w, "key:"+HashKey(key), http.StatusForbidden, err.Error())
		return
	}
	h.inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentCtxKey{}, a)))
}
