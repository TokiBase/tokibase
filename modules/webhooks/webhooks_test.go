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
	w, err := Add(app, Webhook{Name: "wh" + strconv.Itoa(int(time.Now().UnixNano()%1e9)), URL: url, Secret: "s3cret",
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
	if want := Sign("s3cret", ts, h.body); h.header.Get("X-Toki-Signature") != want {
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
	if rec.GetString("secret") != "s3cret" {
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
