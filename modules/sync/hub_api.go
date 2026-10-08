//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

// Hub tunables.
const (
	// SessionTTL is the lifetime of a node session token.
	SessionTTL = 15 * time.Minute
	// SigWindow is the accepted distance between a signed ts and hub time.
	SigWindow = 5 * time.Minute
	// DefaultMaxDrift is the default of TOKI_SYNC_MAX_DRIFT.
	DefaultMaxDrift = 5 * time.Minute
	// DefaultPollMs is the poll interval hint of the handshake.
	DefaultPollMs = 30000

	EnvMaxDrift = "TOKI_SYNC_MAX_DRIFT"

	ctxNodeKey = "tokiSyncNode"
)

func maxDrift() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(EnvMaxDrift))); err == nil && d > 0 {
		return d
	}
	return DefaultMaxDrift
}

// NodeFrom returns the node id that the node-auth middleware stored in the
// request ("" when the route is not node-authenticated).
func NodeFrom(e *core.RequestEvent) string {
	s, _ := e.Get(ctxNodeKey).(string)
	return s
}

// syncErr writes the error body of docs/SYNC_DESIGN.md §3.
func syncErr(e *core.RequestEvent, status int, code, msg string, extra map[string]any) error {
	data := map[string]any{"code": code}
	for k, v := range extra {
		data[k] = v
	}
	return e.JSON(status, proto.ErrorBody{Status: status, Message: msg, Data: data})
}

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

// bindRoutes mounts the routes on OnServe, only for the hub role.
func (m *Module) bindRoutes() {
	if m.role != RoleHub {
		return
	}
	m.app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId + "routes",
		Func: func(se *core.ServeEvent) error {
			g := se.Router
			g.POST(proto.PathEnroll, m.enrollHandler).
				Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(16<<10), rateTag("sync:enroll"))
			g.POST(proto.PathHandshake, m.handshakeHandler).
				Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(64<<10), rateTag("sync:handshake"))
			g.GET(proto.PathPing, m.pingHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:ping"), m.nodeAuth())
			return se.Next()
		},
	})
}

func (m *Module) hubReady() bool { return m.ready.Load() && m.hub != nil }

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func decodeKey(s string, n int) ([]byte, bool) {
	b, err := base64.StdEncoding.DecodeString(s)
	return b, err == nil && len(b) == n
}

func (m *Module) hubURL(e *core.RequestEvent) string {
	if u := strings.TrimRight(strings.TrimSpace(e.App.Settings().Meta.AppURL), "/"); u != "" {
		return u
	}
	scheme := "http"
	if e.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + e.Request.Host
}

// enrollHandler is POST /api/sync/enroll (no auth, the code is the secret).
// Bad, expired and used codes all give the same answer.
func (m *Module) enrollHandler(e *core.RequestEvent) error {
	if !m.hubReady() {
		return syncErr(e, http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready", nil)
	}
	var req proto.EnrollRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	edPub, ok1 := decodeKey(req.Ed25519Pub, ed25519.PublicKeySize)
	kxPub, ok2 := decodeKey(req.X25519Pub, 32)
	if !ok1 || !ok2 || req.Code == "" {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	invalid := func() error {
		return syncErr(e, http.StatusBadRequest, proto.CodeEnrollInvalid, "The enrollment code is invalid, expired or already used.", nil)
	}

	hash := HashEnrollCode(req.Code)
	pending, err := e.App.FindRecordsByFilter(NodesCollection, "status={:s} && enroll_hash!=''", "", 0, 0, dbx.Params{"s": NodePending})
	if err != nil {
		return err
	}
	// compare against every pending row, no early exit
	var match *core.Record
	for _, r := range pending {
		if constEq(hash, r.GetString("enroll_hash")) {
			match = r
		}
	}
	// same work for "no match" so the timing does not depend on the outcome
	_ = constEq(hash, strings.Repeat("0", len(hash)))
	if match == nil || !match.GetDateTime("enroll_expires").Time().After(time.Now()) {
		return invalid()
	}
	nodeID := proto.NodeID(edPub)
	if ex, _ := e.App.FindRecordById(NodesCollection, nodeID); ex != nil {
		return invalid()
	}

	now := time.Now().UTC()
	var cert string
	err = e.App.RunInTransaction(func(tx kernel.App) error {
		// claim the code atomically: only one request can flip pending -> active
		res, err := tx.DB().NewQuery("UPDATE " + NodesCollection + " SET status={:a}, enroll_hash='', enroll_expires='' WHERE id={:id} AND status={:p} AND enroll_hash={:h}").
			Bind(dbx.Params{"a": NodeActive, "p": NodePending, "id": match.Id, "h": hash}).Execute()
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errEnrollUsed
		}
		rec, err := tx.FindRecordById(NodesCollection, match.Id)
		if err != nil {
			return err
		}
		var params map[string]any
		_ = rec.UnmarshalJSONField("params", &params)
		var ser [8]byte
		_, _ = rand.Read(ser[:])
		claims := &proto.CertClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: m.hub.id, Subject: nodeID,
				IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(proto.CertValidity)),
			},
			Pub: b64(edPub), KX: b64(kxPub), Params: params, Ser: hex.EncodeToString(ser[:]),
		}
		cert, err = proto.SignCert(m.hub.priv, claims)
		if err != nil {
			return err
		}
		rec.Set("pubkey", b64(edPub))
		rec.Set("kx_pubkey", b64(kxPub))
		rec.Set("cert_serial", claims.Ser)
		rec.Set("cert_expires", now.Add(proto.CertValidity))
		if req.AppVersion != "" {
			rec.Set("app_version", truncate(req.AppVersion, 64))
		}
		if err := tx.Save(rec); err != nil {
			return err
		}
		// the row id becomes the key-derived node id
		_, err = tx.DB().NewQuery("UPDATE " + NodesCollection + " SET id={:n} WHERE id={:o}").
			Bind(dbx.Params{"n": nodeID, "o": match.Id}).Execute()
		return err
	})
	if err == errEnrollUsed {
		return invalid()
	}
	if err != nil {
		e.App.Logger().Error("sync: enroll failed", "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "enrollment failed", nil)
	}
	emit(AuditNodeEnroll, NodesCollection, nodeID, map[string]any{
		"name": match.GetString("name"), "profile": match.GetString("profile"), "stage": "completed", "ip": e.RealIP(),
	})
	return e.JSON(http.StatusOK, proto.EnrollResponse{
		NodeID: nodeID, HubID: m.hub.id, HubURL: m.hubURL(e), Cert: cert, HubPub: b64(m.hub.pub),
	})
}

type enrollErr string

func (e enrollErr) Error() string { return string(e) }

const errEnrollUsed = enrollErr("enrollment code already used")

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (m *Module) failHandshake(e *core.RequestEvent, node, reason string, status int, code, msg string, extra map[string]any) error {
	emit(AuditHandshakeFailed, NodesCollection, node, map[string]any{"node": node, "reason": reason, "ip": e.RealIP()})
	return syncErr(e, status, code, msg, extra)
}

// handshakeHandler is POST /api/sync/handshake (signed, see proto.SignRequest).
func (m *Module) handshakeHandler(e *core.RequestEvent) error {
	if !m.hubReady() {
		return syncErr(e, http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready", nil)
	}
	nodeID := e.Request.Header.Get(proto.HeaderNode)
	ts := e.Request.Header.Get(proto.HeaderSigTs)
	nonce := e.Request.Header.Get(proto.HeaderNonce)
	sig := e.Request.Header.Get(proto.HeaderSig)
	if nodeID == "" || ts == "" || nonce == "" || sig == "" || len(nonce) > 64 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "missing signature headers", nil)
	}
	body, err := io.ReadAll(e.Request.Body)
	if err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	var req proto.HandshakeRequest
	if err := json.Unmarshal(body, &req); err != nil || req.NodeID != nodeID {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	unauth := func(reason string, extra map[string]any) error {
		return m.failHandshake(e, nodeID, reason, http.StatusUnauthorized, proto.CodeUnauthorized, "The handshake was rejected.", extra)
	}

	node, err := e.App.FindRecordById(NodesCollection, nodeID)
	if err != nil || node.GetString("pubkey") == "" {
		return unauth("unknown_node", nil)
	}
	nodePub, ok := decodeKey(node.GetString("pubkey"), ed25519.PublicKeySize)
	if !ok {
		return unauth("bad_stored_key", nil)
	}
	now := m.now()
	// cert: hub signature, expiry, sub == node, bound to the stored key and serial
	claims, err := proto.VerifyCert(m.hub.pub, req.Cert, now)
	if err != nil || claims.Subject != nodeID || claims.Pub != node.GetString("pubkey") || claims.Ser != node.GetString("cert_serial") {
		return unauth("bad_cert", nil)
	}
	if !proto.VerifyRequest(nodePub, e.Request.Method, e.Request.URL.Path, ts, nonce, body, sig) {
		return unauth("bad_signature", nil)
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return unauth("bad_ts", nil)
	}
	if d := now.Sub(time.UnixMilli(ms)); d > SigWindow || d < -SigWindow {
		// the signature is valid, so the caller may learn the hub time and correct its clock
		return unauth("ts_window", map[string]any{"server_time": now.UTC().Format(proto.TimeLayout)})
	}
	// the signature is genuine from here on: only now the status is revealed
	if node.GetString("status") == NodeRevoked {
		return m.failHandshake(e, nodeID, "revoked", http.StatusForbidden, proto.CodeNodeRevoked, "This node was revoked.", nil)
	}
	if node.GetString("status") == NodePending {
		return unauth("pending", nil)
	}
	if !m.nonces.Use(nodeID, nonce, now) {
		return unauth("nonce_replay", nil)
	}

	// clock offset as the hub sees it: server_time - client_time
	var offset int64
	if ct, err := time.Parse(time.RFC3339Nano, req.ClientTime); err == nil {
		offset = now.Sub(ct).Milliseconds()
	} else {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid client_time", nil)
	}
	drift := maxDrift()
	clockOK := offset <= drift.Milliseconds() && offset >= -drift.Milliseconds()

	node.Set("last_seen", types.NowDateTime())
	node.Set("clock_offset_ms", offset)
	if req.AppVersion != "" {
		node.Set("app_version", truncate(req.AppVersion, 64))
	}
	if validProfile(req.Profile) {
		node.Set("profile", req.Profile)
	}
	node.Set("schema_version", req.SchemaVersion)
	if err := e.App.Save(node); err != nil {
		e.App.Logger().Error("sync: failed to update the node", "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "handshake failed", nil)
	}

	expires := now.Add(SessionTTL)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"typ": proto.SessionTokenType, "sub": nodeID, "iss": m.hub.id,
		"iat": now.Unix(), "exp": expires.Unix(),
	}).SignedString(m.hub.secret)
	if err != nil {
		return err
	}
	var params map[string]any
	_ = node.UnmarshalJSONField("params", &params)
	if params == nil {
		params = map[string]any{}
	}
	return e.JSON(http.StatusOK, proto.HandshakeResponse{
		SessionToken: tok,
		Expires:      expires.UTC().Format(proto.TimeLayout),
		HubID:        m.hub.id,
		HubEpoch:     m.hub.epoch,
		ServerTime:   now.UTC().Format(proto.TimeLayout),
		// TODO(PR8): clock.ok is computed here but enforced (409 sync_clock_drift on push) only in PR8.
		Clock:  proto.Clock{Ok: clockOK, OffsetMs: offset, MaxDriftMs: drift.Milliseconds()},
		Schema: proto.Schema{Version: 0, Bundles: []any{}}, // TODO(PR8): schema versions and bundles
		// TODO(PR6): strategy, partition and crypto come from the full policy model.
		Policies:     m.handshakePolicies(),
		Params:       params,
		Keys:         []any{}, // TODO(PR9): wrapped collection keys
		PushFrom:     int64(node.GetFloat("pushed_origin_seq")) + 1,
		LowWater:     0,       // TODO(PR6): lowest retained hub seq
		Rebootstrap:  false,   // TODO(PR7): snapshot bootstrap decision
		Reservations: []any{}, // TODO(PR8): sequence reservations
		PollMs:       DefaultPollMs,
	})
}

func validProfile(p string) bool {
	for _, v := range nodeProfiles {
		if v == p {
			return true
		}
	}
	return false
}

func (m *Module) handshakePolicies() []proto.Policy {
	out := []proto.Policy{}
	recs, err := m.app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return out
	}
	for _, r := range recs {
		if !r.GetBool("enabled") {
			continue
		}
		p := proto.Policy{
			Collection: r.GetString("collection"), Direction: r.GetString("direction"),
			Strategy: "lww", FieldTypes: map[string]string{}, Exclude: []string{}, Crypto: "ciphertext",
		}
		if p.Direction == "" {
			p.Direction = DirBoth
		}
		if raw := rawJSON(r, "field_types"); raw != nil {
			_ = json.Unmarshal(raw, &p.FieldTypes)
		}
		if raw := rawJSON(r, "exclude"); raw != nil {
			_ = json.Unmarshal(raw, &p.Exclude)
		}
		out = append(out, p)
	}
	return out
}

// nodeAuth is the middleware of node-authenticated routes: it validates the
// `Authorization: Bearer <session_token>` and stores the node id in the request
// (see NodeFrom). 401 sync_unauthorized, 403 sync_node_revoked.
func (m *Module) nodeAuth() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + "nodeauth", Priority: -800,
		Func: func(e *core.RequestEvent) error {
			if !m.hubReady() {
				return syncErr(e, http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready", nil)
			}
			f := strings.Fields(e.Request.Header.Get("Authorization"))
			if len(f) != 2 || !strings.EqualFold(f[0], "bearer") {
				return syncErr(e, http.StatusUnauthorized, proto.CodeUnauthorized, "A node session token is required.", nil)
			}
			claims := jwt.MapClaims{}
			_, err := jwt.ParseWithClaims(f[1], claims, func(*jwt.Token) (any, error) { return m.hub.secret, nil },
				jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(m.now), jwt.WithExpirationRequired())
			sub, _ := claims["sub"].(string)
			if typ, _ := claims["typ"].(string); err != nil || typ != proto.SessionTokenType || sub == "" {
				return syncErr(e, http.StatusUnauthorized, proto.CodeUnauthorized, "The session token is invalid or expired.", nil)
			}
			node, err := e.App.FindRecordById(NodesCollection, sub)
			if err != nil {
				return syncErr(e, http.StatusUnauthorized, proto.CodeUnauthorized, "The session token is invalid or expired.", nil)
			}
			switch node.GetString("status") {
			case NodeRevoked:
				return syncErr(e, http.StatusForbidden, proto.CodeNodeRevoked, "This node was revoked.", nil)
			case NodePending:
				return syncErr(e, http.StatusUnauthorized, proto.CodeUnauthorized, "The session token is invalid or expired.", nil)
			}
			e.Set(ctxNodeKey, sub)
			return e.Next()
		},
	}
}

// pingHandler is GET /api/sync/ping (node-authenticated).
func (m *Module) pingHandler(e *core.RequestEvent) error {
	return e.JSON(http.StatusOK, proto.PingResponse{NodeID: NodeFrom(e), ServerTime: m.now().UTC().Format(proto.TimeLayout)})
}
