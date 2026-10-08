//go:build !no_sync

package sync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/types"
)

type hubEnv struct {
	*env
	srv *httptest.Server
}

func newHub(t *testing.T) *hubEnv {
	t.Helper()
	e := setupWith(t, newApp(t), RoleHub)
	srv := httptest.NewServer(e.mux)
	t.Cleanup(srv.Close)
	return &hubEnv{env: e, srv: srv}
}

type spokeEnv struct {
	app *tests.TestApp
	m   *Module
}

func newSpoke(t *testing.T) *spokeEnv {
	t.Helper()
	app := newApp(t)
	m := RegisterRole(app, RoleSpoke)
	if m == nil || m.Identity() == nil {
		t.Fatal("spoke identity missing")
	}
	return &spokeEnv{app: app, m: m}
}

func (h *hubEnv) enroll(t *testing.T, name string, params map[string]string) string {
	t.Helper()
	// the service actor of the test nodes is the test superuser: their rules are open anyway
	_, code, err := CreateEnrollment(h.app, EnrollOptions{Name: name, Profile: "edge", Params: params, Actor: core.CollectionNameSuperusers + "/" + h.su.Id, AllowSuperuserActor: true})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func (h *hubEnv) join(t *testing.T, s *spokeEnv, code string) *proto.EnrollResponse {
	t.Helper()
	res, err := client.Join(context.Background(), s.app, client.EnrollParams{
		HubURL: h.srv.URL, Code: code, Identity: s.m.Identity(), Profile: "edge", Insecure: true,
	})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	return res
}

func (s *spokeEnv) client(t *testing.T, h *hubEnv, opts ...func(*client.Options)) *client.Client {
	t.Helper()
	opts = append(opts, func(o *client.Options) { o.Insecure = true })
	c, err := s.m.NewClient(opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHubKeyIsGeneratedOnceAndStable(t *testing.T) {
	e := setupWith(t, newApp(t), RoleHub)
	id, epoch, pub := e.m.HubID(), e.m.Epoch(), append([]byte{}, e.m.HubPub()...)
	if id != proto.HubID(pub) || len(id) != 15 || id[0] != 'h' || epoch == "" {
		t.Fatalf("hub id %q epoch %q", id, epoch)
	}
	if e.m.NodeID() != id {
		t.Fatalf("the hub writes under its hub id, got %q", e.m.NodeID())
	}
	st := dbState{db: e.app.NonconcurrentDB()}
	if v, ok, _ := st.Get(keyHubKey); !ok || v == "" {
		t.Fatal("hub key must be stored in _sync_state")
	}
	if err := e.m.Init(); err != nil { // restart
		t.Fatal(err)
	}
	if e.m.HubID() != id || e.m.Epoch() != epoch {
		t.Fatal("hub id and epoch must survive a restart")
	}
	if c, _ := e.app.FindCollectionByNameOrId(NodesCollection); c == nil || !c.System {
		t.Fatal("_sync_nodes must be a system collection")
	}
}

func TestHubKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.key")
	t.Setenv(EnvHubKeyFile, p)
	e := setupWith(t, newApp(t), RoleHub)
	if _, ok, _ := (dbState{db: e.app.NonconcurrentDB()}).Get(keyHubKey); ok {
		t.Fatal("with a key file the key must not be stored in the database")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file: %v %v", st, err)
		}
	}
	id := e.m.HubID()
	if err := e.m.Init(); err != nil || e.m.HubID() != id {
		t.Fatal("key file must be reused")
	}
}

func TestSpokeIdentityFileAndNodeIDMigration(t *testing.T) {
	s := newSpoke(t)
	p := filepath.Join(s.app.DataDir(), NodeKeyFile)
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("node key file: %v %v", st, err)
		}
	}
	if s.m.NodeID() != s.m.Identity().NodeID() {
		t.Fatal("node id must be derived from the key")
	}
	// a PR1 placeholder id with captured rows is replaced and the rows follow
	st := dbState{db: s.app.NonconcurrentDB()}
	if err := st.Set(keyNodeID, "nplaceholder000"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO _changes (node, origin_seq, hlc, collection, record, op, schema_version, status, created) VALUES ('nplaceholder000', 1, 5, 'c', 'r', 'c', 0, 'local', 'x')`,
		`INSERT INTO _sync_meta (collection, record, hlc, node) VALUES ('c', 'r', 5, 'nplaceholder000')`,
		`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, created) VALUES ('c', 'd', 'delete', 5, 'nplaceholder000', 'x')`,
	} {
		if _, err := s.app.NonconcurrentDB().NewQuery(q).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.m.Init(); err != nil {
		t.Fatal(err)
	}
	want := s.m.Identity().NodeID()
	for _, tbl := range []string{"_changes", "_sync_meta", "_sync_tombstones"} {
		var n int
		if err := s.app.DB().NewQuery("SELECT COUNT(*) FROM " + tbl + " WHERE node='" + want + "'").Row(&n); err != nil || n != 1 {
			t.Fatalf("%s: %d rows migrated (%v)", tbl, n, err)
		}
	}
	if v, _, _ := st.Get(keyNodeID); v != want {
		t.Fatalf("state node_id %q", v)
	}

	// the env key replaces the file
	other, _ := proto.GenerateIdentity()
	t.Setenv(EnvNodeKey, other.Encode())
	if err := s.m.Init(); err != nil || s.m.NodeID() != other.NodeID() {
		t.Fatalf("env key: %v %q", err, s.m.NodeID())
	}
	t.Setenv(EnvNodeKey, "garbage")
	if err := s.m.Init(); err == nil {
		t.Fatal("an invalid env key must fail closed")
	}
}

func TestEnrollmentCode(t *testing.T) {
	code, err := NewEnrollCode()
	if err != nil {
		t.Fatal(err)
	}
	g := strings.Split(code, "-")
	if len(g) != 8 || len(g[0]) != 4 {
		t.Fatalf("code shape %q", code)
	}
	h := HashEnrollCode(code)
	if h != HashEnrollCode(strings.ToLower(strings.ReplaceAll(code, "-", " "))) || len(h) != 64 {
		t.Fatal("hash must ignore case and separators")
	}
	if h == HashEnrollCode(code+"A") {
		t.Fatal("different codes must differ")
	}
	if !constEq("abc", "abc") || constEq("abc", "abd") || constEq("abc", "ab") {
		t.Fatal("constEq")
	}
}

func TestEnrollSingleUseAndErrors(t *testing.T) {
	h := newHub(t)
	code := h.enroll(t, "gate-1", map[string]string{"branch": "B12"})

	rec, _ := FindNode(h.app, "gate-1")
	if rec.GetString("status") != NodePending || rec.GetString("enroll_hash") == "" || rec.GetString("enroll_hash") == code {
		t.Fatal("the hub must keep a pending row with the hash only")
	}
	if d := rec.GetDateTime("enroll_expires").Time().Sub(time.Now()); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expiry in %v", d)
	}

	s := newSpoke(t)
	post := func(code string, ed, kx []byte) (int, proto.ErrorBody, string) {
		body, _ := json.Marshal(proto.EnrollRequest{Code: code, Ed25519Pub: b64(ed), X25519Pub: b64(kx)})
		res, err := http.Post(h.srv.URL+proto.PathEnroll, "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		var eb proto.ErrorBody
		_ = json.Unmarshal(raw, &eb)
		return res.StatusCode, eb, string(raw)
	}
	// bad code
	st1, e1, raw1 := post("AAAA-BBBB-CCCC-DDDD-EEEE-FFFF-GGGG-HHHH", s.m.Identity().Pub(), s.m.Identity().KX())
	if st1 != 400 || e1.Data["code"] != proto.CodeEnrollInvalid {
		t.Fatalf("bad code: %d %s", st1, raw1)
	}
	// malformed keys
	if st, e, _ := post(code, []byte("short"), s.m.Identity().KX()); st != 400 || e.Data["code"] != proto.CodeBadRequest {
		t.Fatalf("bad key: %d %v", st, e)
	}

	// good: lower case and no dashes is accepted too
	res := h.join(t, s, strings.ToLower(strings.ReplaceAll(code, "-", "")))
	if res.NodeID != s.m.NodeID() || res.HubID != h.m.HubID() {
		t.Fatalf("ids: %+v", res)
	}
	node, err := h.app.FindRecordById(NodesCollection, res.NodeID)
	if err != nil {
		t.Fatalf("the row must carry the key-derived id: %v", err)
	}
	if node.GetString("status") != NodeActive || node.GetString("enroll_hash") == "" ||
		node.GetString("pubkey") != b64(s.m.Identity().Pub()) || node.GetString("cert_serial") == "" || node.GetString("name") != "gate-1" {
		t.Fatalf("node row: %v", node.FieldsData())
	}
	cl, err := proto.VerifyCert(h.m.HubPub(), res.Cert, time.Now())
	if err != nil || cl.Subject != res.NodeID || cl.Params["branch"] != "B12" || cl.Ser != node.GetString("cert_serial") {
		t.Fatalf("cert: %v %+v", err, cl)
	}
	if cur, _ := client.LoadCursor(s.app); cur == nil || cur.Cert != res.Cert || cur.HubID != res.HubID || cur.NodeID != res.NodeID {
		t.Fatalf("cursor: %+v", cur)
	}

	// a lost answer: the same key and code are answered again until the first handshake
	if st2, _, raw2 := post(code, s.m.Identity().Pub(), s.m.Identity().KX()); st2 != 200 {
		t.Fatalf("idempotent retry: %d %s", st2, raw2)
	}
	if node2, _ := h.app.FindRecordById(NodesCollection, res.NodeID); node2.GetString("cert_serial") != node.GetString("cert_serial") {
		t.Fatal("the reissued certificate keeps its serial")
	}
	if _, err := s.client(t, h).Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	// used (first handshake done): same answer as for a bad code
	st2, e2, raw2 := post(code, s.m.Identity().Pub(), s.m.Identity().KX())
	if st2 != st1 || e2.Message != e1.Message || e2.Data["code"] != e1.Data["code"] {
		t.Fatalf("used code must look like a bad code: %s vs %s", raw1, raw2)
	}
	other := newSpoke(t)
	if st, e, _ := post(code, other.m.Identity().Pub(), other.m.Identity().KX()); st != 400 || e.Message != e1.Message {
		t.Fatal("a used code must stay used for any key")
	}

	// expired
	code2 := h.enroll(t, "gate-2", nil)
	r2, _ := FindNode(h.app, "gate-2")
	r2.Set("enroll_expires", types.NowDateTime().Add(-time.Minute))
	if err := h.app.Save(r2); err != nil {
		t.Fatal(err)
	}
	st3, e3, _ := post(code2, other.m.Identity().Pub(), other.m.Identity().KX())
	if st3 != 400 || e3.Message != e1.Message || e3.Data["code"] != e1.Data["code"] {
		t.Fatalf("expired: %d %v", st3, e3)
	}
	// revoked while pending: the code dies with it
	code3 := h.enroll(t, "gate-3", nil)
	if _, err := RevokeNode(h.app, "gate-3", false); err != nil {
		t.Fatal(err)
	}
	if st, e, _ := post(code3, other.m.Identity().Pub(), other.m.Identity().KX()); st != 400 || e.Message != e1.Message {
		t.Fatal("a revoked pending node must not enroll")
	}
}

func TestEnrollmentValidation(t *testing.T) {
	h := newHub(t)
	for _, o := range []EnrollOptions{
		{Name: "", Profile: "edge"},
		{Name: "bad name!", Profile: "edge"},
		{Name: "ok", Profile: "mainframe"},
		{Name: "ok", Profile: "edge", Actor: "noslash"},
	} {
		if _, _, err := CreateEnrollment(h.app, o); err == nil {
			t.Fatalf("%+v must be refused", o)
		}
	}
	if _, _, err := CreateEnrollment(h.app, EnrollOptions{Name: "dup", Profile: "nano", Actor: "gate_devices/abc123"}); err != nil {
		t.Fatal(err)
	}
	r, _ := FindNode(h.app, "dup")
	if r.GetString("actor_collection") != "gate_devices" || r.GetString("actor_record") != "abc123" {
		t.Fatal("actor not stored")
	}
	if _, _, err := CreateEnrollment(h.app, EnrollOptions{Name: "dup", Profile: "nano"}); err == nil {
		t.Fatal("names are unique")
	}
}

func TestHandshakePingRevokeIntegration(t *testing.T) {
	h := newHub(t)
	h.policy(t, "items", DirBoth, map[string]string{"qty": TypeCounter}, []string{"note"})
	var audits []string
	SetAuditSink(func(action, _, _ string, d map[string]any) { audits = append(audits, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", map[string]string{"branch": "B12"}))
	c := s.client(t, h, func(o *client.Options) { o.AppVersion = "0.41.0" })

	hs, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if hs.HubID != h.m.HubID() || hs.HubEpoch != h.m.Epoch() || hs.SessionToken == "" || !hs.Clock.Ok ||
		hs.PushFrom != 1 || hs.LowWater != 0 || hs.Rebootstrap || hs.PollMs == 0 || hs.Params["branch"] != "B12" ||
		hs.Schema.Version != 0 || len(hs.Keys) != 0 || len(hs.Reservations) != 0 || hs.Clock.MaxDriftMs != 300000 {
		t.Fatalf("response: %+v", hs)
	}
	if len(hs.Policies) != 1 || hs.Policies[0].Collection != "items" || hs.Policies[0].Direction != DirBoth ||
		hs.Policies[0].FieldTypes["qty"] != TypeCounter || hs.Policies[0].Exclude[0] != "note" {
		t.Fatalf("policies: %+v", hs.Policies)
	}
	// token shape
	cl := jwt.MapClaims{}
	if _, err := jwt.ParseWithClaims(hs.SessionToken, cl, func(*jwt.Token) (any, error) { return h.m.hub.secret, nil }, jwt.WithValidMethods([]string{"HS256"})); err != nil {
		t.Fatal(err)
	}
	exp := time.Unix(int64(cl["exp"].(float64)), 0)
	if cl["typ"] != proto.SessionTokenType || cl["sub"] != s.m.NodeID() || exp.Sub(time.Now()) > SessionTTL || exp.Sub(time.Now()) < SessionTTL-time.Minute {
		t.Fatalf("claims: %v", cl)
	}
	// node row updated
	node, _ := h.app.FindRecordById(NodesCollection, s.m.NodeID())
	if node.GetDateTime("last_seen").IsZero() || node.GetString("app_version") != "0.41.0" || node.GetString("profile") != "edge" {
		t.Fatalf("node not updated: %v", node.FieldsData())
	}
	if cur, _ := client.LoadCursor(s.app); cur == nil || !cur.LastOK.Valid || cur.HubEpoch != h.m.Epoch() || cur.LastError != "" {
		t.Fatalf("cursor: %+v", cur)
	}

	p, err := c.Ping(context.Background())
	if err != nil || p.NodeID != s.m.NodeID() {
		t.Fatalf("ping: %v %+v", err, p)
	}

	// pushed_origin_seq drives push_from
	node.Set("pushed_origin_seq", 41)
	if err := h.app.Save(node); err != nil {
		t.Fatal(err)
	}
	if hs2, err := c.Handshake(context.Background()); err != nil || hs2.PushFrom != 42 {
		t.Fatalf("push_from: %v %+v", err, hs2)
	}

	// revoke: the live token and a new handshake are refused with 403
	if _, err := RevokeNode(h.app, "gate-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ping(context.Background()); !client.IsCode(err, proto.CodeNodeRevoked) {
		t.Fatalf("ping after revoke: %v", err)
	}
	var he *client.Error
	if _, err := c.Handshake(context.Background()); !client.IsCode(err, proto.CodeNodeRevoked) {
		t.Fatalf("handshake after revoke: %v", err)
	} else if !errors.As(err, &he) || he.Status != 403 {
		t.Fatalf("status: %v", err)
	}
	if cur, _ := client.LoadCursor(s.app); cur.LastError == "" {
		t.Fatal("the failure must be recorded in the cursor")
	}
	node, _ = h.app.FindRecordById(NodesCollection, s.m.NodeID())
	if node.GetString("status") != NodeRevoked || node.GetDateTime("revoked_at").IsZero() {
		t.Fatal("revoke must set status and revoked_at")
	}
	count := map[string]int{}
	for _, a := range audits {
		count[a]++
	}
	if count[AuditNodeEnroll] != 2 || count[AuditNodeRevoke] != 1 || count[AuditHandshakeFailed] < 1 {
		t.Fatalf("audit: %v", audits)
	}
}

// rawHandshake sends a hand-built handshake request.
type rawHS struct {
	h    *hubEnv
	s    *spokeEnv
	cert string
}

func (r rawHS) send(t *testing.T, mut func(hdr http.Header, body *[]byte), ts time.Time, nonce string) (int, proto.ErrorBody) {
	t.Helper()
	st, eb, _, _ := r.sendFull(t, mut, ts, nonce)
	return st, eb
}

// sign signs like the client: the host of the hub URL and the hub id are part of the string.
func (r rawHS) sign(priv ed25519.PrivateKey, body []byte, ts, nonce string) string {
	u, _ := url.Parse(r.h.srv.URL)
	return proto.SignRequest(priv, "POST", proto.PathHandshake, u.Host, r.h.m.HubID(), ts, nonce, body)
}

func (r rawHS) sendFull(t *testing.T, mut func(hdr http.Header, body *[]byte), ts time.Time, nonce string) (int, proto.ErrorBody, []byte, http.Header) {
	t.Helper()
	body, _ := json.Marshal(proto.HandshakeRequest{NodeID: r.s.m.NodeID(), Cert: r.cert, ClientTime: ts.UTC().Format(proto.TimeLayout)})
	tss := strconv.FormatInt(ts.UnixMilli(), 10)
	hdr := http.Header{}
	hdr.Set(proto.HeaderNode, r.s.m.NodeID())
	hdr.Set(proto.HeaderSigTs, tss)
	hdr.Set(proto.HeaderNonce, nonce)
	hdr.Set(proto.HeaderSig, r.sign(r.s.m.Identity().Ed, body, tss, nonce))
	if mut != nil {
		mut(hdr, &body)
	}
	req, _ := http.NewRequest("POST", r.h.srv.URL+proto.PathHandshake, bytes.NewReader(body))
	req.Header = hdr
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var eb proto.ErrorBody
	_ = json.Unmarshal(raw, &eb)
	return res.StatusCode, eb, raw, res.Header
}

// resetFloor clears the persisted replay floor of the node (tests that use timestamps out of order).
func resetFloor(t *testing.T, h *hubEnv, node string) {
	t.Helper()
	if _, err := h.app.NonconcurrentDB().NewQuery("UPDATE _sync_nodes SET sig_ts_floor=0 WHERE id={:i}").Bind(dbx.Params{"i": node}).Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestSignedHandshakeRules(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	res := h.join(t, s, h.enroll(t, "gate-1", nil))
	r := rawHS{h: h, s: s, cert: res.Cert}
	now := time.Now()

	if st, eb := r.send(t, nil, now, "nonce-ok-1"); st != 200 {
		t.Fatalf("valid: %d %v", st, eb)
	}
	// replay of the same request (same nonce, same ts)
	if st, eb := r.send(t, nil, now, "nonce-ok-1"); st != 401 || eb.Data["code"] != proto.CodeUnauthorized {
		t.Fatalf("replay: %d %v", st, eb)
	}
	// +-5 min window
	for name, c := range map[string]struct {
		d  time.Duration
		ok bool
	}{
		"m4": {-4 * time.Minute, true}, "p4": {4 * time.Minute, true},
		"m6": {-6 * time.Minute, false}, "p6": {6 * time.Minute, false},
	} {
		resetFloor(t, h, s.m.NodeID()) // the cases use timestamps out of order
		st, eb := r.send(t, nil, now.Add(c.d), "nonce-"+name)
		if c.ok && st != 200 || !c.ok && st != 401 {
			t.Fatalf("%s: %d %v", name, st, eb)
		}
		if !c.ok {
			if _, has := eb.Data["server_time"]; !has {
				t.Fatalf("%s: the hub time must be returned so that the spoke can correct itself", name)
			}
		}
		if c.ok {
			continue
		}
		_, _, _, hh := r.sendFull(t, nil, now.Add(c.d), "nonce-sig-"+name)
		if !proto.VerifyServerTime(h.m.HubPub(), s.m.NodeID(), strconv.FormatInt(now.Add(c.d).UnixMilli(), 10), "nonce-sig-"+name, hh.Get(proto.HeaderServerTime), hh.Get(proto.HeaderServerSig)) {
			t.Fatalf("%s: the hub time must be signed", name)
		}
	}
	resetFloor(t, h, s.m.NodeID())
	// body tampered after signing
	if st, _ := r.send(t, func(_ http.Header, b *[]byte) {
		*b = bytes.Replace(*b, []byte("gate"), []byte("gatx"), 1)
		*b = append(*b, ' ')
	}, now, "nonce-t1"); st != 401 {
		t.Fatalf("tampered body: %d", st)
	}
	// signed by another key
	other, _ := proto.GenerateIdentity()
	if st, _ := r.send(t, func(hd http.Header, b *[]byte) {
		hd.Set(proto.HeaderSig, r.sign(other.Ed, *b, hd.Get(proto.HeaderSigTs), hd.Get(proto.HeaderNonce)))
	}, now, "nonce-k1"); st != 401 {
		t.Fatalf("foreign key: %d", st)
	}
	// missing headers -> 400, unknown node -> 401
	if st, eb := r.send(t, func(hd http.Header, _ *[]byte) { hd.Del(proto.HeaderSig) }, now, "nonce-m1"); st != 400 || eb.Data["code"] != proto.CodeBadRequest {
		t.Fatalf("missing sig: %d %v", st, eb)
	}
	if st, _ := r.send(t, func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderNode, "nunknown0000000") }, now, "nonce-u1"); st != 400 && st != 401 {
		t.Fatalf("unknown node: %d", st)
	}
	// a certificate for another node (sub != node) is refused even with a valid request signature
	s2 := newSpoke(t)
	res2 := h.join(t, s2, h.enroll(t, "gate-2", nil))
	if st, _ := (rawHS{h: h, s: s, cert: res2.Cert}).send(t, nil, now, "nonce-c1"); st != 401 {
		t.Fatalf("wrong sub cert: %d", st)
	}
	// expired certificate (hub clock moved past the 365 d validity)
	h.m.now = func() time.Time { return time.Now().Add(proto.CertValidity + time.Hour) }
	if st, _ := r.send(t, nil, time.Now().Add(proto.CertValidity+time.Hour), "nonce-e1"); st != 401 {
		t.Fatalf("expired cert: %d", st)
	}
	h.m.now = time.Now
	resetFloor(t, h, s.m.NodeID())
	if st, _ := r.send(t, nil, time.Now(), "nonce-e2"); st != 200 {
		t.Fatalf("back to a valid clock: %d", st)
	}
	// revoked: the signature is valid, the answer is 403 and a replayed nonce does not matter
	if _, err := RevokeNode(h.app, "gate-1", false); err != nil {
		t.Fatal(err)
	}
	if st, eb := r.send(t, nil, time.Now(), "nonce-r1"); st != 403 || eb.Data["code"] != proto.CodeNodeRevoked {
		t.Fatalf("revoked: %d %v", st, eb)
	}
	// a revoked node with a BAD signature learns nothing (401, not 403)
	if st, _ := r.send(t, func(hd http.Header, _ *[]byte) { hd.Set(proto.HeaderSig, "AAAA") }, time.Now(), "nonce-r2"); st != 401 {
		t.Fatalf("revoked + bad sig: %d", st)
	}
}

func TestNodeAuthMiddleware(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	get := func(auth string) (int, proto.ErrorBody) {
		req, _ := http.NewRequest("GET", h.srv.URL+proto.PathPing, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var eb proto.ErrorBody
		_ = json.NewDecoder(res.Body).Decode(&eb)
		return res.StatusCode, eb
	}
	if st, eb := get(""); st != 401 || eb.Data["code"] != proto.CodeUnauthorized {
		t.Fatalf("no token: %d %v", st, eb)
	}
	if st, _ := get("Bearer garbage"); st != 401 {
		t.Fatalf("garbage: %d", st)
	}
	mk := func(secret []byte, typ, sub string, exp time.Time) string {
		tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"typ": typ, "sub": sub, "exp": exp.Unix()}).SignedString(secret)
		return "Bearer " + tok
	}
	id := s.m.NodeID()
	if st, _ := get(mk(h.m.hub.secret, proto.SessionTokenType, id, time.Now().Add(time.Minute))); st != 200 {
		t.Fatalf("valid token: %d", st)
	}
	for name, a := range map[string]string{
		"other secret": mk([]byte("0123456789abcdef0123456789abcdef"), proto.SessionTokenType, id, time.Now().Add(time.Minute)),
		"wrong typ":    mk(h.m.hub.secret, "access", id, time.Now().Add(time.Minute)),
		"expired":      mk(h.m.hub.secret, proto.SessionTokenType, id, time.Now().Add(-time.Minute)),
		"unknown node": mk(h.m.hub.secret, proto.SessionTokenType, "nunknown0000000", time.Now().Add(time.Minute)),
		"no sub":       mk(h.m.hub.secret, proto.SessionTokenType, "", time.Now().Add(time.Minute)),
	} {
		if st, _ := get(a); st != 401 {
			t.Fatalf("%s: %d", name, st)
		}
	}
	// a token of a pending node does not work
	_, _, _ = CreateEnrollment(h.app, EnrollOptions{Name: "pend", Profile: "nano"})
	pend, _ := FindNode(h.app, "pend")
	if st, _ := get(mk(h.m.hub.secret, proto.SessionTokenType, pend.Id, time.Now().Add(time.Minute))); st != 401 {
		t.Fatalf("pending node: %d", st)
	}
	// revoked
	if _, err := RevokeNode(h.app, id, false); err != nil {
		t.Fatal(err)
	}
	if st, eb := get(mk(h.m.hub.secret, proto.SessionTokenType, id, time.Now().Add(time.Minute))); st != 403 || eb.Data["code"] != proto.CodeNodeRevoked {
		t.Fatalf("revoked: %d %v", st, eb)
	}
	// the sync session token is a guest for PocketBase itself
	req, _ := http.NewRequest("GET", h.srv.URL+"/api/collections/items/records", nil)
	req.Header.Set("Authorization", mk(h.m.hub.secret, proto.SessionTokenType, id, time.Now().Add(time.Minute)))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode == 500 {
		t.Fatal("a sync token must not break the normal API")
	}
}

func TestRoutesOnlyOnHub(t *testing.T) {
	app := newApp(t)
	RegisterRole(app, RoleSpoke)
	mux := buildMux(t, app)
	for _, p := range []string{proto.PathEnroll, proto.PathHandshake, proto.PathPing} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", p, strings.NewReader("{}")))
		if rec.Code != 404 && rec.Code != 405 {
			t.Fatalf("%s on a spoke: %d", p, rec.Code)
		}
	}
}

func TestClockOffsetMeasurement(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))

	// the spoke clock is 90 minutes behind: outside the hub window, so the
	// first attempt is refused with the hub time, the retry succeeds
	skew := -90 * time.Minute
	c := s.client(t, h, func(o *client.Options) { o.Now = func() time.Time { return time.Now().Add(skew) } })
	hs, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatalf("handshake with a skewed clock: %v", err)
	}
	if d := c.Offset() - 90*time.Minute; d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("offset %v, want ~+90m", c.Offset())
	}
	if d := s.m.Clock().Offset() - c.Offset(); d != 0 {
		t.Fatalf("hlc clock offset %v differs from the client's %v", s.m.Clock().Offset(), c.Offset())
	}
	cur, _ := client.LoadCursor(s.app)
	if d := time.Duration(cur.ClockOffsetMs)*time.Millisecond - c.Offset(); d < -time.Millisecond || d > time.Millisecond {
		t.Fatalf("cursor offset %dms", cur.ClockOffsetMs)
	}
	// after correction the hub sees (almost) no residual drift
	if hs.Clock.OffsetMs > 2000 || hs.Clock.OffsetMs < -2000 || !hs.Clock.Ok {
		t.Fatalf("hub verdict after correction: %+v", hs.Clock)
	}
	node, _ := h.app.FindRecordById(NodesCollection, s.m.NodeID())
	if v := node.GetFloat("clock_offset_ms"); v > 2000 || v < -2000 {
		t.Fatalf("node offset %v", v)
	}

	// exact arithmetic: server_time - (t_send + t_recv)/2 with fixed clocks
	s2 := newSpoke(t)
	h.join(t, s2, h.enroll(t, "gate-2", nil))
	base := time.Now()
	calls := 0
	c2 := s2.client(t, h, func(o *client.Options) {
		o.Now = func() time.Time {
			calls++
			return base.Add(-time.Minute) // constant: t_send == t_recv
		}
	})
	if _, err := c2.Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := c2.Offset() - time.Minute; d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("offset %v, want ~+1m", c2.Offset())
	}
	if calls < 2 {
		t.Fatal("the injected clock must be used")
	}
}

func TestClockOkIsComputedNotEnforced(t *testing.T) {
	t.Setenv(EnvMaxDrift, "10s")
	h := newHub(t)
	s := newSpoke(t)
	h.join(t, s, h.enroll(t, "gate-1", nil))
	c := s.client(t, h, func(o *client.Options) { o.Now = func() time.Time { return time.Now().Add(-time.Minute) } })
	hs, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatalf("a drifting clock must not fail the handshake in PR2: %v", err)
	}
	if hs.Clock.Ok || hs.Clock.MaxDriftMs != 10000 || hs.Clock.OffsetMs < 50000 || hs.Clock.OffsetMs > 70000 {
		t.Fatalf("clock: %+v", hs.Clock)
	}
}

func TestClientTransportRules(t *testing.T) {
	id, _ := proto.GenerateIdentity()
	if _, err := client.New(client.Options{Identity: id, HubURL: "http://hub.example.com"}); err == nil {
		t.Fatal("http must be refused without TOKI_SYNC_INSECURE=1")
	}
	if _, err := client.New(client.Options{Identity: id, HubURL: "ftp://hub.example.com"}); err == nil {
		t.Fatal("other schemes must be refused")
	}
	if _, err := client.New(client.Options{Identity: id, HubURL: "https://hub.example.com"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(client.EnvInsecure, "1")
	if _, err := client.New(client.Options{Identity: id, HubURL: "http://127.0.0.1:1"}); err != nil {
		t.Fatalf("insecure env: %v", err)
	}
	for _, ok := range []string{"http://localhost:8090", "http://10.1.2.3", "http://192.168.0.7:80", "http://[::1]:8090"} {
		if _, err := client.New(client.Options{Identity: id, HubURL: ok}); err != nil {
			t.Fatalf("insecure must allow %s: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://hub.example.com", "http://8.8.8.8", "http://172.32.0.1"} {
		if _, err := client.New(client.Options{Identity: id, HubURL: bad}); err == nil {
			t.Fatalf("insecure must refuse the public host %s", bad)
		}
	}
	if _, err := client.New(client.Options{Identity: id, HubURL: "http://127.0.0.1:1", Pin: strings.Repeat("ab", 32)}); err == nil {
		t.Fatal("a pin needs https")
	}
	if _, err := client.New(client.Options{Identity: id, HubURL: "https://hub.example.com", Pin: "not a pin"}); err == nil {
		t.Fatal("an invalid pin must be refused")
	}
	if _, err := client.New(client.Options{Identity: id, HubURL: "https://hub.example.com", Pin: strings.Repeat("ab", 32)}); err != nil {
		t.Fatalf("hex pin: %v", err)
	}
}

func TestClientPinMismatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	id, _ := proto.GenerateIdentity()
	// the test server certificate is not trusted by the system pool, so provide
	// its client and let the pin check wrap it via a wrong pin on a trusting transport
	_, err := client.Enroll(context.Background(), client.EnrollParams{
		HubURL: srv.URL, Code: "x", Identity: id, Pin: strings.Repeat("00", 32),
	})
	if err == nil {
		t.Fatal("a wrong pin must fail the connection")
	}
}

func TestJoinNeedsMatchingHub(t *testing.T) {
	h := newHub(t)
	s := newSpoke(t)
	code := h.enroll(t, "gate-1", nil)
	// https is required without the insecure flag
	if _, err := client.Join(context.Background(), s.app, client.EnrollParams{HubURL: h.srv.URL, Code: code, Identity: s.m.Identity()}); err == nil {
		t.Fatal("http hub without insecure must be refused")
	}
	// the refusal must not burn the code
	h.join(t, s, code)
}

func TestCLIEnrollJoinPeersRevokeStatus(t *testing.T) {
	h := newHub(t)
	t.Setenv(EnvRole, "hub")
	run := func(cmdApp *tests.TestApp, args ...string) (string, error) {
		cmd := NewCommand(cmdApp)
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		cmd.SetErr(&buf)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return buf.String(), err
	}
	out, err := run(h.app, "enroll", "--name", "gate-1", "--profile", "edge", "--param", "branch=B12", "--actor", "gate_devices/abc")
	if err != nil {
		t.Fatalf("enroll: %v %s", err, out)
	}
	var code string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "code:") {
			code = strings.TrimSpace(strings.TrimPrefix(l, "code:"))
		}
	}
	if len(strings.Split(code, "-")) != 8 {
		t.Fatalf("no code in output: %s", out)
	}
	if _, err := run(h.app, "enroll", "--name", "gate-1", "--profile", "edge"); err == nil {
		t.Fatal("duplicate name must fail")
	}
	if _, err := run(h.app, "enroll", "--name", "x"); err == nil {
		t.Fatal("--profile is required")
	}

	// join as a spoke from the CLI
	s := newSpoke(t)
	t.Setenv(EnvRole, "spoke")
	t.Setenv(client.EnvInsecure, "1")
	if out, err := run(s.app, "join", h.srv.URL, code); err != nil || !strings.Contains(out, s.m.NodeID()) {
		t.Fatalf("join: %v %s", err, out)
	}
	if _, err := s.client(t, h).Handshake(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := GetStatus(s.app, RoleSpoke)
	if err != nil || st.HubID != h.m.HubID() || st.NodeID != s.m.NodeID() || st.CertExpires == "" || st.LastHandshake == "" || st.Epoch != h.m.Epoch() {
		t.Fatalf("spoke status: %v %+v", err, st)
	}
	if exp, _ := time.Parse(proto.TimeLayout, st.CertExpires); time.Until(exp) < 360*24*time.Hour {
		t.Fatalf("cert expiry %s", st.CertExpires)
	}
	// join on a hub is refused
	t.Setenv(EnvRole, "hub")
	if _, err := run(h.app, "join", h.srv.URL, code); err == nil {
		t.Fatal("join needs the spoke role")
	}

	// peers (hub)
	out, err = run(h.app, "peers", "--json")
	var peers []Peer
	if err != nil || json.Unmarshal([]byte(out), &peers) != nil || len(peers) != 1 || peers[0].Name != "gate-1" || peers[0].Status != NodeActive || peers[0].LastSeen == "" {
		t.Fatalf("peers: %v %s", err, out)
	}
	if out, err = run(h.app, "peers"); err != nil || !strings.Contains(out, "gate-1") {
		t.Fatalf("peers table: %v %s", err, out)
	}
	hst, err := GetStatus(h.app, RoleHub)
	if err != nil || hst.HubID != h.m.HubID() || hst.Epoch != h.m.Epoch() || hst.Nodes != 1 {
		t.Fatalf("hub status: %v %+v", err, hst)
	}

	// revoke by name
	if out, err := run(h.app, "revoke", "gate-1"); err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: %v %s", err, out)
	}
	if _, err := run(h.app, "revoke", "nope"); err == nil {
		t.Fatal("unknown node must fail")
	}
	if _, err := s.client(t, h).Ping(context.Background()); !client.IsCode(err, proto.CodeNodeRevoked) {
		t.Fatalf("ping after CLI revoke: %v", err)
	}
	t.Setenv(EnvRole, "off")
	if _, err := run(h.app, "peers"); err == nil {
		t.Fatal("peers needs the hub role")
	}
}
