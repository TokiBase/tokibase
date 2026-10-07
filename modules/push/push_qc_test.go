package push

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/kernel"
)

func auditCapture(t *testing.T) *[]string {
	var got []string
	SetAuditSink(func(action, _, _ string, _ map[string]any) { got = append(got, action) })
	t.Cleanup(func() { SetAuditSink(nil) })
	return &got
}

func (e *env) register(t *testing.T, tok, body string) {
	t.Helper()
	if rec := e.do("POST", "/api/push/devices", tok, body); rec.Code != 200 {
		t.Fatalf("register %s: %d %s", body, rec.Code, rec.Body)
	}
}

// P1: classification of provider answers.
func TestP1TokenDeadClassification(t *testing.T) {
	f := &FCM{}
	a := &APNs{Now: time.Now}
	var ce *ConfigError
	var pe *PermanentError
	cases := []struct {
		name   string
		err    error
		dead   bool
		config bool
		perm   bool
	}{
		{"fcm unregistered", classifyFCM(f, 404, []byte(`{"error":{"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`)), true, false, false},
		{"fcm not found project", classifyFCM(f, 404, []byte(`{"error":{"status":"NOT_FOUND","message":"Requested entity was not found."}}`)), false, true, false},
		{"fcm 404 empty", classifyFCM(f, 404, nil), false, true, false},
		{"fcm invalid arg token", classifyFCM(f, 400, []byte(`{"error":{"status":"INVALID_ARGUMENT","details":[{"fieldViolations":[{"field":"message.token"}]}]}}`)), true, false, false},
		{"fcm invalid arg other", classifyFCM(f, 400, []byte(`{"error":{"status":"INVALID_ARGUMENT","details":[{"fieldViolations":[{"field":"message.data"}]}]}}`)), false, false, true},
		{"fcm permission denied", classifyFCM(f, 403, []byte(`{"error":{"status":"PERMISSION_DENIED"}}`)), false, true, false},
		{"apns BadDeviceToken", classifyAPNs(a, 400, []byte(`{"reason":"BadDeviceToken"}`)), true, false, false},
		{"apns Unregistered", classifyAPNs(a, 410, []byte(`{"reason":"Unregistered"}`)), true, false, false},
		{"apns NotForTopic", classifyAPNs(a, 400, []byte(`{"reason":"DeviceTokenNotForTopic"}`)), false, true, false},
		{"apns BadTopic", classifyAPNs(a, 400, []byte(`{"reason":"BadTopic"}`)), false, true, false},
		{"apns MissingTopic", classifyAPNs(a, 400, []byte(`{"reason":"MissingTopic"}`)), false, true, false},
		{"apns InvalidProviderToken", classifyAPNs(a, 403, []byte(`{"reason":"InvalidProviderToken"}`)), false, true, false},
	}
	for _, c := range cases {
		if errors.Is(c.err, ErrInvalidToken) != c.dead || errors.As(c.err, &ce) != c.config || errors.As(c.err, &pe) != c.perm {
			t.Errorf("%s: %v", c.name, c.err)
		}
	}
}

// P1 + P5: a configuration error keeps the device enabled, retries, is audited.
func TestP1P5ConfigErrorRetriesAndAudits(t *testing.T) {
	e := setup(t)
	acts := auditCapture(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"t1","platform":"fcm"}`)
	e.fcm.SetError("t1", &ConfigError{Err: errors.New("fcm: HTTP 404 NOT_FOUND")})
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	errs := e.q.run(e.app)
	var ce *ConfigError
	if !errors.As(errs[0], &ce) {
		t.Fatalf("config error must be returned for retry: %v", errs[0])
	}
	devs, _ := ListDevices(e.app, "members", a.Id)
	if !devs[0].Enabled {
		t.Fatal("config error must not disable the device")
	}
	found := false
	for _, x := range *acts {
		found = found || x == ActionProviderError
	}
	if !found {
		t.Fatalf("no %s audit: %v", ActionProviderError, *acts)
	}
	// a missing provider is audited as well
	e.m.SetProvider(PlatformFCM, nil)
	t.Setenv("TOKI_PUSH_FCM_SA_FILE", "")
	t.Setenv("TOKI_PUSH_FCM_SA_B64", "")
	e.m.noteLast = nil
	*acts = nil
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	if errs := e.q.run(e.app); errs[0] == nil {
		t.Fatal("expected error")
	}
	if len(*acts) < 2 || (*acts)[len(*acts)-1] != ActionProviderError {
		t.Fatalf("%v", *acts)
	}
}

// P5: an oauth endpoint rejection is retryable config, not a silently dropped permanent error.
func TestP5OAuthRejectionIsConfigError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	sa, _ := testRSAServiceAccount(t, srv.URL)
	f, err := NewFCM(sa)
	if err != nil {
		t.Fatal(err)
	}
	var ce *ConfigError
	if err := f.Send(context.Background(), &Notification{Title: "x"}, "tok"); !errors.As(err, &ce) {
		t.Fatalf("%v", err)
	}
}

// Circuit breaker: mass rejections stop disabling devices.
func TestBreakerStopsMassDisable(t *testing.T) {
	m := &Module{}
	for i := 0; i < breakerMax; i++ {
		if !m.allowDisable() {
			t.Fatalf("breaker tripped early at %d", i)
		}
	}
	if m.allowDisable() {
		t.Fatal("breaker must trip")
	}
}

// P2: apns-topic comes from the device app_id; app_id is validated.
func TestP2PerDeviceTopic(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"ios-1","platform":"apns","app_id":"com.acme.app"}`)
	e.register(t, ta, `{"token":"ios-2","platform":"apns"}`)
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	e.q.run(e.app)
	topics := map[string]string{}
	for _, s := range e.apns.Sent() {
		topics[s.Token] = s.N.Topic
	}
	if topics["ios-1"] != "com.acme.app" || topics["ios-2"] != "" {
		t.Fatalf("topics %v", topics)
	}
	for _, bad := range []string{"bad id!", "-x", "a/b", "x..", strings.Repeat("a", 201)} {
		if rec := e.do("POST", "/api/push/devices", ta, toJSON(map[string]string{"token": "z", "platform": "apns", "app_id": bad})); rec.Code != 400 {
			t.Errorf("app_id %q accepted: %d", bad, rec.Code)
		}
	}
	t.Setenv("TOKI_PUSH_APP_IDS", "com.acme.app")
	if rec := e.do("POST", "/api/push/devices", ta, `{"token":"z","platform":"apns","app_id":"com.other.app"}`); rec.Code != 400 {
		t.Fatal("allowlist not enforced")
	}
}

func TestP2APNsRequestUsesNotificationTopic(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	a, _ := NewAPNs(APNsConfig{KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), KeyID: "k", TeamID: "t", Topic: "global.app"})
	req, _ := a.BuildRequest(context.Background(), &Notification{Title: "x", Topic: "per.device"}, "tok")
	if req.Header.Get("apns-topic") != "per.device" {
		t.Fatal(req.Header)
	}
	req, _ = a.BuildRequest(context.Background(), &Notification{Title: "x"}, "tok")
	if req.Header.Get("apns-topic") != "global.app" {
		t.Fatal(req.Header)
	}
}

// P3: UNREGISTERED for a job enqueued before a re-registration must not disable the fresh row.
func TestP3ReRegistrationRace(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"racy","platform":"fcm"}`)
	e.fcm.SetError("racy", errors.Join(ErrInvalidToken, errors.New("UNREGISTERED")))
	if n, _ := SendToUser(e.app, "members", a.Id, Notification{Title: "x"}); n != 1 {
		t.Fatal(n)
	}
	time.Sleep(15 * time.Millisecond)
	e.register(t, ta, `{"token":"racy","platform":"fcm"}`) // app re-registers while the job is in flight
	e.q.run(e.app)
	if devs, _ := ListDevices(e.app, "members", a.Id); !devs[0].Enabled {
		t.Fatal("fresh registration was disabled by a stale answer")
	}
	// a job enqueued after the registration does disable it
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x"})
	e.q.run(e.app)
	if devs, _ := ListDevices(e.app, "members", a.Id); devs[0].Enabled {
		t.Fatal("a current UNREGISTERED must disable")
	}
}

// P4: the TTL is absolute across retries.
func TestP4TTLAcrossRetries(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"ttl-1","platform":"fcm"}`)
	devs, _ := ListDevices(e.app, "members", a.Id)
	run := func(p jobPayload) {
		p.Device = devs[0].ID
		b, _ := json.Marshal(p)
		if err := e.m.handleJob(context.Background(), e.app, &kernel.Job{Payload: b}); err != nil {
			t.Fatal(err)
		}
	}
	// expired: dropped, never sent
	run(jobPayload{ExpiresAt: time.Now().Add(-time.Minute).Unix(), Notification: Notification{Title: "otp", TTLSeconds: 60}})
	if len(e.fcm.Sent()) != 0 {
		t.Fatal("expired message was delivered")
	}
	// still valid: remaining TTL, not the original one
	run(jobPayload{ExpiresAt: time.Now().Add(20 * time.Second).Unix(), Notification: Notification{Title: "otp", TTLSeconds: 3600}})
	sent := e.fcm.Sent()
	if len(sent) != 1 || sent[0].N.TTLSeconds > 20 || sent[0].N.TTLSeconds < 1 {
		t.Fatalf("%+v", sent)
	}
	// enqueue stores the absolute expiry
	_, _ = SendToUser(e.app, "members", a.Id, Notification{Title: "x", TTLSeconds: 60})
	var p jobPayload
	_ = json.Unmarshal(e.q.jobs[0].Payload, &p)
	if p.ExpiresAt < time.Now().Unix()+55 || p.Enqueued == 0 {
		t.Fatalf("%+v", p)
	}
	if _, err := SendToUser(e.app, "members", a.Id, Notification{Title: "x", TTLSeconds: maxTTLSeconds + 1}); err == nil {
		t.Fatal("ttl above the FCM maximum must be rejected")
	}
}

// P6: the device token never reaches errors that are stored or logged.
func TestP6TokenNotInErrors(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	a, _ := NewAPNs(APNsConfig{KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), KeyID: "k", TeamID: "t", Topic: "b"})
	srv := httptest.NewServer(http.NotFoundHandler())
	a.BaseURL = srv.URL
	srv.Close() // connection refused
	const tok = "0123456789abcdef0123456789abcdef0123456789abcdef"
	err := a.Send(context.Background(), &Notification{Title: "x"}, tok)
	if err == nil || strings.Contains(err.Error(), tok) {
		t.Fatalf("token leaked: %v", err)
	}

	e := setup(t)
	u, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"`+tok+`","platform":"fcm"}`)
	e.fcm.SetError(tok, fmt.Errorf(`Post "https://api.push.apple.com/3/device/%s": dial tcp: timeout`, tok))
	_, _ = SendToUser(e.app, "members", u.Id, Notification{Title: "x"})
	got := e.q.run(e.app)[0]
	if got == nil || strings.Contains(got.Error(), tok) {
		t.Fatalf("stored error leaks the token: %v", got)
	}
}

// P7: at most 20 devices per user, oldest evicted.
func TestP7DeviceCap(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	for i := 0; i < maxDevicesPerUser+5; i++ {
		e.register(t, ta, fmt.Sprintf(`{"token":"dev-%02d","platform":"fcm"}`, i))
		time.Sleep(2 * time.Millisecond)
	}
	devs, _ := ListDevices(e.app, "members", a.Id)
	if len(devs) != maxDevicesPerUser {
		t.Fatalf("%d devices", len(devs))
	}
	have := map[string]bool{}
	for _, d := range devs {
		have[d.Token] = true
	}
	if have["dev-00"] || !have["dev-24"] {
		t.Fatalf("eviction must drop the oldest: %v", have)
	}
}

// P8: users see and join only public topics.
func TestP8TopicVisibility(t *testing.T) {
	e := setup(t)
	_ = CreateTopicVisibility(e.app, "promo", "p", VisibilityPublic)
	_ = CreateTopic(e.app, "staff-alerts", "secret")
	su := e.superuser(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"t1","platform":"fcm"}`)

	rec := e.do("GET", "/api/push/topics", ta, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "staff-alerts") || !strings.Contains(rec.Body.String(), "promo") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/api/push/subscribe", ta, `{"token":"t1","topic":"staff-alerts"}`); rec.Code != 404 {
		t.Fatalf("private topic joined: %d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/subscribe", ta, `{"token":"t1","topic":"promo"}`); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	// server side sends to a private topic still work once a subscription exists
	devs, _ := ListDevices(e.app, "members", a.Id)
	if err := Subscribe(e.app, devs[0].ID, "staff-alerts"); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("POST", "/api/push/send", su, `{"to":{"topics":["staff-alerts"]},"title":"x"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"queued":1`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// P10: DELETE accepts the token in the body or a header.
func TestP10DeleteWithoutTokenInURL(t *testing.T) {
	e := setup(t)
	a, ta := e.user(t, "a@example.com")
	for _, tok := range []string{"d1", "d2", "d3"} {
		e.register(t, ta, `{"token":"`+tok+`","platform":"fcm"}`)
	}
	if rec := e.do("DELETE", "/api/push/devices", ta, `{"token":"d1"}`); rec.Code != 204 {
		t.Fatalf("body: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("DELETE", "/api/push/devices", nil)
	req.Header.Set("Authorization", ta)
	req.Header.Set("X-Push-Token", "d2")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("header: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/push/devices/d3", ta, ""); rec.Code != 204 {
		t.Fatalf("url: %d", rec.Code)
	}
	if rec := e.do("DELETE", "/api/push/devices", ta, `{}`); rec.Code != 400 {
		t.Fatalf("missing token: %d", rec.Code)
	}
	if devs, _ := ListDevices(e.app, "members", a.Id); len(devs) != 0 {
		t.Fatal("devices left")
	}
}

// P11: numbers in data keep their digits.
func TestP11DataNumbers(t *testing.T) {
	got, err := stringifyData(map[string]any{
		"a": float64(1000000), "b": json.Number("12345678901234567"), "c": 1.5, "d": true, "e": nil, "f": []any{1},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "1000000", "b": "12345678901234567", "c": "1.5", "d": "true", "e": "", "f": "[1]"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q want %q", k, got[k], v)
		}
	}

	e := setup(t)
	su := e.superuser(t)
	a, ta := e.user(t, "a@example.com")
	e.register(t, ta, `{"token":"n1","platform":"fcm"}`)
	body := fmt.Sprintf(`{"to":{"users":[{"collection":"members","id":%q}]},"title":"x","data":{"order_id":1000000,"big":12345678901234567}}`, a.Id)
	if rec := e.do("POST", "/api/push/send", su, body); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var p jobPayload
	_ = json.Unmarshal(e.q.jobs[0].Payload, &p)
	if p.Data["order_id"] != "1000000" || p.Data["big"] != "12345678901234567" {
		t.Fatalf("%v", p.Data)
	}
}

// Non superusers cannot send.
func TestSendRequiresSuperuser(t *testing.T) {
	e := setup(t)
	_, ta := e.user(t, "a@example.com")
	if rec := e.do("POST", "/api/push/send", ta, `{"to":{"topics":["x"]},"title":"x"}`); rec.Code != 403 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := e.do("POST", "/api/push/send", "", `{}`); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("%d", rec.Code)
	}
}
