//go:build !no_sessions

// Package sessions records every issued auth token as a server-side session
// so tokens can be revoked per device or en masse, without changing the
// PocketBase token format (the JWT only gains a "sid" claim) or any REST flow.
package sessions

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
	"github.com/tokibase/tokibase/tools/subscriptions"
)

const (
	hookId       = "__tokiSessions__"
	hookPriority = -1 << 20
	TableName    = "_sessions"
	timeLayout   = "2006-01-02 15:04:05.000Z"

	// ClaimSID is the JWT claim carrying the session token id.
	ClaimSID = "sid"
	// DeviceHeader optionally names the device of a login/refresh request.
	DeviceHeader = "X-Toki-Device"

	KindAuth    = "auth"
	KindRefresh = "refresh"
	KindStatic  = "static"

	ReasonRotated = "rotated"

	ActionRevoke     = "auth.session_revoke"
	ActionRevokeAll  = "auth.session_revoke_all"
	ActionReuse      = "auth.reuse_detected"
	lastSeenThrottle = time.Minute
	ctxRefreshOldSid = "__tokiSessionsRefreshFrom__"

	// realtimeSidKey is the realtime client store key holding the sid of the
	// token the client authenticated with.
	realtimeSidKey = "__tokiSessionsSid__"
	// sweepInterval bounds how long a realtime stream may outlive a revoke
	// done by ANOTHER process (e.g. the CLI); in-process revokes are immediate.
	sweepInterval  = 10 * time.Second
	seenSweepEvery = 1024
)

const createTableSQL = `CREATE TABLE IF NOT EXISTS {{_sessions}} (
	[[id]]             TEXT PRIMARY KEY NOT NULL,
	[[collection]]     TEXT NOT NULL DEFAULT '',
	[[record]]         TEXT NOT NULL DEFAULT '',
	[[token_id]]       TEXT NOT NULL DEFAULT '',
	[[kind]]           TEXT NOT NULL DEFAULT 'auth',
	[[created]]        TEXT NOT NULL DEFAULT '',
	[[last_seen]]      TEXT NOT NULL DEFAULT '',
	[[expires]]        TEXT NOT NULL DEFAULT '',
	[[ip]]             TEXT NOT NULL DEFAULT '',
	[[user_agent]]     TEXT NOT NULL DEFAULT '',
	[[device]]         TEXT NOT NULL DEFAULT '',
	[[revoked]]        TEXT,
	[[revoked_reason]] TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS {{idx__sessions_token_id}} ON {{_sessions}} ([[token_id]]);
CREATE INDEX IF NOT EXISTS {{idx__sessions_record}} ON {{_sessions}} ([[collection]], [[record]]);`

// Enabled reports whether the module is enabled (env TOKI_SESSIONS=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_SESSIONS"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// RotateEnabled reports whether refresh rotation is on (env TOKI_SESSIONS_ROTATE=on, default off).
func RotateEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_SESSIONS_ROTATE"))) {
	case "on", "true", "1", "enabled":
		return true
	}
	return false
}

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects revoke/reuse events to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// Session is a persisted session row.
type Session struct {
	Id            string `db:"id" json:"id"`
	Collection    string `db:"collection" json:"collection"`
	Record        string `db:"record" json:"record"`
	TokenId       string `db:"token_id" json:"tokenId"`
	Kind          string `db:"kind" json:"kind"`
	Created       string `db:"created" json:"created"`
	LastSeen      string `db:"last_seen" json:"lastSeen"`
	Expires       string `db:"expires" json:"expires"`
	IP            string `db:"ip" json:"ip"`
	UserAgent     string `db:"user_agent" json:"userAgent"`
	Device        string `db:"device" json:"device"`
	Revoked       string `db:"revoked" json:"revoked"`
	RevokedReason string `db:"revoked_reason" json:"revokedReason"`
}

const selectCols = "[[id]], [[collection]], [[record]], [[token_id]], [[kind]], [[created]], [[last_seen]], [[expires]], [[ip]], [[user_agent]], [[device]], COALESCE([[revoked]], '') AS [[revoked]], [[revoked_reason]]"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

// ParseTime parses the stored datetime layout (zero time on failure).
func ParseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Module holds the hooks state.
type Module struct {
	app core.App
	now func() time.Time

	seen      sync.Map // token_id -> time of last last_seen write
	seenCount atomic.Int64

	sweepMu sync.Mutex
	stop    chan struct{}
}

// Register binds the module to app: table, token issue seam, validation
// middleware, refresh/auth hooks and revoke-on-credential-change.
func Register(app core.App) *Module {
	m := &Module{app: app, now: time.Now}

	init := func() {
		if _, err := app.DB().NewQuery(createTableSQL).Execute(); err != nil {
			app.Logger().Error("sessions: failed to initialize the _sessions table", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})

	// issue: add the sid claim and record the session
	kernel.OnAuthTokenIssue = m.onIssue

	// validate
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.ServeEvent) error {
			e.Router.Bind(&hook.Handler[*core.RequestEvent]{
				Id:       hookId,
				Priority: apis.DefaultLoadAuthTokenMiddlewarePriority + 1,
				Func:     m.validate,
			})
			m.startSweeper()
			return e.Next()
		},
	})

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		m.stopSweeper()
		return e.Next()
	})

	// realtime: bind the stream to the session of the token that opened it
	app.OnRealtimeSubscribeRequest().Bind(&hook.Handler[*core.RealtimeSubscribeRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RealtimeSubscribeRequestEvent) error {
			sid := ""
			if e.Auth != nil {
				claims, _ := security.ParseUnverifiedJWT(requestToken(e.RequestEvent))
				sid = cast.ToString(claims[ClaimSID])
				if sid != "" && !m.sessionValid(sid) {
					return e.UnauthorizedError("The session is revoked or expired.", nil)
				}
			}
			if err := e.Next(); err != nil {
				return err
			}
			if sid != "" {
				e.Client.Set(realtimeSidKey, sid)
			}
			return nil
		},
	})

	// request metadata for the session created while answering this request
	app.OnRecordAuthRequest().Bind(&hook.Handler[*core.RecordAuthRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordAuthRequestEvent) error {
			m.stamp(e)
			return e.Next()
		},
	})

	// refresh / rotation
	app.OnRecordAuthRefreshRequest().Bind(&hook.Handler[*core.RecordAuthRefreshRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordAuthRefreshRequestEvent) error {
			oldSid := ""
			if claims, _ := security.ParseUnverifiedJWT(requestToken(e.RequestEvent)); claims != nil {
				oldSid = cast.ToString(claims[ClaimSID])
				if !cast.ToBool(claims[kernel.TokenClaimRefreshable]) {
					oldSid = ""
				}
			}
			if oldSid != "" {
				e.RequestEvent.Set(ctxRefreshOldSid, oldSid)
			}
			if err := e.Next(); err != nil {
				return err
			}
			if oldSid != "" && RotateEnabled() {
				m.revokeToken(oldSid, ReasonRotated)
			}
			return nil
		},
	})

	// revoke everything on password or email change
	app.OnRecordUpdateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordEvent) error {
			if !e.Record.Collection().IsAuth() {
				return e.Next()
			}
			reason := credentialChange(e.Record)
			if err := e.Next(); err != nil {
				return err
			}
			if reason != "" {
				// e.App (not m.app): it is the transaction aware app, so the
				// revoke commits or rolls back together with the credential change
				if _, err := RevokeUser(e.App, e.Record.Collection().Id, e.Record.Id, reason); err != nil {
					e.App.Logger().Error("sessions: revoke on credential change failed", "error", err)
					return err
				}
			}
			return nil
		},
	})
	return m
}

func credentialChange(r *core.Record) string {
	orig := r.Original()
	if orig == nil {
		return ""
	}
	if r.Email() != orig.Email() {
		return "email_changed"
	}
	if r.GetString(kernel.FieldNamePassword+":hash") != orig.GetString(kernel.FieldNamePassword+":hash") {
		return "password_changed"
	}
	return ""
}

func requestToken(e *core.RequestEvent) string {
	token := e.Request.Header.Get("Authorization")
	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		return token[7:]
	}
	return token
}

// onIssue is the kernel.OnAuthTokenIssue seam.
func (m *Module) onIssue(rec *kernel.Record, kind string, claims jwt.MapClaims, duration time.Duration) error {
	sid := security.RandomString(16)
	now := m.now()
	_, err := m.app.DB().NewQuery(`INSERT INTO {{_sessions}} ([[id]],[[collection]],[[record]],[[token_id]],[[kind]],[[created]],[[last_seen]],[[expires]])
		VALUES ({:id},{:c},{:r},{:t},{:k},{:now},{:now},{:exp})`).
		Bind(dbx.Params{
			"id": security.RandomString(15), "c": rec.Collection().Id, "r": rec.Id, "t": sid, "k": kind,
			"now": fmtTime(now), "exp": fmtTime(now.Add(duration)),
		}).Execute()
	if err != nil {
		// fail closed: a token without a session row would be unrevocable
		m.app.Logger().Error("sessions: failed to record session, token not issued", "error", err)
		return err
	}
	claims[ClaimSID] = sid
	return nil
}

// stamp fills ip, user agent, device and the refresh kind of the session whose token is answered by e.
func (m *Module) stamp(e *core.RecordAuthRequestEvent) {
	claims, _ := security.ParseUnverifiedJWT(e.Token)
	sid := cast.ToString(claims[ClaimSID])
	if sid == "" {
		return
	}
	ua := e.Request.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	device := strings.TrimSpace(e.Request.Header.Get(DeviceHeader))
	if len(device) > 128 {
		device = device[:128]
	}
	kind := ""
	if old := cast.ToString(e.Get(ctxRefreshOldSid)); old != "" {
		kind = KindRefresh
		if device == "" {
			if s, ok := m.byToken(old); ok {
				device = s.Device
			}
		}
	}
	_, err := m.app.DB().NewQuery(`UPDATE {{_sessions}} SET [[ip]]={:ip}, [[user_agent]]={:ua}, [[device]]={:d},
		[[kind]]=CASE WHEN {:k}='' THEN [[kind]] ELSE {:k} END WHERE [[token_id]]={:t}`).
		Bind(dbx.Params{"ip": e.RealIP(), "ua": ua, "d": device, "k": kind, "t": sid}).Execute()
	if err != nil {
		m.app.Logger().Error("sessions: failed to stamp session", "error", err)
	}
}

// lookup returns the session row of a token id; sql.ErrNoRows when absent.
func (m *Module) lookup(sid string) (*Session, error) {
	var s Session
	err := m.app.DB().NewQuery("SELECT " + selectCols + " FROM {{_sessions}} WHERE [[token_id]]={:t}").
		Bind(dbx.Params{"t": sid}).One(&s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (m *Module) byToken(sid string) (*Session, bool) {
	s, err := m.lookup(sid)
	return s, err == nil
}

// sessionValid reports whether the session is active. Any lookup error or a
// missing row counts as invalid (fail closed).
func (m *Module) sessionValid(sid string) bool {
	s, err := m.lookup(sid)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			m.app.Logger().Error("sessions: lookup failed, treating session as invalid", "error", err)
		}
		return false
	}
	return s.Revoked == "" && !m.expired(s)
}

func (m *Module) expired(s *Session) bool {
	t := ParseTime(s.Expires)
	return !t.IsZero() && !t.After(m.now())
}

// validate drops the auth context when the token's session is revoked or expired.
func (m *Module) validate(e *core.RequestEvent) error {
	if e.Auth == nil {
		return e.Next()
	}
	claims, _ := security.ParseUnverifiedJWT(requestToken(e))
	sid := cast.ToString(claims[ClaimSID])
	if sid == "" {
		// issued before the module existed: valid until natural expiry
		return e.Next()
	}
	s, err := m.lookup(sid)
	if err != nil {
		// fail closed: a token carrying a sid was issued by this module, so a
		// missing row (purged, lost) or a DB error must not authenticate it
		if !errors.Is(err, sql.ErrNoRows) {
			m.app.Logger().Error("sessions: lookup failed, rejecting token", "error", err)
		}
		e.Auth = nil
		return e.Next()
	}
	now := m.now()
	if s.Revoked != "" || m.expired(s) {
		if s.Revoked != "" && s.RevokedReason == ReasonRotated && RotateEnabled() {
			m.reuse(s)
		}
		e.Auth = nil
		return e.Next()
	}
	if last, ok := m.seen.Load(sid); !ok || now.Sub(last.(time.Time)) >= lastSeenThrottle {
		if ok || now.Sub(ParseTime(s.LastSeen)) >= lastSeenThrottle {
			if _, err := m.app.DB().NewQuery("UPDATE {{_sessions}} SET [[last_seen]]={:n} WHERE [[token_id]]={:t}").
				Bind(dbx.Params{"n": fmtTime(now), "t": sid}).Execute(); err != nil {
				m.app.Logger().Error("sessions: failed to update last_seen", "error", err)
			}
		}
		m.seen.Store(sid, now)
		if m.seenCount.Add(1)%seenSweepEvery == 0 {
			m.pruneSeen(now)
		}
	}
	return e.Next()
}

// reuse revokes the family (same user and device) of a rotated-out session presented again.
func (m *Module) reuse(s *Session) {
	var sids []string
	_ = m.app.DB().NewQuery(`SELECT [[token_id]] FROM {{_sessions}} WHERE [[collection]]={:c} AND [[record]]={:r} AND [[device]]={:d} AND [[revoked]] IS NULL`).
		Bind(dbx.Params{"c": s.Collection, "r": s.Record, "d": s.Device}).Column(&sids)
	res, err := m.app.DB().NewQuery(`UPDATE {{_sessions}} SET [[revoked]]={:n}, [[revoked_reason]]='reuse_detected'
		WHERE [[collection]]={:c} AND [[record]]={:r} AND [[device]]={:d} AND [[revoked]] IS NULL`).
		Bind(dbx.Params{"n": fmtTime(m.now()), "c": s.Collection, "r": s.Record, "d": s.Device}).Execute()
	if err != nil {
		m.app.Logger().Error("sessions: reuse revoke failed", "error", err)
		return
	}
	n, _ := res.RowsAffected()
	dropRealtime(m.app, func(c subscriptions.Client) bool { return sidIn(sids, c) })
	if n == 0 {
		return // family already revoked: do not repeat the audit entry
	}
	m.app.Logger().Warn("sessions: refresh token reuse detected, session family revoked",
		"collection", s.Collection, "record", s.Record, "device", s.Device, "revoked", n)
	audit(ActionReuse, s.Collection, s.Record, map[string]any{"device": s.Device, "revoked": n, "session": s.Id})
}

func (m *Module) revokeToken(sid, reason string) {
	_, err := m.app.DB().NewQuery(`UPDATE {{_sessions}} SET [[revoked]]={:n}, [[revoked_reason]]={:r} WHERE [[token_id]]={:t} AND [[revoked]] IS NULL`).
		Bind(dbx.Params{"n": fmtTime(m.now()), "r": reason, "t": sid}).Execute()
	if err != nil {
		m.app.Logger().Error("sessions: revoke failed", "error", err)
		return
	}
	dropRealtime(m.app, func(c subscriptions.Client) bool { return sidIn([]string{sid}, c) })
}

func (m *Module) pruneSeen(now time.Time) {
	m.seen.Range(func(k, v any) bool {
		if now.Sub(v.(time.Time)) >= lastSeenThrottle {
			m.seen.Delete(k)
		}
		return true
	})
}

func sidIn(sids []string, c subscriptions.Client) bool {
	sid, _ := c.Get(realtimeSidKey).(string)
	if sid == "" {
		return false
	}
	for _, x := range sids {
		if x == sid {
			return true
		}
	}
	return false
}

// dropRealtime discards the realtime clients matching fn, which closes their
// SSE streams (the next subscribe must authenticate again).
func dropRealtime(app kernel.App, fn func(subscriptions.Client) bool) {
	for _, c := range app.SubscriptionsBroker().Clients() {
		if fn(c) {
			c.Discard()
		}
	}
}

// sweepRealtime closes the realtime clients whose session is no longer valid.
// It covers revocations done by another process (CLI), which cannot reach the
// in-memory broker directly.
func (m *Module) sweepRealtime() {
	for _, c := range m.app.SubscriptionsBroker().Clients() {
		if sid, _ := c.Get(realtimeSidKey).(string); sid != "" && !m.sessionValid(sid) {
			c.Discard()
		}
	}
}

func (m *Module) startSweeper() {
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	if m.stop != nil {
		return
	}
	m.stop = make(chan struct{})
	go func(stop chan struct{}) {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				m.sweepRealtime()
			}
		}
	}(m.stop)
}

func (m *Module) stopSweeper() {
	m.sweepMu.Lock()
	defer m.sweepMu.Unlock()
	if m.stop != nil {
		close(m.stop)
		m.stop = nil
	}
}
