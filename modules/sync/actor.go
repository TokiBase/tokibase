//go:build !no_sync

package sync

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/security"
)

// Env of the actor model (docs/SYNC_DESIGN.md §8.3).
const (
	EnvActorTTL             = "TOKI_SYNC_ACTOR_TTL"
	EnvAllowSuperuserActors = "TOKI_SYNC_ALLOW_SUPERUSER_ACTORS"
	EnvAuditAll             = "TOKI_SYNC_AUDIT_ALL"
	EnvRetention            = "TOKI_SYNC_RETENTION"

	// DefaultActorTTL is the lifetime of a grant.
	DefaultActorTTL = 30 * 24 * time.Hour
	// DefaultRetention is the default of TOKI_SYNC_RETENTION; a grant never outlives it.
	DefaultRetention = 90 * 24 * time.Hour
	// ActorSkew is how far before its issue time a change HLC may lie.
	ActorSkew = 5 * time.Minute
)

// parseDuration is time.ParseDuration plus a "d" (days) suffix.
func parseDuration(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		f, err := strconv.ParseFloat(n, 64)
		if err != nil || f <= 0 {
			return 0, false
		}
		return time.Duration(f * float64(24*time.Hour)), true
	}
	d, err := time.ParseDuration(s)
	return d, err == nil && d > 0
}

func envFlag(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func retention() time.Duration {
	if d, ok := parseDuration(os.Getenv(EnvRetention)); ok {
		return d
	}
	return DefaultRetention
}

// actorTTL is TOKI_SYNC_ACTOR_TTL capped at the retention.
func actorTTL() time.Duration {
	ttl := DefaultActorTTL
	if d, ok := parseDuration(os.Getenv(EnvActorTTL)); ok {
		ttl = d
	}
	return min(ttl, retention())
}

func allowSuperuserActors() bool { return envFlag(EnvAllowSuperuserActors) }

func tokenKeyHash(rec *core.Record) string {
	h := sha256.Sum256([]byte(rec.TokenKey()))
	return hex.EncodeToString(h[:])
}

// ---- spoke side: the actor of a local write ----------------------------

// actorIDFor is the `actor` of a change written by a request of auth: the aid
// of the newest valid grant for that record on this node, else "node".
func (m *Module) actorIDFor(app core.App, auth *core.Record) string {
	if auth == nil || m.role != RoleSpoke || m.Clock() == nil {
		return ActorNode
	}
	var aid string
	err := app.DB().NewQuery("SELECT aid FROM _sync_actors WHERE collection={:c} AND record={:r} AND exp>{:now} ORDER BY rowid DESC LIMIT 1").
		Bind(dbx.Params{"c": auth.Collection().Id, "r": auth.Id, "now": m.Clock().WallNow().UnixMilli()}).Row(&aid)
	if err != nil || aid == "" {
		return ActorNode
	}
	return aid
}

// ---- hub side: grants ---------------------------------------------------

type grantRow struct {
	AID        string `db:"aid"`
	Node       string `db:"node"`
	Collection string `db:"collection"`
	Record     string `db:"record"`
	SID        string `db:"sid"`
	TKH        string `db:"tkh"`
	IAT        int64  `db:"iat"`
	EXP        int64  `db:"exp"`
	RevokedAt  string `db:"revoked_at"`
}

func loadGrant(db dbx.Builder, aid string) (*grantRow, error) {
	var g grantRow
	err := db.NewQuery("SELECT aid, node, collection, record, sid, tkh, iat, exp, revoked_at FROM _sync_actor_grants WHERE aid={:a}").
		Bind(dbx.Params{"a": aid}).One(&g)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &g, err
}

// actorCtx is the resolved actor of a push group.
type actorCtx struct {
	rec     *core.Record
	aid     string // "" = the service actor of the node
	service bool
}

func (a *actorCtx) kind() string { return kernel.AuthKindOf(a.rec) }

// resolveActor validates the actor of a group on the hub at APPLY time
// (docs/SYNC_DESIGN.md §1.6). aid is "" for the service actor. hlcs are the
// HLCs of the group's changes made under this actor.
func (m *Module) resolveActor(tx kernel.App, nodeID, aid string, hlcs []hlc.HLC) (*actorCtx, *rejection) {
	db := tx.DB()
	node, err := tx.FindRecordById(NodesCollection, nodeID)
	if err != nil {
		return nil, reject(proto.CodeActorUnknown, "unknown node")
	}
	nodeRevoked := node.GetString("status") == NodeRevoked

	if aid == "" {
		colRef, id := node.GetString("actor_collection"), node.GetString("actor_record")
		if colRef == "" || id == "" {
			return nil, reject(proto.CodeActorUnknown, "the node has no service actor (toki sync enroll --actor)")
		}
		if nodeRevoked {
			return nil, parked(proto.CodeActorRevoked, "the node is revoked")
		}
		rec, err := tx.FindRecordById(colRef, id)
		if err != nil {
			return nil, reject(proto.CodeActorUnknown, "the service actor does not exist")
		}
		return &actorCtx{rec: rec, service: true}, nil
	}

	if len(aid) > 64 {
		return nil, reject(proto.CodeActorUnknown, "unknown actor grant")
	}
	g, err := loadGrant(db, aid)
	if err != nil {
		return nil, &rejection{code: "apply_error", msg: err.Error(), internal: true}
	}
	if g == nil {
		return nil, reject(proto.CodeActorUnknown, "unknown actor grant")
	}
	if g.Node != nodeID {
		return nil, reject(proto.CodeActorNodeMismatch, "the grant belongs to another node")
	}
	for _, h := range hlcs {
		ms := h.PhysicalMs()
		if ms < g.IAT-ActorSkew.Milliseconds() || ms > g.EXP {
			return nil, reject(proto.CodeActorExpired, "the change is outside the validity of the grant")
		}
	}
	if g.RevokedAt != "" || nodeRevoked {
		return nil, parked(proto.CodeActorRevoked, "the grant or the node is revoked")
	}
	if g.SID != "" && kernel.SessionActive != nil {
		ok, err := kernel.SessionActive(tx, g.SID)
		if err != nil {
			return nil, &rejection{code: "apply_error", msg: err.Error(), internal: true}
		}
		if !ok {
			return nil, parked(proto.CodeActorRevoked, "the hub session of the user is no longer active")
		}
	}
	rec, err := tx.FindRecordById(g.Collection, g.Record)
	if err != nil {
		return nil, reject(proto.CodeActorUnknown, "the actor record no longer exists")
	}
	if g.TKH != "" && rec.Collection().IsAuth() && g.TKH != tokenKeyHash(rec) {
		return nil, parked(proto.CodeActorRevoked, "the credentials of the user changed")
	}
	if kernel.AuthKindOf(rec) == kernel.AuthKindSuperuser && !allowSuperuserActors() {
		return nil, reject(proto.CodeActorForbidden, "superusers cannot be sync actors")
	}
	return &actorCtx{rec: rec, aid: aid}, nil
}

// parked is a rejection that parks the change for review instead of reverting.
func parked(code, msg string) *rejection { return &rejection{code: code, msg: msg, park: true} }

// RevokeActorGrant revokes a grant on the hub (logout on a device, admin).
func RevokeActorGrant(app core.App, aid string, by map[string]any) (bool, error) {
	res, err := app.NonconcurrentDB().NewQuery("UPDATE _sync_actor_grants SET revoked_at={:t} WHERE aid={:a} AND revoked_at=''").
		Bind(dbx.Params{"t": time.Now().UTC().Format("2006-01-02 15:04:05.000Z"), "a": aid}).Execute()
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		d := map[string]any{"aid": aid}
		for k, v := range by {
			d[k] = v
		}
		emit(AuditActorRevoke, "", aid, d)
	}
	return n > 0, nil
}

// actorHandler is POST /api/sync/actor (docs/SYNC_DESIGN.md §3.11).
func (m *Module) actorHandler(e *core.RequestEvent) error {
	if !m.hubReady() {
		return syncErr(e, http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready", nil)
	}
	nodeID := NodeFrom(e)
	tok := strings.TrimSpace(e.Request.Header.Get(proto.HeaderActorToken))
	tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer "))
	invalid := func(msg string) error {
		return syncErr(e, http.StatusUnauthorized, proto.CodeActorInvalid, msg, nil)
	}
	if tok == "" || len(tok) > 4096 {
		return invalid("A hub user token is required in " + proto.HeaderActorToken + ".")
	}
	user, err := e.App.FindAuthRecordByToken(tok, core.TokenTypeAuth)
	if err != nil || user == nil {
		return invalid("The user token is invalid or expired.")
	}
	claims, _ := security.ParseUnverifiedJWT(tok)
	sid, _ := claims["sid"].(string)
	if sid != "" && kernel.SessionActive != nil {
		ok, err := kernel.SessionActive(e.App, sid)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("The session of the user token is no longer active.")
		}
	}
	if kernel.AuthKindOf(user) == kernel.AuthKindSuperuser && !allowSuperuserActors() {
		emit(AuditReject, user.Collection().Name, user.Id, map[string]any{
			"code": proto.CodeActorForbidden, "node": nodeID, "ip": e.RealIP(), "stage": "grant", "actor_kind": kernel.AuthKindSuperuser,
			"actor_id": user.Id, "actor_collection": user.Collection().Name,
		})
		return syncErr(e, http.StatusForbidden, proto.CodeActorForbidden, "Superusers cannot be sync actors.", nil)
	}
	now := m.now().UTC().Truncate(time.Second)
	exp := now.Add(actorTTL())
	aid := "g" + strings.ToLower(security.RandomString(15))
	_, err = e.App.NonconcurrentDB().NewQuery(`INSERT INTO _sync_actor_grants (aid, node, collection, record, sid, tkh, iat, exp, created)
  VALUES ({:aid},{:node},{:col},{:rec},{:sid},{:tkh},{:iat},{:exp},{:cr})`).
		Bind(dbx.Params{"aid": aid, "node": nodeID, "col": user.Collection().Id, "rec": user.Id, "sid": sid, "tkh": tokenKeyHash(user),
			"iat": now.UnixMilli(), "exp": exp.UnixMilli(), "cr": m.created()}).Execute()
	if err != nil {
		return err
	}
	assertion, err := proto.SignActor(m.hub.priv, &proto.ActorClaims{
		RegisteredClaims: jwtClaims(m.hub.id, aid, now, exp),
		Node:             nodeID, Col: user.Collection().Id, Rec: user.Id, Sid: sid,
	})
	if err != nil {
		return err
	}
	emit(AuditActorGrant, user.Collection().Name, user.Id, map[string]any{
		"aid": aid, "node": nodeID, "exp": exp.Format(proto.TimeLayout), "ip": e.RealIP(),
		"actor_kind": kernel.AuthKindOf(user), "actor_id": user.Id, "actor_collection": user.Collection().Name,
	})
	return e.JSON(http.StatusOK, proto.ActorResponse{
		AID: aid, Exp: exp.Format(proto.TimeLayout), Assertion: assertion, Record: userExport(user),
	})
}

// userExport is the record of the user for the device: everything except
// password, tokenKey and file names.
func userExport(user *core.Record) map[string]any {
	out := map[string]any{}
	for _, f := range user.Collection().Fields {
		name := f.GetName()
		if name == kernel.FieldNamePassword || name == kernel.FieldNameTokenKey ||
			f.Type() == kernel.FieldTypePassword || f.Type() == kernel.FieldTypeFile {
			continue
		}
		var v any
		if dv, ok := f.(core.DriverValuer); ok {
			if x, err := dv.DriverValue(user); err == nil {
				v = x
			}
		} else {
			v = user.GetRaw(name)
		}
		// round trip through JSON for a plain wire shape
		if b, err := json.Marshal(v); err == nil {
			var plain any
			if json.Unmarshal(b, &plain) == nil {
				v = plain
			}
		}
		out[name] = v
	}
	out["id"] = user.Id
	return out
}

// actorRevokeHandler is DELETE /api/sync/actor/{aid}.
func (m *Module) actorRevokeHandler(e *core.RequestEvent) error {
	aid := e.Request.PathValue("aid")
	g, err := loadGrant(e.App.DB(), aid)
	if err != nil {
		return err
	}
	if g == nil || g.Node != NodeFrom(e) {
		return syncErr(e, http.StatusNotFound, proto.CodeBadRequest, "unknown actor grant", nil)
	}
	if _, err := RevokeActorGrant(e.App, aid, map[string]any{"node": g.Node, "ip": e.RealIP(), "by": "node"}); err != nil {
		return err
	}
	return e.JSON(http.StatusOK, map[string]any{"ok": true})
}

func jwtClaims(iss, sub string, iat, exp time.Time) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{Issuer: iss, Subject: sub, IssuedAt: jwt.NewNumericDate(iat), ExpiresAt: jwt.NewNumericDate(exp)}
}
