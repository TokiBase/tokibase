package webhooks

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

type hit struct {
	header http.Header
	body   []byte
}

type receiver struct {
	srv    *httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	hits   []hit
}

func newReceiver(t *testing.T, status int) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(int32(status))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, hit{req.Header.Clone(), b})
		r.mu.Unlock()
		w.WriteHeader(int(r.status.Load()))
		_, _ = w.Write([]byte("receiver says no"))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func (r *receiver) last() hit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hits[len(r.hits)-1]
}

func setup(t *testing.T) (*tests.TestApp, *Module) {
	t.Helper()
	t.Setenv("TOKI_WEBHOOK_ALLOW_PRIVATE", "1")
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)

	oldPoll := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = oldPoll })
	t.Cleanup(SetBackoff(func(int) time.Duration { return 10 * time.Millisecond }))
	t.Cleanup(m.Stop)
	return app, m
}

func makeCollection(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c := core.NewBaseCollection(name)
	c.Fields.Add(
		&core.TextField{Name: "title"},
		&core.TextField{Name: "priv", Hidden: true},
	)
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func newRecord(t *testing.T, app core.App, c *core.Collection, title string) *core.Record {
	t.Helper()
	r := core.NewRecord(c)
	r.Set("title", title)
	r.Set("priv", "hidden-value")
	if err := app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func addHook(t *testing.T, app core.App, url, events, collections string) *Webhook {
	t.Helper()
	w, err := Add(app, Webhook{Name: "wh" + strconv.Itoa(int(time.Now().UnixNano()%1e9)), URL: url, Secret: "s3cret-s3cret-s3cret",
		Events: normalizeEvents(events), Collections: normalizeEvents(collections)})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func deliveries(t *testing.T, app core.App, state string) []Delivery {
	t.Helper()
	rows, err := ListDeliveries(app, state, 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRecordCreateDeliveredWithValidSignature(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.*", "")
	c := makeCollection(t, app, "items")
	m.Start()

	rec := newRecord(t, app, c, "hello")
	waitFor(t, "delivery", func() bool { return rcv.count() >= 1 })

	h := rcv.last()
	ts, _ := strconv.ParseInt(h.header.Get("X-Toki-Timestamp"), 10, 64)
	if want := Sign("s3cret-s3cret-s3cret", ts, h.body); h.header.Get("X-Toki-Signature") != want {
		t.Fatalf("bad signature %q want %q", h.header.Get("X-Toki-Signature"), want)
	}
	if !strings.HasPrefix(h.header.Get("X-Toki-Signature"), "v1=") {
		t.Fatal("signature must start with v1=")
	}
	if h.header.Get("X-Toki-Event") != "record.create" || h.header.Get("X-Toki-Delivery") == "" ||
		h.header.Get("User-Agent") != "TokiBase-Webhooks/1" || h.header.Get("Content-Type") != "application/json" {
		t.Fatalf("bad headers: %v", h.header)
	}
	var p Payload
	if err := json.Unmarshal(h.body, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != "record.create" || p.Collection != "items" || p.RecordID != rec.Id || p.ID == "" || p.Created == "" {
		t.Fatalf("bad payload: %s", h.body)
	}
	data := p.Data.(map[string]any)
	if data["title"] != "hello" {
		t.Fatalf("data: %v", data)
	}
	if _, ok := data["priv"]; ok || strings.Contains(string(h.body), "hidden-value") {
		t.Fatalf("hidden field leaked: %s", h.body)
	}
	waitFor(t, "delivered state", func() bool { return len(deliveries(t, app, StateDelivered)) == 1 })
}

func TestRecordUpdateChangedAndDelete(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.update,record.delete", "")
	c := makeCollection(t, app, "items")
	m.Start()

	rec := newRecord(t, app, c, "v1")
	rec, err := app.FindRecordById("items", rec.Id) // like the REST API: load, then update
	if err != nil {
		t.Fatal(err)
	}
	rec.Set("title", "v2")
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "update delivery", func() bool { return rcv.count() >= 1 })
	var p Payload
	_ = json.Unmarshal(rcv.last().body, &p)
	if p.Event != "record.update" || len(p.Changed) == 0 || p.Old["title"] != "v1" {
		t.Fatalf("bad update payload: %s", rcv.last().body)
	}
	if err := app.Delete(rec); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delete delivery", func() bool { return rcv.count() >= 2 })
	if rcv.last().header.Get("X-Toki-Event") != "record.delete" {
		t.Fatal("expected record.delete")
	}
}

func TestSystemCollectionsAndConfigNotCaptured(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "*", "")
	m.Start()
	// adding another webhook is a _webhooks record write: must not be captured
	addHook(t, app, rcv.srv.URL, "record.create", "")
	time.Sleep(150 * time.Millisecond)
	if rows := deliveries(t, app, ""); len(rows) != 0 {
		t.Fatalf("expected no deliveries, got %d", len(rows))
	}
}

func TestRetryThenDead(t *testing.T) {
	app, m := setup(t)
	var sunk []map[string]any
	var smu sync.Mutex
	SetAuditSink(func(action, collection, record string, details map[string]any) {
		if action != AuditDead {
			t.Errorf("action %q", action)
		}
		smu.Lock()
		sunk = append(sunk, details)
		smu.Unlock()
	})
	t.Cleanup(func() { SetAuditSink(nil) })

	rcv := newReceiver(t, 500)
	w := addHook(t, app, rcv.srv.URL, "record.create", "")
	rec, _ := app.FindRecordById(ConfigCollection, w.ID)
	rec.Set("max_attempts", 3)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	m.invalidate()
	c := makeCollection(t, app, "items")
	m.Start()
	newRecord(t, app, c, "x")

	waitFor(t, "dead", func() bool { return len(deliveries(t, app, StateDead)) == 1 })
	d := deliveries(t, app, StateDead)[0]
	if d.Attempt != 3 || d.LastStatus != 500 || !strings.Contains(d.LastError, "receiver says no") {
		t.Fatalf("bad dead row: %+v", d)
	}
	if rcv.count() != 3 {
		t.Fatalf("expected 3 attempts, got %d", rcv.count())
	}
	smu.Lock()
	defer smu.Unlock()
	if len(sunk) != 1 || sunk[0]["attempts"] != 3 {
		t.Fatalf("audit sink: %v", sunk)
	}
}

func TestReplayRedelivers(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 500)
	w := addHook(t, app, rcv.srv.URL, "record.create", "")
	rec, _ := app.FindRecordById(ConfigCollection, w.ID)
	rec.Set("max_attempts", 2)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	m.invalidate()
	c := makeCollection(t, app, "items")
	m.Start()
	newRecord(t, app, c, "x")
	waitFor(t, "dead", func() bool { return len(deliveries(t, app, StateDead)) == 1 })

	rcv.status.Store(200)
	n, err := Replay(app, "", false)
	if err != nil || n != 1 {
		t.Fatalf("replay: %d %v", n, err)
	}
	waitFor(t, "delivered", func() bool { return len(deliveries(t, app, StateDelivered)) == 1 })
	if d := deliveries(t, app, StateDelivered)[0]; d.Attempt != 1 {
		t.Fatalf("attempt should restart: %+v", d)
	}
	if _, err := Replay(app, "nope", false); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestCollectionsFilter(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.create", "only")
	only := makeCollection(t, app, "only")
	other := makeCollection(t, app, "other")
	m.Start()

	newRecord(t, app, other, "skip")
	newRecord(t, app, only, "take")
	waitFor(t, "delivery", func() bool { return rcv.count() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", rcv.count())
	}
	var p Payload
	_ = json.Unmarshal(rcv.last().body, &p)
	if p.Collection != "only" {
		t.Fatalf("wrong collection %q", p.Collection)
	}
}

func TestCollectionEvents(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "collection.*", "")
	m.Start()
	makeCollection(t, app, "things")
	waitFor(t, "delivery", func() bool { return rcv.count() >= 1 })
	if rcv.last().header.Get("X-Toki-Event") != "collection.create" {
		t.Fatalf("event %q", rcv.last().header.Get("X-Toki-Event"))
	}
}

func TestSSRFGuard(t *testing.T) {
	app, _ := setup(t)
	rcv := newReceiver(t, 200)
	w := addHook(t, app, rcv.srv.URL, "*", "")

	t.Setenv("TOKI_WEBHOOK_ALLOW_PRIVATE", "")
	d, err := Ping(t.Context(), app, w)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateDead || !strings.Contains(d.LastError, "private") || rcv.count() != 0 {
		t.Fatalf("expected block, got %+v (hits=%d)", d, rcv.count())
	}

	t.Setenv("TOKI_WEBHOOK_ALLOW_PRIVATE", "1")
	d, err = Ping(t.Context(), app, w)
	if err != nil || d.State != StateDelivered || d.LastStatus != 200 {
		t.Fatalf("expected delivery with allow-private: %+v %v", d, err)
	}
}

func TestBlockedIPs(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "192.168.0.1": true, "172.16.5.5": true,
		"169.254.169.254": true, "100.64.0.1": true, "0.0.0.0": true, "::ffff:127.0.0.1": true, "fe80::1": true,
		"8.8.8.8": false, "1.1.1.1": false, "2606:4700::1111": false,
	} {
		if got := blockedIP(mustAddr(ip)); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
}

func TestRedirectsNotFollowed(t *testing.T) {
	app, _ := setup(t)
	target := newReceiver(t, 200)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.srv.URL, http.StatusFound)
	}))
	defer redir.Close()
	w := addHook(t, app, redir.URL, "*", "")
	d, err := Ping(t.Context(), app, w)
	if err != nil {
		t.Fatal(err)
	}
	if d.State == StateDelivered || d.LastStatus != 302 || target.count() != 0 {
		t.Fatalf("redirect must not be followed: %+v", d)
	}
}

func TestPingAndErrorCap(t *testing.T) {
	app, _ := setup(t)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(strings.Repeat("x", 100000)))
	}))
	defer big.Close()
	w := addHook(t, app, big.URL, "*", "")
	d, err := Ping(t.Context(), app, w)
	if err != nil {
		t.Fatal(err)
	}
	if d.State != StateDead || len(d.LastError) > maxErrorBytes+64 {
		t.Fatalf("error cap: state=%s len=%d", d.State, len(d.LastError))
	}

	rcv := newReceiver(t, 204)
	w2 := addHook(t, app, rcv.srv.URL, "*", "")
	d, err = Ping(t.Context(), app, w2)
	if err != nil || d.State != StateDelivered || d.LastStatus != 204 {
		t.Fatalf("ping: %+v %v", d, err)
	}
	if rcv.last().header.Get("X-Toki-Event") != "ping" {
		t.Fatal("expected ping event")
	}
}

func TestSecretHiddenFromAPIExport(t *testing.T) {
	app, _ := setup(t)
	w := addHook(t, app, "http://example.com/x", "*", "")
	rec, err := app.FindRecordById(ConfigCollection, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.PublicExport()["secret"]; ok {
		t.Fatal("secret must be hidden from API exports")
	}
	if rec.GetString("secret") != "s3cret-s3cret-s3cret" {
		t.Fatal("secret must still be readable internally")
	}
	if _, err := Add(app, Webhook{Name: "bad", URL: "ftp://x", Events: []string{"*"}}); err == nil {
		t.Fatal("non-http url must be rejected")
	}
}

func TestBackoffCapped(t *testing.T) {
	for a := 1; a <= 30; a++ {
		d := defaultBackoff(a)
		if d <= 0 || d > time.Hour*12/10 {
			t.Fatalf("attempt %d: %v", a, d)
		}
	}
	if d := defaultBackoff(1); d < 8*time.Second || d > 12*time.Second {
		t.Fatalf("first retry should be ~10s, got %v", d)
	}
}

func mustAddr(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestAuthLoginEvent(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "auth.login", "")
	m.Start()

	col := core.NewAuthCollection("members")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(col)
	user.SetEmail("m@example.com")
	user.SetPassword("1234567890")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		h = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/collections/members/auth-with-password",
		strings.NewReader(`{"identity":"m@example.com","password":"1234567890"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "auth.login delivery", func() bool { return rcv.count() >= 1 })
	var p Payload
	_ = json.Unmarshal(rcv.last().body, &p)
	d := p.Data.(map[string]any)
	if p.Event != "auth.login" || p.RecordID != user.Id || d["method"] != "password" || d["ip"] == "" {
		t.Fatalf("bad payload: %s", rcv.last().body)
	}
	if strings.Contains(string(rcv.last().body), "token") {
		t.Fatalf("token leaked: %s", rcv.last().body)
	}
}

// ---- QC regression tests ----

const testSecret = "s3cret-s3cret-s3cret"

func payloadOf(t *testing.T, h hit) Payload {
	t.Helper()
	var p Payload
	if err := json.Unmarshal(h.body, &p); err != nil {
		t.Fatalf("bad payload %s: %v", h.body, err)
	}
	return p
}

// serveMux builds the app HTTP handler (for login tests).
func serveMux(t *testing.T, app core.App) http.Handler {
	t.Helper()
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		h = mux
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return h
}

func login(t *testing.T, h http.Handler, collection, identity, password string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/collections/"+collection+"/auth-with-password",
		strings.NewReader(`{"identity":"`+identity+`","password":"`+password+`"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login %s failed: %d %s", collection, rec.Code, rec.Body)
	}
}

// W1: a webhook added by another process (no invalidate hook) is picked up
// once the cache is older than cacheTTL, so the event is not dropped.
func TestW1_StaleCacheReloadsFromDB(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	c := makeCollection(t, app, "items")
	if got, _ := m.webhooks(); len(got) != 0 { // warm the cache (empty)
		t.Fatal("expected no webhooks")
	}
	// "other process": write the record without running this process's hooks
	col, _ := app.FindCachedCollectionByNameOrId(ConfigCollection)
	r := core.NewRecord(col)
	r.Id = "externalhook001"
	r.Set("name", "external")
	r.Set("url", rcv.srv.URL)
	r.Set("secret", testSecret)
	r.Set("events", []string{"record.create"})
	r.Set("enabled", true)
	if err := app.UnsafeWithoutHooks().Save(r); err != nil {
		t.Fatal(err)
	}
	newRecord(t, app, c, "fresh cache: event is missed (documented window)")
	if rows := deliveries(t, app, ""); len(rows) != 0 {
		t.Fatalf("cache should still be fresh, got %d deliveries", len(rows))
	}
	m.mu.Lock()
	m.loadedAt = time.Now().Add(-cacheTTL - time.Second)
	m.mu.Unlock()
	newRecord(t, app, c, "stale cache: reloaded")
	if rows := deliveries(t, app, ""); len(rows) != 1 {
		t.Fatalf("expected 1 delivery after stale reload, got %d", len(rows))
	}
}

// W2: old/changed/data never carry the auth email unless emailVisibility.
func TestW2_AuthEmailNotLeakedInOld(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.update,record.create", "members")
	m.Start()
	col := core.NewAuthCollection("members")
	col.Fields.Add(&core.TextField{Name: "nick"})
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(col)
	u.SetEmail("old@example.com")
	u.SetEmailVisibility(false)
	u.SetPassword("1234567890")
	u.Set("nick", "a")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	u, _ = app.FindRecordById("members", u.Id)
	u.SetEmail("new@example.com")
	u.Set("nick", "b")
	u.SetPassword("0987654321")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "2 deliveries", func() bool { return rcv.count() >= 2 })
	body := string(rcv.last().body)
	for _, bad := range []string{"old@example.com", "new@example.com", "password", "tokenKey"} {
		if strings.Contains(body, bad) {
			t.Fatalf("%q leaked: %s", bad, body)
		}
	}
	p := payloadOf(t, rcv.last())
	if len(p.Changed) != 1 || p.Changed[0] != "nick" {
		t.Fatalf("changed = %v", p.Changed)
	}
}

func TestW2_VisibleEmailIsReported(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.update", "members")
	m.Start()
	col := core.NewAuthCollection("members")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(col)
	u.SetEmail("old@example.com")
	u.SetEmailVisibility(true)
	u.SetPassword("1234567890")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	u, _ = app.FindRecordById("members", u.Id)
	u.SetEmail("new@example.com")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "delivery", func() bool { return rcv.count() >= 1 })
	p := payloadOf(t, rcv.last())
	if p.Old["email"] != "old@example.com" {
		t.Fatalf("visible email should be reported: %s", rcv.last().body)
	}
}

// W8: system auth collections (_superusers) never produce auth.login events.
func TestW8_SuperuserLoginNotCaptured(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "auth.*", "")
	m.Start()
	su := core.NewRecord(mustCollection(t, app, core.CollectionNameSuperusers))
	su.SetEmail("root@example.com")
	su.SetPassword("1234567890")
	if err := app.Save(su); err != nil {
		t.Fatal(err)
	}
	col := core.NewAuthCollection("members")
	if err := app.Save(col); err != nil {
		t.Fatal(err)
	}
	u := core.NewRecord(col)
	u.SetEmail("m@example.com")
	u.SetPassword("1234567890")
	if err := app.Save(u); err != nil {
		t.Fatal(err)
	}
	h := serveMux(t, app)
	login(t, h, core.CollectionNameSuperusers, "root@example.com", "1234567890")
	login(t, h, "members", "m@example.com", "1234567890") // barrier
	waitFor(t, "member login", func() bool { return rcv.count() >= 1 })
	time.Sleep(150 * time.Millisecond)
	if rcv.count() != 1 {
		t.Fatalf("expected only the member login, got %d deliveries", rcv.count())
	}
	if p := payloadOf(t, rcv.last()); p.Collection != "members" {
		t.Fatalf("collection = %q", p.Collection)
	}
}

func mustCollection(t *testing.T, app core.App, name string) *core.Collection {
	t.Helper()
	c, err := app.FindCollectionByNameOrId(name)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// W6: secret is required (>= 16 chars), header rules, redacted CLI output.
func TestW6_SecretRequiredAndHeadersValidated(t *testing.T) {
	app, _ := setup(t)
	if _, err := Add(app, Webhook{Name: "short", URL: "https://example.com", Secret: "tooshort", Events: []string{"*"}}); err == nil {
		t.Fatal("short secret must be rejected")
	}
	// admin API path: a record without secret fails validation
	col, _ := app.FindCachedCollectionByNameOrId(ConfigCollection)
	r := core.NewRecord(col)
	r.Set("name", "nosecret")
	r.Set("url", "https://example.com")
	r.Set("events", []string{"*"})
	if err := app.Save(r); err == nil {
		t.Fatal("record without secret must be rejected")
	}
	for _, h := range []map[string]string{{"Host": "evil"}, {"content-length": "1"}, {"X-A": "a\r\nB: c"}} {
		r := core.NewRecord(col)
		r.Set("name", "h"+strconv.Itoa(len(h)))
		r.Set("url", "https://example.com")
		r.Set("secret", testSecret)
		r.Set("events", []string{"*"})
		r.Set("headers", h)
		if err := app.Save(r); err == nil {
			t.Fatalf("headers %v must be rejected", h)
		}
	}
	if _, err := Add(app, Webhook{Name: "gen", URL: "https://example.com", Events: []string{"*"}}); err != nil {
		t.Fatalf("generated secret must pass: %v", err)
	}
}

func TestW6_ListJSONRedactsHeadersAndSecret(t *testing.T) {
	app, _ := setup(t)
	if _, err := Add(app, Webhook{Name: "crm", URL: "https://example.com", Secret: testSecret,
		Events: []string{"*"}, Headers: map[string]string{"Authorization": "Bearer topsecrettoken"}}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	cmd := NewCommand(app)
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "topsecrettoken") || strings.Contains(s, testSecret) || !strings.Contains(s, "Authorization") {
		t.Fatalf("bad output: %s", s)
	}
}

// W4: shutdown mid-delivery does not consume an attempt.
func TestW4_ShutdownDoesNotChargeAttempt(t *testing.T) {
	app, m := setup(t)
	inflight := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inflight <- struct{}{}:
		default:
		}
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the disconnect
		<-r.Context().Done()               // hang until the client gives up
	}))
	t.Cleanup(srv.Close)
	addHook(t, app, srv.URL, "record.create", "")
	c := makeCollection(t, app, "items")
	m.Start()
	newRecord(t, app, c, "x")
	<-inflight
	m.Stop()
	rows := deliveries(t, app, "")
	if len(rows) != 1 || rows[0].Attempt != 0 || rows[0].State != StateQueued || rows[0].LastError != "" {
		t.Fatalf("shutdown charged an attempt: %+v", rows)
	}
}

// W5: deliveries of a disabled webhook stop retrying; dead/failed rows expire.
func TestW5_DisabledWebhookDropsDelivery(t *testing.T) {
	app, _ := setup(t)
	rcv := newReceiver(t, 200)
	w := addHook(t, app, rcv.srv.URL, "record.create", "")
	c := makeCollection(t, app, "items")
	newRecord(t, app, c, "x")
	rows := deliveries(t, app, StateQueued)
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	rec, _ := app.FindRecordById(ConfigCollection, w.ID)
	rec.Set("enabled", false)
	if err := app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if err := deliver(t.Context(), app, rows[0].Id); err != nil {
		t.Fatal(err)
	}
	d, _ := getDelivery(app, rows[0].Id)
	if d.State != StateDead || d.Attempt != 0 || rcv.count() != 0 || !strings.Contains(d.LastError, "disabled") {
		t.Fatalf("%+v hits=%d", d, rcv.count())
	}
}

func TestW5_PruneUnfinished(t *testing.T) {
	app, _ := setup(t)
	rcv := newReceiver(t, 200)
	w := addHook(t, app, rcv.srv.URL, "record.create", "")
	for _, st := range []string{StateDead, StateFailed, StateQueued} {
		id, err := insertDelivery(app, w, "record.create", "items", "r", []byte(`{"pii":1}`), 1, nowFn())
		if err != nil {
			t.Fatal(err)
		}
		old := fmtTime(nowFn().Add(-31 * 24 * time.Hour))
		if _, err := app.AuxDB().NewQuery("UPDATE {{" + DeliveriesTable + "}} SET state={:s}, updated={:u} WHERE id={:i}").
			Bind(map[string]any{"s": st, "u": old, "i": id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	pruneUnfinished(app, nowFn().Add(-deadRetention()))
	rows := deliveries(t, app, "")
	if len(rows) != 1 || rows[0].State != StateQueued {
		t.Fatalf("rows = %+v", rows)
	}
}

// W3: defaults and caps.
func TestW3_WorkerDefaultAndTimeoutCap(t *testing.T) {
	t.Setenv("TOKI_WEBHOOK_WORKERS", "")
	if workerCount() != 4 {
		t.Fatalf("workers = %d", workerCount())
	}
	t.Setenv("TOKI_WEBHOOK_WORKERS", "7")
	if workerCount() != 7 {
		t.Fatal(workerCount())
	}
	app, _ := setup(t)
	col, _ := app.FindCachedCollectionByNameOrId(ConfigCollection)
	r := core.NewRecord(col)
	r.Set("timeout_ms", 120000)
	if w := webhookOf(r); w.TimeoutMs != 30000 {
		t.Fatalf("timeout = %d", w.TimeoutMs)
	}
	r.Set("timeout_ms", 1)
	if w := webhookOf(r); w.TimeoutMs != 100 {
		t.Fatalf("timeout = %d", w.TimeoutMs)
	}
}

// W7: monotonic seq per webhook (payload + header); replay state guard.
func TestW7_SeqAndReplayGuard(t *testing.T) {
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.create", "")
	c := makeCollection(t, app, "items")
	newRecord(t, app, c, "a")
	newRecord(t, app, c, "b")
	m.Start()
	waitFor(t, "2 deliveries", func() bool { return len(deliveries(t, app, StateDelivered)) == 2 })
	seqs := map[int64]bool{}
	for _, h := range rcv.hits {
		p := payloadOf(t, h)
		seqs[p.Seq] = true
		if h.header.Get("X-Toki-Seq") != strconv.FormatInt(p.Seq, 10) {
			t.Fatalf("header seq %q vs %d", h.header.Get("X-Toki-Seq"), p.Seq)
		}
	}
	if !seqs[1] || !seqs[2] || len(seqs) != 2 {
		t.Fatalf("seqs = %v", seqs)
	}
	id := deliveries(t, app, StateDelivered)[0].Id
	if _, err := Replay(app, id, false); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("replay of delivered row must be refused, got %v", err)
	}
	if n, err := ReplayForce(app, id, false); err != nil || n != 1 {
		t.Fatalf("force replay: %d %v", n, err)
	}
}

// SSRF: extended ranges.
func TestSSRF_ExtendedRanges(t *testing.T) {
	for _, s := range []string{
		"64:ff9b::a9fe:a9fe", "64:ff9b::a00:1", "64:ff9b:1::1", "2002:a9fe:a9fe::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2",
		"240.0.0.1", "255.255.255.255", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "::1", "169.254.169.254", "fec0::1",
	} {
		if !blockedIP(mustAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if blockedIP(mustAddr(s)) {
			t.Errorf("%s must be allowed", s)
		}
	}
}

// 3xx is a terminal failure: no retries.
func TestRedirectIsNotRetried(t *testing.T) {
	app, m := setup(t)
	hits := atomic.Int32{}
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://127.0.0.1:1/", http.StatusFound)
	}))
	t.Cleanup(redir.Close)
	addHook(t, app, redir.URL, "record.create", "")
	c := makeCollection(t, app, "items")
	m.Start()
	newRecord(t, app, c, "x")
	waitFor(t, "dead", func() bool { return len(deliveries(t, app, StateDead)) == 1 })
	time.Sleep(100 * time.Millisecond)
	if d := deliveries(t, app, StateDead)[0]; d.Attempt != 1 || hits.Load() != 1 {
		t.Fatalf("redirect retried: %+v hits=%d", d, hits.Load())
	}
}

// Payload cap with truncation marker.
func TestPayloadCapTruncates(t *testing.T) {
	t.Setenv("TOKI_WEBHOOK_MAX_PAYLOAD_BYTES", "1000")
	app, m := setup(t)
	rcv := newReceiver(t, 200)
	addHook(t, app, rcv.srv.URL, "record.create", "")
	c := makeCollection(t, app, "items")
	m.Start()
	r := newRecord(t, app, c, strings.Repeat("x", 5000))
	waitFor(t, "delivery", func() bool { return rcv.count() == 1 })
	if len(rcv.last().body) > 1000 {
		t.Fatalf("body not truncated: %d bytes", len(rcv.last().body))
	}
	p := payloadOf(t, rcv.last())
	d, _ := p.Data.(map[string]any)
	if !p.Truncated || d["id"] != r.Id || p.RecordID != r.Id {
		t.Fatalf("bad truncated payload: %s", rcv.last().body)
	}
}

// claimNext survives (and logs) database errors instead of panicking.
func TestClaimNextDatabaseError(t *testing.T) {
	app, _ := setup(t)
	if _, err := app.AuxDB().NewQuery("DROP TABLE {{" + DeliveriesTable + "}}").Execute(); err != nil {
		t.Fatal(err)
	}
	claimLogMu.Lock()
	claimLogLast = time.Time{}
	claimLogMu.Unlock()
	if id, ok := claimNext(app, nowFn()); ok || id != "" {
		t.Fatal("claim on a broken table must fail")
	}
}
