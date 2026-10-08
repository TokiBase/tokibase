//go:build !no_kiosk

package kiosk

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cast"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/edgeguard"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

//go:embed kiosk.js
var kioskJS []byte

const pairPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">` +
	`<title>Pairing</title><body style="font:16px system-ui;margin:2rem"><p id="toki-pair">Pairing...</p>` +
	`<script src="/kiosk/kiosk.js"></script>`

func rateTag(tag string) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + tag, Priority: -900,
		Func: func(e *core.RequestEvent) error {
			if err := apis.CheckRateLimitTags(e, tag); err != nil {
				return err
			}
			return e.Next()
		},
	}
}

func kioskError(e *core.RequestEvent, status int, code, msg string) error {
	return e.JSON(status, map[string]any{"status": status, "message": msg, "data": map[string]any{"code": code}})
}

// isLoopback reports whether the TCP peer is a loopback address and the request
// carries no proxy headers (a reverse proxy on the same host would make every
// remote request look local).
func isLoopback(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// deviceFrom authenticates the device cookie (constant-time hash compare) and bind_ip.
func (m *Module) deviceFrom(e *core.RequestEvent) (*Device, int, string, string) {
	c, err := e.Request.Cookie(CookieName)
	if err != nil || c.Value == "" || len(c.Value) > 256 {
		return nil, http.StatusUnauthorized, "not_paired", "this browser is not paired"
	}
	h := HashToken(c.Value)
	rec, err := m.app.FindFirstRecordByData(Collection, "token_hash", h)
	if err != nil || !equalHash(rec.GetString("token_hash"), h) {
		return nil, http.StatusUnauthorized, "not_paired", "this browser is not paired"
	}
	d := deviceOf(rec)
	if !d.bindOK(e.RealIP()) {
		return nil, http.StatusForbidden, "bind_ip", "this device may not be used from this address"
	}
	return d, 0, "", ""
}

// session issues the auth token of the service actor of d.
func (m *Module) session(d *Device) (map[string]any, int, string, string) {
	actor, err := actorOf(m.app, d)
	if err != nil {
		return nil, http.StatusForbidden, "actor_gone", "the service actor of this device no longer exists"
	}
	if d.NodeID != "" {
		if id := kernel.NodeIdentityOf(m.app); id != nil && id.NodeID() != "" && id.NodeID() != d.NodeID {
			return nil, http.StatusForbidden, "node_mismatch", "this device belongs to another node"
		}
	}
	ttl := time.Duration(d.TTLHours) * time.Hour
	gen, err := generation(m.app, d.ID)
	if err != nil {
		return nil, http.StatusInternalServerError, "token", "failed to issue the token"
	}
	tok, err := actor.NewStaticAuthToken(ttl)
	if err != nil {
		return nil, http.StatusInternalServerError, "token", "failed to issue the token"
	}
	// add the device claims and re-sign with the key of the actor: the token then
	// dies with the next RevokeDevice even when no session store exists
	claims, _ := security.ParseUnverifiedJWT(tok)
	claims[claimDev], claims[claimGen] = d.ID, gen
	if tok, err = security.NewJWT(claims, actor.TokenKey()+actor.Collection().AuthToken.Secret, ttl); err != nil {
		return nil, http.StatusInternalServerError, "token", "failed to issue the token"
	}
	m.rememberSID(d.ID, cast.ToString(claims["sid"]), m.now().Add(ttl))
	m.touchSeen(d.ID)
	return map[string]any{
		"token":   tok,
		"expires": m.now().Add(ttl).Format(time.RFC3339),
		"ttl_s":   int(ttl / time.Second),
		"record": map[string]any{
			"id": actor.Id, "collectionId": actor.Collection().Id, "collectionName": actor.Collection().Name,
		},
		"device":       d.Name,
		"pin_required": d.HasPin,
		"lock_after_s": d.LockAfterS,
	}, 0, "", ""
}

type pairRequest struct {
	Code string `json:"code"`
}

func (m *Module) handlePair(e *core.RequestEvent) error {
	if !allowRemote() && !isLoopback(e.Request) {
		return kioskError(e, http.StatusForbidden, "remote_pairing",
			"pairing is only allowed from the device itself (set TOKI_KIOSK_ALLOW_REMOTE=1 to allow remote pairing)")
	}
	if !m.allow("pair", e.RealIP(), pairPerWindow) {
		return kioskError(e, http.StatusTooManyRequests, "rate_limited", "too many pairing attempts, try again later")
	}
	var req pairRequest
	if err := e.BindBody(&req); err != nil || req.Code == "" || len(req.Code) > 256 {
		return kioskError(e, http.StatusBadRequest, "bad_code", "a pairing code is required")
	}
	bad := func() error {
		return kioskError(e, http.StatusForbidden, "bad_code", "the pairing code is invalid, expired or already used")
	}
	h := HashToken(req.Code)
	rec, err := m.app.FindFirstRecordByData(Collection, "pairing_hash", h)
	if err != nil || !equalHash(rec.GetString("pairing_hash"), h) {
		return bad()
	}
	now := m.now()
	if !rec.GetDateTime("pairing_expires").Time().After(now) {
		return bad()
	}
	d := deviceOf(rec)
	if !d.bindOK(e.RealIP()) {
		return kioskError(e, http.StatusForbidden, "bind_ip", "this device may not be paired from this address")
	}
	tok := NewSecret()
	// the claim is one conditional UPDATE: of two concurrent requests with the same code only one wins
	res, err := m.app.DB().NewQuery(`UPDATE {{` + Collection + `}} SET [[token_hash]]={:t}, [[pairing_hash]]='', [[pairing_expires]]='', [[locked]]=0, [[updated]]={:n}
		WHERE [[id]]={:id} AND [[pairing_hash]]={:h} AND [[pairing_expires]]>{:n}`).
		Bind(map[string]any{"t": HashToken(tok), "n": fmtTime(now), "id": d.ID, "h": h}).Execute()
	if err != nil {
		return e.InternalServerError("Failed to pair the device.", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return bad()
	}
	http.SetCookie(e.Response, &http.Cookie{
		Name: CookieName, Value: tok, Path: "/api/kiosk", MaxAge: 10 * 365 * 24 * 3600,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secureRequest(e.Request),
	})
	audit(AuditPair, d.ID, map[string]any{"device": d.Name, "ip": e.RealIP()})
	return e.JSON(http.StatusOK, map[string]any{"paired": true, "device": d.Name})
}

func (m *Module) handleSession(e *core.RequestEvent) error {
	d, st, code, msg := m.deviceFrom(e)
	if d == nil {
		return kioskError(e, st, code, msg)
	}
	if d.Locked {
		return kioskError(e, http.StatusLocked, "locked", "the device is locked")
	}
	if !m.allow("session", d.ID, sessionPerWindow) {
		return kioskError(e, http.StatusTooManyRequests, "rate_limited", "too many session requests for this device")
	}
	out, st, code, msg := m.session(d)
	if out == nil {
		return kioskError(e, st, code, msg)
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, out)
}

// setLocked stores the lock flag.
func (m *Module) setLocked(id string, locked bool) error {
	v := 0
	if locked {
		v = 1
	}
	_, err := m.app.DB().NewQuery(`UPDATE {{` + Collection + `}} SET [[locked]]={:l}, [[updated]]={:n} WHERE [[id]]={:id}`).
		Bind(map[string]any{"l": v, "n": fmtTime(m.now()), "id": id}).Execute()
	return err
}

func (m *Module) handleLock(e *core.RequestEvent) error {
	d, st, code, msg := m.deviceFrom(e)
	if d == nil {
		return kioskError(e, st, code, msg)
	}
	if !d.HasPin {
		return kioskError(e, http.StatusConflict, "no_pin", "the device has no PIN; set one with `toki kiosk set-pin`")
	}
	if err := m.setLocked(d.ID, true); err != nil {
		return e.InternalServerError("Failed to lock the device.", err)
	}
	// the sid of the request token counts only when the token was verified by the
	// auth loader and belongs to the actor of this device (never trust unverified claims)
	if a := e.Auth; a != nil && a.Id == d.AuthRecord && (a.Collection().Name == d.AuthCollection || a.Collection().Id == d.AuthCollection) {
		claims, _ := security.ParseUnverifiedJWT(bearer(e.Request))
		if sid := cast.ToString(claims["sid"]); sid != "" && cast.ToString(claims[claimDev]) == d.ID {
			m.rememberSID(d.ID, sid, m.now().Add(time.Duration(d.TTLHours)*time.Hour))
		}
	}
	revoked, err := RevokeDevice(m.app, d.ID, "kiosk lock")
	if err != nil {
		return e.InternalServerError("Failed to revoke the tokens of the device.", err)
	}
	return e.JSON(http.StatusOK, map[string]any{"locked": true, "revoked": revoked, "revocable": true})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return h
}

// guard drops the auth context of a kiosk token whose device generation is stale
// (the device was locked, revoked or rotated since the token was issued).
func (m *Module) guard(e *core.RequestEvent) error {
	if e.Auth == nil {
		return e.Next()
	}
	claims, _ := security.ParseUnverifiedJWT(bearer(e.Request))
	dev := cast.ToString(claims[claimDev])
	if dev == "" {
		return e.Next()
	}
	if gen, err := generation(m.app, dev); err != nil || gen != cast.ToInt64(claims[claimGen]) {
		e.Auth = nil
	}
	return e.Next()
}

type unlockRequest struct {
	PIN string `json:"pin"`
}

func (m *Module) handleUnlock(e *core.RequestEvent) error {
	d, st, code, msg := m.deviceFrom(e)
	if d == nil {
		return kioskError(e, st, code, msg)
	}
	if !d.HasPin {
		return kioskError(e, http.StatusConflict, "no_pin", "the device has no PIN")
	}
	if !m.allow("unlock", e.RealIP(), unlockPerWin) {
		e.Response.Header().Set("Retry-After", "60")
		return kioskError(e, http.StatusTooManyRequests, "rate_limited", "too many attempts, try again later")
	}
	var req unlockRequest
	if err := e.BindBody(&req); err != nil {
		return kioskError(e, http.StatusBadRequest, "bad_request", "invalid JSON body")
	}
	// check, compare and record under one per-device mutex: parallel attempts
	// cannot all pass the brake before the first failure is counted
	mu := m.deviceMu(d.ID)
	mu.Lock()
	defer mu.Unlock()
	if ok, wait := m.pinAllowed(d.ID); !ok {
		e.Response.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		return kioskError(e, http.StatusTooManyRequests, "pin_locked", "too many wrong PINs, try again later")
	}
	if !d.checkPin(req.PIN) {
		n, lock := m.pinFailed(d.ID)
		audit(AuditUnlockFail, d.ID, map[string]any{"device": d.Name, "ip": e.RealIP(), "failures": n, "lockout_s": int(lock / time.Second)})
		if lock > 0 {
			e.Response.Header().Set("Retry-After", strconv.Itoa(int(lock/time.Second)))
		}
		return kioskError(e, http.StatusUnauthorized, "bad_pin", "wrong PIN")
	}
	m.pinOK(d.ID)
	// issue the session first: when it fails the device stays locked
	out, st, code, msg := m.session(d)
	if out == nil {
		return kioskError(e, st, code, msg)
	}
	if err := m.setLocked(d.ID, false); err != nil {
		return e.InternalServerError("Failed to unlock the device.", err)
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, out)
}

type printerState struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// printers reads the health block of the printer module (when present).
func (m *Module) printers() []printerState {
	out := []printerState{}
	v, ok := apis.HealthExtra(m.app, "printer")
	if !ok {
		return out
	}
	b, err := json.Marshal(v)
	if err != nil {
		return out
	}
	var h struct {
		Printers []struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
			Status  struct {
				State string `json:"state"`
			} `json:"status"`
		} `json:"printers"`
	}
	if json.Unmarshal(b, &h) != nil {
		return out
	}
	for _, p := range h.Printers {
		if p.Enabled {
			out = append(out, printerState{Name: p.Name, State: p.Status.State})
		}
	}
	return out
}

// Status is the body of GET /api/kiosk/status.
func (m *Module) Status() map[string]any {
	out := map[string]any{"edge": true, "printers": m.printers(), "time": m.now().Format(time.RFC3339)}
	if st, ok := kernel.SyncStatusOf(m.app); ok {
		out["hub"] = st.HubReachable || st.State == kernel.SyncStateHub
		out["pending_changes"] = st.Pending
		if st.LastSync.IsZero() {
			out["last_sync"] = nil
		} else {
			out["last_sync"] = st.LastSync.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func (m *Module) handleStatus(e *core.RequestEvent) error {
	if e.Auth == nil && edgeguard.Device(e) == "" {
		d, st, code, msg := m.deviceFrom(e)
		if d == nil {
			return kioskError(e, st, code, msg)
		}
		m.touchSeen(d.ID)
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	return e.JSON(http.StatusOK, m.Status())
}

func (m *Module) bindRoutes() {
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "routes",
		Func: func(se *core.ServeEvent) error {
			g := se.Router
			g.Bind(&hook.Handler[*core.RequestEvent]{
				Id: hookId + "gen", Priority: apis.DefaultLoadAuthTokenMiddlewarePriority + 2, Func: m.guard,
			})
			quiet := apis.SkipSuccessActivityLog()
			g.POST("/api/kiosk/pair", m.handlePair).Bind(apis.BodyLimit(4<<10), rateTag("kiosk"))
			g.POST("/api/kiosk/session", m.handleSession).Bind(quiet, apis.BodyLimit(4<<10), rateTag("kiosk"))
			g.GET("/api/kiosk/status", m.handleStatus).Bind(quiet)
			g.POST("/api/kiosk/lock", m.handleLock).Bind(apis.BodyLimit(4<<10), rateTag("kiosk"))
			g.POST("/api/kiosk/unlock", m.handleUnlock).Bind(apis.BodyLimit(4<<10), rateTag("kiosk"))
			g.GET("/kiosk/kiosk.js", func(e *core.RequestEvent) error {
				h := e.Response.Header()
				h.Set("Content-Type", "application/javascript; charset=utf-8")
				h.Set("Cache-Control", "public, max-age=300")
				h.Set("X-Content-Type-Options", "nosniff")
				_, err := e.Response.Write(kioskJS)
				return err
			}).Bind(quiet)
			g.GET("/kiosk/pair", func(e *core.RequestEvent) error {
				h := e.Response.Header()
				h.Set("Content-Type", "text/html; charset=utf-8")
				h.Set("Cache-Control", "no-store")
				h.Set("Referrer-Policy", "no-referrer")
				_, err := e.Response.Write([]byte(pairPage))
				return err
			}).Bind(quiet)
			return se.Next()
		},
	})
}
