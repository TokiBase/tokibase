//go:build !no_devicecert

package devicecert

import (
	"crypto/x509"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/edgeguard"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// forbiddenScopes are route prefixes a client certificate can never be scoped
// to (nor to a parent of them, such as "/api"): data, admin and sync routes.
var forbiddenScopes = []string{
	"/api/collections", "/api/admins", "/api/sync", "/api/batch", "/api/settings", "/api/backups",
	"/api/files", "/api/realtime", "/api/logs", "/api/crons", "/api/mcp", "/api/device", "/api/health", "/_",
}

// NormalizeScope validates a route_scope (comma separated path prefixes such
// as "/api/scan,/api/print") and returns it sorted and de-duplicated. An empty
// scope is valid and grants nothing.
func NormalizeScope(s string) (string, error) {
	seen := map[string]bool{}
	var out []string
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, "/") || strings.ContainsAny(e, " *?#%\\") || path.Clean(e) != strings.TrimRight(e, "/") && e != "/" {
			return "", fmt.Errorf("devicecert: invalid route scope %q (use path prefixes such as /api/scan)", e)
		}
		e = strings.TrimRight(e, "/")
		if strings.Count(e, "/") < 2 {
			return "", fmt.Errorf("devicecert: route scope %q is too broad (at least two path segments)", e)
		}
		for _, f := range forbiddenScopes {
			if e == f || strings.HasPrefix(e, f+"/") || strings.HasPrefix(f, e+"/") {
				return "", fmt.Errorf("devicecert: route scope %q covers %s, which a client certificate can not reach", e, f)
			}
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	r := strings.Join(out, ",")
	if len(r) > 256 {
		return "", errors.New("devicecert: route scope is longer than 256 characters")
	}
	return r, nil
}

// MatchScope reports whether the request path is covered by the scope. Paths
// that are not in canonical form never match.
func MatchScope(scope, p string) bool {
	if scope == "" || p == "" || path.Clean(p) != p {
		return false
	}
	for _, e := range strings.Split(scope, ",") {
		if e != "" && (p == e || strings.HasPrefix(p, e+"/")) {
			return true
		}
	}
	return false
}

// bindHTTP mounts the device middleware and /api/device/*.
func (m *Module) bindHTTP() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "http",
		Func: func(se *core.ServeEvent) error {
			se.Router.Bind(&hook.Handler[*core.RequestEvent]{
				Id: hookId + "scope", Priority: apis.DefaultLoadAuthTokenMiddlewarePriority + 3,
				Func: m.scopeMiddleware,
			})
			se.Router.GET("/api/device/identity", m.handleIdentity).Bind(apis.SkipSuccessActivityLog())
			se.Router.POST("/api/device/attest", m.handleAttest).Bind(apis.BodyLimit(2 << 10))
			return se.Next()
		},
	})
}

// scopeMiddleware strips X-Toki-Device from every inbound request and, when
// the request carries a verified, unrevoked client certificate of kind client
// and no auth token, marks it as made by that device if the path is inside the
// certificate's route_scope. The device is not a user and not the service
// actor: only routes that ask for it (edgeguard.Device) serve it, and rules can
// test @request.headers.x_toki_device.
func (m *Module) scopeMiddleware(e *core.RequestEvent) error {
	e.Request.Header.Del(HeaderDevice)
	if e.Auth != nil || e.Request.TLS == nil || len(e.Request.TLS.VerifiedChains) == 0 || len(e.Request.TLS.VerifiedChains[0]) == 0 {
		return e.Next()
	}
	leaf := e.Request.TLS.VerifiedChains[0][0]
	if hasUsage(leaf, x509.ExtKeyUsageServerAuth) || !hasUsage(leaf, x509.ExtKeyUsageClientAuth) { // serverAuth present or clientAuth missing
		return e.Next()
	}
	serial := SerialHex(leaf.SerialNumber)
	if m.deny.has(serial) {
		return e.Next()
	}
	ci := m.info(serial)
	now := m.now()
	if !ci.found || ci.kind != kernel.DeviceCertClient || ci.revoked || !ci.notAfter.IsZero() && now.After(ci.notAfter) {
		return e.Next()
	}
	if MatchScope(ci.scope, e.Request.URL.Path) {
		edgeguard.SetDevice(e, ci.name)
		e.Request.Header.Set(HeaderDevice, ci.name)
	}
	return e.Next()
}
