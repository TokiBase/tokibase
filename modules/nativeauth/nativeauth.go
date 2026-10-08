//go:build !no_nativeauth

// Package nativeauth signs users in with an ID token that a mobile app
// obtained natively (google_sign_in, sign_in_with_apple). The token is verified
// against the provider JWKS and mapped to the same _externalAuths link and
// auth record that the browser OAuth2 code flow would create.
package nativeauth

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	hookId = "__tokiNativeAuth__"

	EnvSwitch          = "TOKI_NATIVEAUTH"
	EnvGoogleAudiences = "TOKI_NATIVEAUTH_GOOGLE_AUDIENCES"
	EnvAppleAudiences  = "TOKI_NATIVEAUTH_APPLE_AUDIENCES"

	ActionLogin  = "auth.native"
	ActionFailed = "auth.native_failed"

	googleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"
	appleJWKSURL  = "https://appleid.apple.com/auth/keys"

	clockSkew   = 60 * time.Second
	replayLimit = 20000
	maxFailures = 20 // failed attempts per minute per IP
)

// Enabled reports whether the module is active (TOKI_NATIVEAUTH=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSwitch))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

var (
	sinkMu    sync.RWMutex
	auditSink func(action, collection, record string, details map[string]any)
	failSink  func(collection string, rec *core.Record)
	lockSink  func(collection string, rec *core.Record) bool
)

// SetAuditSink connects module events to an external audit log (tokibase.go wires it).
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	auditSink = fn
	sinkMu.Unlock()
}

// SetFailureSink counts a rejected token against the record it belongs to (lockout).
func SetFailureSink(fn func(collection string, rec *core.Record)) {
	sinkMu.Lock()
	failSink = fn
	sinkMu.Unlock()
}

// SetLockedSink refuses sign-in for locked records.
func SetLockedSink(fn func(collection string, rec *core.Record) bool) {
	sinkMu.Lock()
	lockSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := auditSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

func failure(collection string, rec *core.Record) {
	sinkMu.RLock()
	fn := failSink
	sinkMu.RUnlock()
	if fn != nil && rec != nil {
		fn(collection, rec)
	}
}

func isLocked(collection string, rec *core.Record) bool {
	sinkMu.RLock()
	fn := lockSink
	sinkMu.RUnlock()
	return fn != nil && rec != nil && fn(collection, rec)
}

// Module is the native auth module.
type Module struct {
	app core.App
	now func() time.Time

	keys    map[string]*jwks // by provider name
	replay  *replayCache
	limiter *limiter
}

// Register binds the endpoint.
func Register(app core.App) *Module {
	m := &Module{
		app:     app,
		now:     time.Now,
		replay:  newReplayCache(replayLimit),
		limiter: newLimiter(),
	}
	m.keys = map[string]*jwks{
		"google": newJWKS(googleJWKSURL, func() time.Time { return m.now() }),
		"apple":  newJWKS(appleJWKSURL, func() time.Time { return m.now() }),
	}
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId,
		Func: func(e *core.ServeEvent) error {
			e.Router.POST("/api/collections/{collection}/auth-with-native", m.login)
			return e.Next()
		},
	})
	return m
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
