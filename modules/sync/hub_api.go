//go:build !no_sync

package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
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
				Bind(apis.SkipSuccessActivityLog(), m.throttle("enroll"), apis.BodyLimit(16<<10), rateTag("sync:enroll"))
			g.POST(proto.PathHandshake, m.handshakeHandler).
				Bind(apis.SkipSuccessActivityLog(), m.throttle("handshake"), apis.BodyLimit(64<<10), rateTag("sync:handshake"))
			g.GET(proto.PathPing, m.pingHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:ping"), m.nodeAuth())
			// the push body limit is enforced by the handler (413 sync_batch_too_large)
			g.POST(proto.PathPush, m.pushHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:push"), m.nodeAuth())
			g.GET(proto.PathPull, m.pullHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:pull"), m.nodeAuth())
			g.POST(proto.PathSnapshot, m.snapshotStartHandler).
				Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(16<<10), rateTag("sync:snapshot"), m.nodeAuth())
			g.GET(proto.PathSnapshot, m.snapshotPageHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:snapshot"), m.nodeAuth())
			g.POST(proto.PathAck, m.ackHandler).
				Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(1<<20), rateTag("sync:ack"), m.nodeAuth())
			g.POST(proto.PathActor, m.actorHandler).
				Bind(apis.SkipSuccessActivityLog(), apis.BodyLimit(16<<10), rateTag("sync:actor"), m.nodeAuth())
			g.DELETE(proto.PathActor+"/{aid}", m.actorRevokeHandler).
				Bind(apis.SkipSuccessActivityLog(), rateTag("sync:actor"), m.nodeAuth())
			g.POST(proto.PathPurge, m.purgeHandler).
				Bind(apis.BodyLimit(16<<10), rateTag("sync:purge"), apis.RequireSuperuserAuth())
			// parked changes nobody resolved are rejected after TOKI_SYNC_PARK_TTL
			_ = se.App.Cron().Add("__tokiSyncParkTTL", "23 * * * *", func() {
				if _, err := m.ExpireParked(); err != nil {
					se.App.Logger().Error("sync: expiring parked changes failed", "error", err)
				}
			})
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

// throttle is the built-in per-IP limit of the unauthenticated routes. It does
// not depend on Settings > Rate limits (off by default).
func (m *Module) throttle(kind string) *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + "throttle-" + kind, Priority: -950,
		Func: func(e *core.RequestEvent) error {
			t, max := &m.guards.handshake, m.guards.handshakeMax()
			if kind == "enroll" {
				t, max = &m.guards.enroll, m.guards.enrollMax()
			}
			if !t.allow(throttleKey(e.RealIP()), time.Now(), max) {
				e.Response.Header().Set("Retry-After", throttleRetryAfterSc)
				return syncErr(e, http.StatusTooManyRequests, proto.CodeRateLimited, "Too many requests, retry later.", nil)
			}
			return e.Next()
		},
	}
}

var (
	nodeIDRe = regexp.MustCompile(`^n[a-z2-7]{14}$`)
	nonceRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
)

// issueCert signs a device certificate.
func (m *Module) issueCert(nodeID string, edPub, kxPub []byte, ser string, params map[string]any, now time.Time) (string, error) {
	claims := &proto.CertClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: m.hub.id, Subject: nodeID,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(proto.CertValidity)),
		},
		Pub: b64(edPub), KX: b64(kxPub), Params: params, Ser: ser,
	}
	return proto.SignCert(m.hub.priv, claims)
}

func isConstraintErr(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "constraint")
}

// enrollHandler is POST /api/sync/enroll (no auth, the code is the secret).
// Bad, expired and used codes all give the same answer. A retry with the same
// key and code is answered again (a lost response must not burn the code)
// until the node completed its first handshake.
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
	if !ok1 || !ok2 || req.Code == "" || len(req.Code) > 128 {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	invalid := func() error {
		return syncErr(e, http.StatusBadRequest, proto.CodeEnrollInvalid, "The enrollment code is invalid, expired or already used.", nil)
	}

	hash := HashEnrollCode(req.Code)
	cands, err := e.App.FindRecordsByFilter(NodesCollection, "(status={:p} || status={:a}) && enroll_hash!=''", "", 0, 0,
		dbx.Params{"p": NodePending, "a": NodeActive})
	if err != nil {
		return err
	}
	// compare against every candidate row, no early exit
	var match *core.Record
	for _, r := range cands {
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
	now := time.Now().UTC()

	if match.GetString("status") == NodeActive {
		// idempotent retry: only the same key, before the first handshake
		if match.Id != nodeID || match.GetString("pubkey") != b64(edPub) || match.GetString("kx_pubkey") != b64(kxPub) ||
			!match.GetDateTime("last_seen").IsZero() || match.GetString("cert_serial") == "" {
			return invalid()
		}
		var params map[string]any
		_ = match.UnmarshalJSONField("params", &params)
		cert, err := m.issueCert(nodeID, edPub, kxPub, match.GetString("cert_serial"), params, now)
		if err != nil {
			return err
		}
		res, err := e.App.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET cert_expires={:ce} WHERE id={:id} AND status={:a} AND enroll_hash={:h}").
			Bind(dbx.Params{"ce": now.Add(proto.CertValidity).Format(types.DefaultDateLayout), "id": nodeID, "a": NodeActive, "h": hash}).Execute()
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return invalid()
		}
		emit(AuditNodeEnroll, NodesCollection, nodeID, map[string]any{
			"name": match.GetString("name"), "profile": match.GetString("profile"), "stage": "reissued", "ip": e.RealIP(),
		})
		return e.JSON(http.StatusOK, proto.EnrollResponse{
			NodeID: nodeID, HubID: m.hub.id, HubURL: m.hubURL(e), Cert: cert, HubPub: b64(m.hub.pub),
		})
	}

	var cert string
	err = e.App.RunInTransaction(func(tx kernel.App) error {
		// claim the code atomically: only one request can flip pending -> active
		// (the hash stays until the first handshake so that a lost answer can be repeated)
		res, err := tx.DB().NewQuery("UPDATE " + NodesCollection + " SET status={:a} WHERE id={:id} AND status={:p} AND enroll_hash={:h}").
			Bind(dbx.Params{"a": NodeActive, "p": NodePending, "id": match.Id, "h": hash}).Execute()
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errEnrollUsed
		}
		var taken int
		if err := tx.DB().NewQuery("SELECT COUNT(*) FROM " + NodesCollection + " WHERE id={:n}").Bind(dbx.Params{"n": nodeID}).Row(&taken); err != nil {
			return err
		}
		if taken > 0 {
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
		serHex := hex.EncodeToString(ser[:])
		if cert, err = m.issueCert(nodeID, edPub, kxPub, serHex, params, now); err != nil {
			return err
		}
		rec.Set("pubkey", b64(edPub))
		rec.Set("kx_pubkey", b64(kxPub))
		rec.Set("cert_serial", serHex)
		rec.Set("cert_expires", now.Add(proto.CertValidity))
		if req.AppVersion != "" {
			rec.Set("app_version", truncate(req.AppVersion, 64))
		}
		// the profile is chosen by the admin; the spoke's value is ignored
		if err := tx.Save(rec); err != nil {
			return err
		}
		// the row id becomes the key-derived node id
		_, err = tx.DB().NewQuery("UPDATE " + NodesCollection + " SET id={:n} WHERE id={:o}").
			Bind(dbx.Params{"n": nodeID, "o": match.Id}).Execute()
		return err
	})
	if errors.Is(err, errEnrollUsed) || isConstraintErr(err) {
		return invalid()
	}
	if err != nil {
		e.App.Logger().Error("sync: enroll failed", "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "enrollment failed", nil)
	}
	emit(AuditNodeEnroll, NodesCollection, nodeID, map[string]any{
		"name": match.GetString("name"), "profile": match.GetString("profile"), "stage": "completed",
		"pending_id": match.Id, "ip": e.RealIP(),
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

// failHandshake audits a failure of a KNOWN node and answers.
func (m *Module) failHandshake(e *core.RequestEvent, node, reason string, status int, code, msg string, extra map[string]any) error {
	emit(AuditHandshakeFailed, NodesCollection, node, map[string]any{"node": node, "reason": reason, "ip": e.RealIP()})
	return syncErr(e, status, code, msg, extra)
}

// audienceHosts are the hosts a signed handshake may be addressed to: the host
// of the request and the host of the configured app URL (a proxy may rewrite
// the former).
func (m *Module) audienceHosts(e *core.RequestEvent) []string {
	hosts := []string{proto.NormalizeHost(e.Request.Host)}
	if u, err := url.Parse(strings.TrimSpace(e.App.Settings().Meta.AppURL)); err == nil && u.Host != "" {
		if h := proto.NormalizeHost(u.Host); h != hosts[0] {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// stampTime sets the signed hub time headers of the answer to a signed
// handshake. The client only trusts a hub time that carries a valid signature.
func (m *Module) stampTime(e *core.RequestEvent, node, ts, nonce string, now time.Time) string {
	st := now.UTC().Format(proto.TimeLayout)
	h := e.Response.Header()
	h.Set(proto.HeaderServerTime, st)
	h.Set(proto.HeaderServerSig, proto.SignServerTime(m.hub.priv, node, ts, nonce, st))
	return st
}

// touchNode records a successful handshake with a targeted UPDATE of the
// columns it owns. It never rewrites the whole row (a stale Save could undo a
// concurrent revoke) and only matches a live node (not revoked, not pending)
// whose replay floor is below ts. It reports whether a row was changed.
func (m *Module) touchNode(id string, now time.Time, offset int64, schema int64, appVersion string, tsMs int64, certExpires time.Time) (bool, error) {
	ce := ""
	if !certExpires.IsZero() {
		ce = certExpires.UTC().Format(types.DefaultDateLayout)
	}
	t := now.UTC().Format(types.DefaultDateLayout)
	res, err := m.app.NonconcurrentDB().NewQuery("UPDATE " + NodesCollection + " SET last_seen={:t}, updated={:t}, clock_offset_ms={:o}, max_skew_ms=MAX(COALESCE(max_skew_ms,0), ABS({:o})), schema_version={:sv}, " +
		"app_version=CASE WHEN {:av}!='' THEN {:av} ELSE app_version END, enroll_hash='', enroll_expires='', sig_ts_floor={:ts}, " +
		"cert_expires=CASE WHEN {:ce}!='' THEN {:ce} ELSE cert_expires END " +
		"WHERE id={:id} AND status IN ({:s1},{:s2},{:s3}) AND sig_ts_floor<{:ts}").
		Bind(dbx.Params{"t": t, "o": offset, "sv": schema, "av": truncate(appVersion, 64), "ts": tsMs, "ce": ce, "id": id,
			"s1": NodeActive, "s2": NodeStale, "s3": NodeRebootstrap}).Execute()
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// handshakeHandler is POST /api/sync/handshake (signed, see proto.SigningDigest).
func (m *Module) handshakeHandler(e *core.RequestEvent) error {
	if !m.hubReady() {
		return syncErr(e, http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready", nil)
	}
	nodeID := e.Request.Header.Get(proto.HeaderNode)
	ts := e.Request.Header.Get(proto.HeaderSigTs)
	nonce := e.Request.Header.Get(proto.HeaderNonce)
	sig := e.Request.Header.Get(proto.HeaderSig)
	// cheap shape checks first: nothing below runs for junk
	if nodeID == "" || ts == "" || nonce == "" || sig == "" || len(nodeID) > maxNodeHeaderLen || len(ts) > 20 || len(sig) > 128 ||
		!nodeIDRe.MatchString(nodeID) || !nonceRe.MatchString(nonce) {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "missing or malformed signature headers", nil)
	}
	body, err := io.ReadAll(e.Request.Body)
	if err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}
	var req proto.HandshakeRequest
	if err := json.Unmarshal(body, &req); err != nil || req.NodeID != nodeID {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid request body", nil)
	}

	node, err := e.App.FindRecordById(NodesCollection, nodeID)
	if err != nil || node.GetString("pubkey") == "" {
		// unknown node: counted and logged at debug level, never audited
		m.guards.unknownHS.Add(1)
		e.App.Logger().Debug("sync: handshake for an unknown node", "node", nodeID, "ip", e.RealIP())
		return syncErr(e, http.StatusUnauthorized, proto.CodeUnauthorized, "The handshake was rejected.", nil)
	}
	unauth := func(reason string, extra map[string]any) error {
		return m.failHandshake(e, nodeID, reason, http.StatusUnauthorized, proto.CodeUnauthorized, "The handshake was rejected.", extra)
	}
	revoked := func() error {
		return m.failHandshake(e, nodeID, "revoked", http.StatusForbidden, proto.CodeNodeRevoked, "This node was revoked.", nil)
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
	sigOK := false
	for _, host := range m.audienceHosts(e) {
		if proto.VerifyRequest(nodePub, e.Request.Method, e.Request.URL.Path, host, m.hub.id, ts, nonce, body, sig) {
			sigOK = true
			break
		}
	}
	if !sigOK {
		return unauth("bad_signature", nil)
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return unauth("bad_ts", nil)
	}
	if d := now.Sub(time.UnixMilli(ms)); d > SigWindow || d < -SigWindow {
		// the signature is valid, so the caller may learn the hub time (signed, so
		// that the caller can tell that it really comes from this hub)
		st := m.stampTime(e, nodeID, ts, nonce, now)
		return unauth("ts_window", map[string]any{"server_time": st})
	}
	// the signature is genuine from here on: only now the status is revealed
	switch node.GetString("status") {
	case NodeRevoked:
		return revoked()
	case NodePending:
		return unauth("pending", nil)
	}
	if ms <= int64(node.GetFloat("sig_ts_floor")) {
		return unauth("replay_floor", nil)
	}
	if !m.nonces.Use(nodeID, nonce, now) {
		return unauth("nonce_replay", nil)
	}

	// clock offset as the hub sees it: server_time - client_time
	ct, err := time.Parse(time.RFC3339Nano, req.ClientTime)
	if err != nil {
		return syncErr(e, http.StatusBadRequest, proto.CodeBadRequest, "invalid client_time", nil)
	}
	offset := now.Sub(ct).Milliseconds()
	drift := maxDrift()
	clockOK := offset <= drift.Milliseconds() && offset >= -drift.Milliseconds()

	var params map[string]any
	_ = node.UnmarshalJSONField("params", &params)
	if params == nil {
		params = map[string]any{}
	}
	// a certificate that expires within 30 days is renewed (same serial, so a
	// lost answer leaves the old certificate usable until it expires)
	var newCert string
	var certExp time.Time
	if claims.ExpiresAt != nil && claims.ExpiresAt.Sub(now) < proto.CertRenewBefore {
		kx, _ := decodeKey(node.GetString("kx_pubkey"), 32)
		if kx != nil {
			if c, err := m.issueCert(nodeID, nodePub, kx, claims.Ser, params, now); err == nil {
				newCert, certExp = c, now.Add(proto.CertValidity)
			}
		}
	}

	// targeted update: never rewrites status/revoked_at (see touchNode)
	touched, err := m.touchNode(nodeID, now, offset, req.SchemaVersion, req.AppVersion, ms, certExp)
	if err != nil {
		e.App.Logger().Error("sync: failed to update the node", "error", err)
		return syncErr(e, http.StatusInternalServerError, "sync_internal", "handshake failed", nil)
	}
	// re-check the status after the write: a revoke that raced with it wins
	cur, err := e.App.FindRecordById(NodesCollection, nodeID)
	if err != nil {
		return unauth("node_gone", nil)
	}
	switch cur.GetString("status") {
	case NodeRevoked:
		return revoked()
	case NodePending:
		return unauth("pending", nil)
	}
	if !touched {
		return unauth("replay_floor", nil)
	}

	// compaction (§3.6): a stale node, or a cursor older than the oldest kept
	// change, has to re-bootstrap
	m.noteHead()
	low := m.lowWater()
	rebootstrap := cur.GetString("status") == NodeStale || cur.GetString("status") == NodeRebootstrap || req.PullAfter < low ||
		m.epochRequiresRebootstrap(req.HubEpoch, req.PullAfter)
	// PR8: schema bundles newer than the node's version; a node too far behind re-bootstraps
	schema, schemaTooOld := m.handshakeSchema(nodeID, req.SchemaVersion)
	rebootstrap = rebootstrap || schemaTooOld

	expires := now.Add(SessionTTL)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"typ": proto.SessionTokenType, "sub": nodeID, "iss": m.hub.id,
		"iat": now.Unix(), "exp": expires.Unix(),
	}).SignedString(m.hub.secret)
	if err != nil {
		return err
	}
	serverTime := m.stampTime(e, nodeID, ts, nonce, now)
	return e.JSON(http.StatusOK, proto.HandshakeResponse{
		SessionToken: tok,
		Cert:         newCert,
		Expires:      expires.UTC().Format(proto.TimeLayout),
		HubID:        m.hub.id,
		HubEpoch:     m.hub.epoch,
		HubEpochSeq:  m.hub.epochSeq,
		ServerTime:   serverTime,
		Clock:        proto.Clock{Ok: clockOK, OffsetMs: offset, MaxDriftMs: drift.Milliseconds()}, // enforced on push (drift.go)
		Schema:       schema,
		Policies:     m.handshakePolicies(),
		Params:       params,
		Keys:         []any{}, // TODO(PR9): wrapped collection keys
		PushFrom:     int64(cur.GetFloat("pushed_origin_seq")) + 1,
		LowWater:     low,
		Rebootstrap:  rebootstrap,
		Reservations: m.handshakeReservations(nodeID),
		PollMs:       DefaultPollMs,
		Caps:         []string{proto.CapFiller},
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
			Strategy: r.GetString("strategy"), Partition: strings.TrimSpace(r.GetString("partition")),
			FieldTypes: map[string]string{}, Exclude: []string{}, Crypto: r.GetString("crypto"),
		}
		if p.Direction == "" {
			p.Direction = DirBoth
		}
		if p.Strategy == "" {
			p.Strategy = "lww"
		}
		if p.Crypto == "" {
			p.Crypto = "ciphertext"
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

// sessionNode validates `Authorization: Bearer <session_token>` and returns
// the node id. On failure status is the HTTP status (0 = ok) with the error
// code and message of docs/SYNC_DESIGN.md §3.12.
func (m *Module) sessionNode(app core.App, authz string) (nodeID string, status int, code, msg string) {
	if !m.hubReady() {
		return "", http.StatusServiceUnavailable, proto.CodeHubUnavailable, "sync hub is not ready"
	}
	f := strings.Fields(authz)
	if len(f) != 2 || !strings.EqualFold(f[0], "bearer") {
		return "", http.StatusUnauthorized, proto.CodeUnauthorized, "A node session token is required."
	}
	invalid := func() (string, int, string, string) {
		return "", http.StatusUnauthorized, proto.CodeUnauthorized, "The session token is invalid or expired."
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(f[1], claims, func(*jwt.Token) (any, error) { return m.hub.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithTimeFunc(m.now), jwt.WithExpirationRequired())
	sub, _ := claims["sub"].(string)
	if typ, _ := claims["typ"].(string); err != nil || typ != proto.SessionTokenType || sub == "" {
		return invalid()
	}
	node, err := app.FindRecordById(NodesCollection, sub)
	if err != nil {
		return invalid()
	}
	switch node.GetString("status") {
	case NodeRevoked:
		return "", http.StatusForbidden, proto.CodeNodeRevoked, "This node was revoked."
	case NodePending:
		return invalid()
	}
	return sub, 0, "", ""
}

// nodeAuth is the middleware of node-authenticated routes: it validates the
// `Authorization: Bearer <session_token>` and stores the node id in the request
// (see NodeFrom). 401 sync_unauthorized, 403 sync_node_revoked.
func (m *Module) nodeAuth() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: hookId + "nodeauth", Priority: -800,
		Func: func(e *core.RequestEvent) error {
			id, status, code, msg := m.sessionNode(e.App, e.Request.Header.Get("Authorization"))
			if status != 0 {
				return syncErr(e, status, code, msg, nil)
			}
			e.Set(ctxNodeKey, id)
			return e.Next()
		},
	}
}

// pingHandler is GET /api/sync/ping (node-authenticated).
func (m *Module) pingHandler(e *core.RequestEvent) error {
	return e.JSON(http.StatusOK, proto.PingResponse{NodeID: NodeFrom(e), ServerTime: m.now().UTC().Format(proto.TimeLayout)})
}
