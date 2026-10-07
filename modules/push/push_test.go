//go:build !no_push

package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

// stubQueue is an in-memory kernel.JobQueue.
type stubQueue struct {
	mu       sync.Mutex
	handlers map[string]kernel.JobHandler
	jobs     []*kernel.Job
}

func (q *stubQueue) Enqueue(_ context.Context, kind string, payload any, _ ...kernel.EnqueueOption) (string, error) {
	b, _ := json.Marshal(payload)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.jobs = append(q.jobs, &kernel.Job{ID: kind + "-" + string(rune('a'+len(q.jobs))), Kind: kind, Payload: b, Attempt: 1})
	return q.jobs[len(q.jobs)-1].ID, nil
}
func (q *stubQueue) Register(kind string, h kernel.JobHandler) {
	q.mu.Lock()
	q.handlers[kind] = h
	q.mu.Unlock()
}
func (q *stubQueue) Stats(context.Context) (kernel.JobStats, error) { return kernel.JobStats{}, nil }

// run executes every queued job, returning the first handler error.
func (q *stubQueue) run(app kernel.App) []error {
	q.mu.Lock()
	jobs := q.jobs
	q.jobs = nil
	q.mu.Unlock()
	var errs []error
	for _, j := range jobs {
		errs = append(errs, q.handlers[j.Kind](context.Background(), app, j))
	}
	return errs
}

type env struct {
	app   *tests.TestApp
	m     *Module
	q     *stubQueue
	fcm   *FakeProvider
	apns  *FakeProvider
	h     http.Handler
	users *core.Collection
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	q := &stubQueue{handlers: map[string]kernel.JobHandler{}}
	kernel.SetJobs(app, q)
	m := Register(app)
	e := &env{app: app, m: m, q: q, fcm: NewFake("fcm"), apns: NewFake("apns")}
	m.SetProvider(PlatformFCM, e.fcm)
	m.SetProvider(PlatformAPNs, e.apns)
	if err := ensureCollections(app); err != nil {
		t.Fatal(err)
	}
	e.users = core.NewAuthCollection("members")
	if err := app.Save(e.users); err != nil {
		t.Fatal(err)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		e.h = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) user(t *testing.T, email string) (*core.Record, string) {
	t.Helper()
	r := core.NewRecord(e.users)
	r.SetEmail(email)
	r.SetPassword("1234567890")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	tok, err := r.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return r, tok
}

func (e *env) superuser(t *testing.T) string {
	t.Helper()
	su := core.NewRecord(mustCol(t, e.app, core.CollectionNameSuperusers))
	su.SetEmail("root@example.com")
	su.SetPassword("1234567890")
	if err := e.app.Save(su); err != nil {
		t.Fatal(err)
	}
	tok, _ := su.NewAuthToken()
	return tok
}

func mustCol(t *testing.T, app core.App, n string) *core.Collection {
	c, err := app.FindCachedCollectionByNameOrId(n)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (e *env) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestDeviceOwnershipAndSubscribe(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	_, tb := e.user(t, "b@example.com")
	if err := CreateTopicVisibility(e.app, "news", "", VisibilityPublic); err != nil {
		t.Fatal(err)
	}

	if rec := e.do("POST", "/api/push/devices", "", `{"token":"tok1","platform":"fcm"}`); rec.Code != 401 {
		t.Fatalf("anon register: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/devices", ta, `{"token":"tok1","platform":"fcm","app_id":"x","locale":"id"}`); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	// upsert is idempotent
	if rec := e.do("POST", "/api/push/devices", ta, `{"token":"tok1","platform":"fcm"}`); rec.Code != 200 {
		t.Fatalf("upsert: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/devices", ta, `{"token":"x","platform":"sms"}`); rec.Code != 400 {
		t.Fatalf("bad platform: %d", rec.Code)
	}
	devs, _ := ListDevices(e.app, "members", a.Id)
	if len(devs) != 1 {
		t.Fatalf("devices: %d", len(devs))
	}

	// user B cannot touch A's device
	if rec := e.do("DELETE", "/api/push/devices/tok1", tb, ""); rec.Code != 404 {
		t.Fatalf("B delete: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/subscribe", tb, `{"token":"tok1","topic":"news"}`); rec.Code != 404 {
		t.Fatalf("B subscribe: %d", rec.Code)
	}

	if rec := e.do("POST", "/api/push/subscribe", ta, `{"token":"tok1","topic":"news"}`); rec.Code != 200 {
		t.Fatalf("subscribe: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/push/subscribe", ta, `{"token":"tok1","topic":"news"}`); rec.Code != 200 {
		t.Fatalf("subscribe twice: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/subscribe", ta, `{"token":"tok1","topic":"nope"}`); rec.Code != 404 {
		t.Fatalf("unknown topic: %d", rec.Code)
	}
	subs, _ := e.app.FindAllRecords(SubscriptionsCollection)
	if len(subs) != 1 {
		t.Fatalf("subs: %d", len(subs))
	}
	if rec := e.do("GET", "/api/push/topics", ta, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"news"`) {
		t.Fatalf("topics: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/push/unsubscribe", ta, `{"token":"tok1","topic":"news"}`); rec.Code != 200 {
		t.Fatalf("unsubscribe: %d", rec.Code)
	}
	if subs, _ := e.app.FindAllRecords(SubscriptionsCollection); len(subs) != 0 {
		t.Fatalf("subs after unsubscribe: %d", len(subs))
	}

	// B registering the same token takes it over (shared phone), A loses it
	if rec := e.do("POST", "/api/push/devices", tb, `{"token":"tok1","platform":"fcm"}`); rec.Code != 200 {
		t.Fatalf("takeover: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/push/devices/tok1", ta, ""); rec.Code != 404 {
		t.Fatalf("A delete after takeover: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/push/devices/tok1", tb, ""); rec.Code != 204 {
		t.Fatalf("B delete own: %d", rec.Code)
	}
}

func TestSendTopicEnqueuesAndHandlerCallsProvider(t *testing.T) {
	e := setup(t)
	_ = CreateTopicVisibility(e.app, "news", "", VisibilityPublic)
	su := e.superuser(t)
	_, ta := e.user(t, "a@example.com")
	_, tb := e.user(t, "b@example.com")
	for _, c := range []struct{ tok, body string }{
		{ta, `{"token":"fcm-1","platform":"fcm"}`},
		{tb, `{"token":"apns-1","platform":"apns"}`},
		{tb, `{"token":"fcm-2","platform":"fcm"}`},
	} {
		if rec := e.do("POST", "/api/push/devices", c.tok, c.body); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}
	for _, c := range []struct{ tok, body string }{
		{ta, `{"token":"fcm-1","topic":"news"}`},
		{tb, `{"token":"apns-1","topic":"news"}`},
	} {
		if rec := e.do("POST", "/api/push/subscribe", c.tok, c.body); rec.Code != 200 {
			t.Fatal(rec.Body)
		}
	}

	var audited []map[string]any
	SetAuditSink(func(action, _, _ string, d map[string]any) {
		if action == "push.send" {
			audited = append(audited, d)
		}
	})
	t.Cleanup(func() { SetAuditSink(nil) })

	if rec := e.do("POST", "/api/push/send", ta, `{"to":{"topics":["news"]},"title":"t"}`); rec.Code != 403 {
		t.Fatalf("user send: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/send", su, `{"to":{},"title":"t"}`); rec.Code != 400 {
		t.Fatalf("empty to: %d", rec.Code)
	}
	rec := e.do("POST", "/api/push/send", su,
		`{"to":{"topics":["news"],"tokens":["fcm-1","fcm-2"]},"title":"Hi","body":"B","data":{"n":1,"s":"x"},"ttl_seconds":60,"collapse_key":"c","priority":"normal"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"queued":3`) {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if len(e.q.jobs) != 3 {
		t.Fatalf("jobs: %d", len(e.q.jobs))
	}
	if len(audited) != 1 || audited[0]["queued"] != 3 {
		t.Fatalf("audit: %v", audited)
	}
	if _, has := audited[0]["tokens_list"]; has || strings.Contains(toJSON(audited), "fcm-1") {
		t.Fatalf("audit leaks tokens: %v", audited)
	}
	for _, err := range e.q.run(e.app) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.fcm.Sent()); got != 2 {
		t.Fatalf("fcm sends: %d", got)
	}
	if got := len(e.apns.Sent()); got != 1 {
		t.Fatalf("apns sends: %d", got)
	}
	s := e.fcm.Sent()[0].N
	if s.Title != "Hi" || s.Data["n"] != "1" || s.Data["s"] != "x" || s.TTLSeconds != 60 || s.CollapseKey != "c" || s.Priority != "normal" {
		t.Fatalf("payload: %+v", s)
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestInvalidTokenDisablesDevice(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.do("POST", "/api/push/devices", ta, `{"token":"dead","platform":"fcm"}`)
	e.fcm.SetError("dead", errors.Join(ErrInvalidToken, errors.New("UNREGISTERED")))
	var actions []string
	SetAuditSink(func(action, _, _ string, _ map[string]any) { actions = append(actions, action) })
	t.Cleanup(func() { SetAuditSink(nil) })

	if n, err := SendToUser(e.app, "members", a.Id, Notification{Title: "x"}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if errs := e.q.run(e.app); errs[0] != nil {
		t.Fatalf("invalid token must not retry: %v", errs[0])
	}
	devs, _ := ListDevices(e.app, "members", a.Id)
	if len(devs) != 1 || devs[0].Enabled {
		t.Fatalf("device not disabled: %+v", devs)
	}
	if len(actions) != 2 || actions[1] != "push.device.disabled" {
		t.Fatalf("audit: %v", actions)
	}
	// disabled devices are no longer targeted
	if n, _ := SendToUser(e.app, "members", a.Id, Notification{Title: "x"}); n != 0 {
		t.Fatalf("disabled device still targeted: %d", n)
	}
}

func TestRetryableAndPermanentErrors(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.do("POST", "/api/push/devices", ta, `{"token":"t1","platform":"fcm"}`)
	e.fcm.SetError("t1", &RetryableError{Err: errors.New("HTTP 429")})
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	errs := e.q.run(e.app)
	var re *RetryableError
	if !errors.As(errs[0], &re) {
		t.Fatalf("want retryable error for the queue, got %v", errs[0])
	}
	e.fcm.SetError("t1", &PermanentError{Err: errors.New("INVALID_ARGUMENT")})
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	if errs := e.q.run(e.app); errs[0] != nil {
		t.Fatalf("permanent rejection must not retry: %v", errs[0])
	}
	devs, _ := ListDevices(e.app, "members", a.Id)
	if !devs[0].Enabled {
		t.Fatal("permanent rejection must not disable the device")
	}
	// missing provider config -> error (retry, visible in the queue)
	e.m.SetProvider(PlatformFCM, nil)
	t.Setenv("TOKI_PUSH_FCM_SA_FILE", "")
	t.Setenv("TOKI_PUSH_FCM_SA_B64", "")
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	if errs := e.q.run(e.app); errs[0] == nil {
		t.Fatal("unconfigured provider must return an error")
	}
}

func TestDeleteAuthRecordRemovesDevices(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.do("POST", "/api/push/devices", ta, `{"token":"t1","platform":"fcm"}`)
	if err := e.app.Delete(a); err != nil {
		t.Fatal(err)
	}
	if devs, _ := ListDevices(e.app, "", ""); len(devs) != 0 {
		t.Fatalf("devices left: %d", len(devs))
	}
}

func TestPrune(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.do("POST", "/api/push/devices", ta, `{"token":"old","platform":"fcm"}`)
	e.do("POST", "/api/push/devices", ta, `{"token":"new","platform":"fcm"}`)
	r, _ := e.app.FindFirstRecordByData(DevicesCollection, "token", "old")
	r.Set("last_seen", time.Now().Add(-100*24*time.Hour).UTC().Format("2006-01-02 15:04:05.000Z"))
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	n, err := PruneDevices(e.app, 90*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if devs, _ := ListDevices(e.app, "members", a.Id); len(devs) != 1 || devs[0].Token != "new" {
		t.Fatalf("%+v", devs)
	}
}

// ---- providers ----

func testRSAServiceAccount(t *testing.T, tokenURI string) ([]byte, *rsa.PublicKey) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa, _ := json.Marshal(map[string]string{
		"project_id": "demo-proj", "client_email": "svc@demo-proj.iam.gserviceaccount.com",
		"private_key": string(pemKey), "token_uri": tokenURI,
	})
	return sa, &k.PublicKey
}

func TestFCMOAuthExchangeAndSend(t *testing.T) {
	var tokenCalls, sendCalls int
	var pub *rsa.PublicKey
	var sendStatus = 200
	var sendBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant_type: %q", r.Form.Get("grant_type"))
		}
		parts := strings.Split(r.Form.Get("assertion"), ".")
		if len(parts) != 3 {
			t.Fatalf("assertion: %q", r.Form.Get("assertion"))
		}
		var claims map[string]any
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(raw, &claims)
		if claims["scope"] != fcmScope || claims["iss"] != "svc@demo-proj.iam.gserviceaccount.com" || claims["aud"] == "" {
			t.Errorf("claims: %v", claims)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := verifyRS256(pub, parts[0]+"."+parts[1], sig); err != nil {
			t.Errorf("bad RS256 signature: %v", err)
		}
		_, _ = w.Write([]byte(`{"access_token":"at-1","expires_in":3600}`))
	})
	mux.HandleFunc("/v1/projects/demo-proj/messages:send", func(w http.ResponseWriter, r *http.Request) {
		sendCalls++
		if r.Header.Get("Authorization") != "Bearer at-1" {
			t.Errorf("auth: %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		sendBody = string(b)
		w.WriteHeader(sendStatus)
		if sendStatus == 404 {
			_, _ = w.Write([]byte(`{"error":{"status":"NOT_FOUND","message":"x","details":[{"errorCode":"UNREGISTERED"}]}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	saJSON, pk := testRSAServiceAccount(t, srv.URL+"/token")
	pub = pk
	f, err := NewFCM(saJSON)
	if err != nil {
		t.Fatal(err)
	}
	f.BaseURL = srv.URL
	now := time.Unix(1_800_000_000, 0)
	f.Now = func() time.Time { return now }

	n := &Notification{Title: "T", Body: "B", Data: map[string]string{"k": "v"}, TTLSeconds: 120, CollapseKey: "ck", Priority: PriorityNormal}
	if err := f.Send(context.Background(), n, "device-tok"); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Message struct {
			Token        string            `json:"token"`
			Notification map[string]string `json:"notification"`
			Data         map[string]string `json:"data"`
			Android      map[string]string `json:"android"`
		} `json:"message"`
	}
	_ = json.Unmarshal([]byte(sendBody), &m)
	if m.Message.Token != "device-tok" || m.Message.Notification["title"] != "T" || m.Message.Data["k"] != "v" ||
		m.Message.Android["ttl"] != "120s" || m.Message.Android["collapse_key"] != "ck" || m.Message.Android["priority"] != "NORMAL" {
		t.Fatalf("body: %s", sendBody)
	}
	// token is cached
	_ = f.Send(context.Background(), n, "device-tok")
	if tokenCalls != 1 || sendCalls != 2 {
		t.Fatalf("token calls %d send calls %d", tokenCalls, sendCalls)
	}
	// ... until it expires
	now = now.Add(2 * time.Hour)
	_ = f.Send(context.Background(), n, "device-tok")
	if tokenCalls != 2 {
		t.Fatalf("token not refreshed: %d", tokenCalls)
	}
	// error mapping
	sendStatus = 404 // body carries errorCode UNREGISTERED
	if err := f.Send(context.Background(), n, "gone"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("want ErrInvalidToken, got %v", err)
	}
	sendStatus = 429
	var re *RetryableError
	if err := f.Send(context.Background(), n, "x"); !errors.As(err, &re) {
		t.Fatalf("429: %v", err)
	}
	sendStatus = 503
	if err := f.Send(context.Background(), n, "x"); !errors.As(err, &re) {
		t.Fatalf("503: %v", err)
	}
	sendStatus = 400
	var pe *PermanentError
	if err := f.Send(context.Background(), n, "x"); !errors.As(err, &pe) {
		t.Fatalf("400: %v", err)
	}
	sendStatus = 401
	if err := f.Send(context.Background(), n, "x"); !errors.As(err, &re) {
		t.Fatalf("401: %v", err)
	}
	if f.token != "" {
		t.Fatal("401 must drop the cached token")
	}
}

func verifyRS256(pub *rsa.PublicKey, signing string, sig []byte) error {
	return verifyPKCS1(pub, signing, sig)
}

func TestAPNsRequestShapeAndClassification(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	a, err := NewAPNs(APNsConfig{KeyPEM: pemKey, KeyID: "KEY123", TeamID: "TEAM456", Topic: "com.example.app", Sandbox: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.BaseURL != "https://api.sandbox.push.apple.com" {
		t.Fatalf("sandbox url: %s", a.BaseURL)
	}
	now := time.Unix(1_800_000_000, 0)
	a.Now = func() time.Time { return now }
	n := &Notification{Title: "T", Body: "B", Data: map[string]string{"screen": "inbox", "aps": "evil"}, TTLSeconds: 60, CollapseKey: "ck", Priority: PriorityNormal}
	req, err := a.BuildRequest(context.Background(), n, "abcdef0123")
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != "POST" || req.URL.Path != "/3/device/abcdef0123" {
		t.Fatalf("%s %s", req.Method, req.URL)
	}
	h := req.Header
	if h.Get("apns-topic") != "com.example.app" || h.Get("apns-push-type") != "alert" || h.Get("apns-priority") != "5" ||
		h.Get("apns-expiration") != "1800000060" || h.Get("apns-collapse-id") != "ck" {
		t.Fatalf("headers: %v", h)
	}
	auth := h.Get("Authorization")
	if !strings.HasPrefix(auth, "bearer ") {
		t.Fatalf("auth: %q", auth)
	}
	jwt := strings.TrimPrefix(auth, "bearer ")
	parts := strings.Split(jwt, ".")
	var hdr, claims map[string]any
	r0, _ := base64.RawURLEncoding.DecodeString(parts[0])
	r1, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(r0, &hdr)
	_ = json.Unmarshal(r1, &claims)
	if hdr["alg"] != "ES256" || hdr["kid"] != "KEY123" || claims["iss"] != "TEAM456" || claims["iat"] != float64(now.Unix()) {
		t.Fatalf("jwt: %v %v", hdr, claims)
	}
	if !verifyES256(&k.PublicKey, jwt) {
		t.Fatal("bad ES256 signature")
	}
	body, _ := io.ReadAll(req.Body)
	var p map[string]any
	_ = json.Unmarshal(body, &p)
	aps, _ := p["aps"].(map[string]any)
	alert, _ := aps["alert"].(map[string]any)
	if p["screen"] != "inbox" || alert["title"] != "T" || alert["body"] != "B" {
		t.Fatalf("payload: %s", body)
	}
	// the JWT is cached for 50 minutes, then renewed
	req2, _ := a.BuildRequest(context.Background(), n, "x")
	if req2.Header.Get("Authorization") != auth {
		t.Fatal("jwt should be cached")
	}
	now = now.Add(51 * time.Minute)
	req3, _ := a.BuildRequest(context.Background(), n, "x")
	if req3.Header.Get("Authorization") == auth {
		t.Fatal("jwt should be renewed after 50 minutes")
	}

	var re *RetryableError
	var pe *PermanentError
	if err := classifyAPNs(a, 410, []byte(`{"reason":"Unregistered"}`)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("410: %v", err)
	}
	if err := classifyAPNs(a, 400, []byte(`{"reason":"BadDeviceToken"}`)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("BadDeviceToken: %v", err)
	}
	if err := classifyAPNs(a, 429, []byte(`{"reason":"TooManyRequests"}`)); !errors.As(err, &re) {
		t.Fatalf("429: %v", err)
	}
	if err := classifyAPNs(a, 503, nil); !errors.As(err, &re) {
		t.Fatalf("503: %v", err)
	}
	if err := classifyAPNs(a, 400, []byte(`{"reason":"PayloadEmpty"}`)); !errors.As(err, &pe) {
		t.Fatalf("400: %v", err)
	}
	now = now.Add(21 * time.Minute) // Apple rate limits provider token refreshes (< 20 min)
	if err := classifyAPNs(a, 403, []byte(`{"reason":"ExpiredProviderToken"}`)); !errors.As(err, &re) || a.jwt != "" {
		t.Fatalf("403: %v jwt=%q", err, a.jwt)
	}
	if classifyAPNs(a, 200, nil) != nil {
		t.Fatal("200 must be nil")
	}
}

func TestAPNsSendAgainstHTTPTestServer(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	a, _ := NewAPNs(APNsConfig{KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), KeyID: "k", TeamID: "t", Topic: "b"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(410)
		_, _ = w.Write([]byte(`{"reason":"Unregistered"}`))
	}))
	defer srv.Close()
	a.BaseURL = srv.URL
	if err := a.Send(context.Background(), &Notification{Title: "x"}, "tok"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("%v", err)
	}
}

func TestEnvConfigLoading(t *testing.T) {
	saJSON, _ := testRSAServiceAccount(t, "https://example.invalid/token")
	t.Setenv("TOKI_PUSH_FCM_SA_B64", base64.StdEncoding.EncodeToString(saJSON))
	f, err := loadFCMFromEnv()
	if err != nil || f == nil {
		t.Fatal(f, err)
	}
	t.Setenv("TOKI_PUSH_FCM_SA_B64", "%%%")
	if _, err := loadFCMFromEnv(); err == nil {
		t.Fatal("expected error for bad base64")
	}
	t.Setenv("TOKI_PUSH_APNS_KEY_FILE", "")
	if a, err := loadAPNsFromEnv(); a != nil || err != nil {
		t.Fatal("unset APNs config must be nil, nil")
	}
}
