//go:build !no_scanner

package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/subscriptions"
)

func TestE7DefaultReadIsServiceOnly(t *testing.T) {
	e := setup(t)
	t.Setenv("TOKI_SCAN_READ_AUTH", "")
	user, su := e.token(t, "users"), e.token(t, core.CollectionNameSuperusers)
	c := e.connectSSE(t)
	if st := c.subscribe(e, user, Topic); st != 403 {
		t.Fatalf("a plain user subscribed by default: %d", st)
	}
	if st, _ := e.get(t, user, "/api/scan/events"); st != 403 {
		t.Fatalf("a plain user read the events by default: %d", st)
	}
	if st, _ := e.get(t, su, "/api/scan/events"); st != 200 {
		t.Fatalf("superuser: %d", st)
	}
	t.Setenv("TOKI_SCAN_READ_AUTH", "users")
	if st, _ := e.get(t, user, "/api/scan/events"); st != 200 {
		t.Fatalf("allowlisted collection: %d", st)
	}
	t.Setenv("TOKI_SCAN_READ_AUTH", "")
	t.Setenv("TOKI_SCAN_TOPIC_AUTH", "auth")
	if st, _ := e.get(t, user, "/api/scan/events"); st != 200 {
		t.Fatalf("explicit auth mode: %d", st)
	}
}

func TestE8PostNeedsAnAllowedActor(t *testing.T) {
	e := setup(t)
	t.Setenv("TOKI_SCAN_POST_COLLECTIONS", "")
	user, su := e.token(t, "users"), e.token(t, core.CollectionNameSuperusers)
	if st, _ := e.post(t, user, map[string]any{"code": "ABC12345"}); st != 403 {
		t.Fatalf("a plain user posted by default: %d", st)
	}
	if st, _ := e.post(t, su, map[string]any{"code": "ABC12345"}); st != 200 {
		t.Fatalf("superuser: %d", st)
	}
	// a configured scanner decides for itself
	e.saveScanner(t, map[string]any{"name": "door", "kind": KindWeb, "enabled": true, "allowed_actors": "users/someoneelse"})
	if st, _ := e.post(t, user, map[string]any{"code": "ABC12345", "scanner": "door"}); st != 403 {
		t.Fatalf("allowed_actors must exclude this record: %d", st)
	}
	r, _ := e.app.FindFirstRecordByData(ConfigCollection, "name", "door")
	r.Set("allowed_actors", "users")
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.post(t, user, map[string]any{"code": "ABC12345", "scanner": "door"}); st != 200 {
		t.Fatalf("allowed_actors collection: %d", st)
	}
	// the implicit "web" scanner disappears once a web scanner is configured
	if st, _ := e.post(t, su, map[string]any{"code": "ABC12345", "scanner": "web"}); st != 404 {
		t.Fatalf("built-in web scanner next to a configured one: %d", st)
	}
	if st, _ := e.post(t, su, map[string]any{"code": "ABC12345", "client_seq": strings.Repeat("x", 200)}); st != 400 {
		t.Fatalf("long client_seq: %d", st)
	}
}

func TestE8PayloadCarriesSourceAndActor(t *testing.T) {
	e := setup(t)
	su := e.token(t, core.CollectionNameSuperusers)
	c := e.connectSSE(t)
	if st := c.subscribe(e, su, Topic); st != 204 {
		t.Fatalf("subscribe %d", st)
	}
	user := e.token(t, "users")
	if st, _ := e.post(t, user, map[string]any{"code": "SRC12345"}); st != 200 {
		t.Fatal("post")
	}
	got := c.next(3 * time.Second)
	if got == nil || got["source"] != "web" || !strings.HasPrefix(got["actor"].(string), "users/") {
		t.Fatalf("payload %v", got)
	}
	if st, o := e.get(t, su, "/api/scan/events"); st != 200 || !strings.Contains(strings.ToLower(toJSON(o)), `"actor":"users/`) {
		t.Fatalf("events lack the actor: %d %v", st, o)
	}
}

func toJSON(v any) string {
	var sb strings.Builder
	var enc func(v any)
	enc = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			sb.WriteString("{")
			for k, e := range x {
				sb.WriteString(`"` + k + `":`)
				enc(e)
				sb.WriteString(",")
			}
			sb.WriteString("}")
		case []any:
			for _, e := range x {
				enc(e)
			}
		case string:
			sb.WriteString(`"` + x + `"`)
		}
	}
	enc(v)
	return sb.String()
}

func TestE9PublishIsOrdered(t *testing.T) {
	e := setup(t)
	su := e.token(t, core.CollectionNameSuperusers)
	c := e.connectSSE(t)
	if st := c.subscribe(e, su, Topic); st != 204 {
		t.Fatalf("subscribe %d", st)
	}
	sc := defaultWeb()
	sc.DedupeMs = -1
	for i := 0; i < 30; i++ {
		if _, err := e.m.Ingest(context.Background(), sc, "SEQ"+string(rune('A'+i)), IngestOptions{Source: "web"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		got := c.next(3 * time.Second)
		if got == nil || got["code"] != "SEQ"+string(rune('A'+i)) {
			t.Fatalf("event %d out of order or missing: %v", i, got)
		}
	}
}

func TestE9StalledClientIsBounded(t *testing.T) {
	old := senderIdle
	senderIdle = 50 * time.Millisecond
	t.Cleanup(func() { senderIdle = old })
	e := setup(t)
	su, _ := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	cl := subscriptions.NewDefaultClient()
	cl.Set(apis.RealtimeClientAuthKey, su)
	cl.Subscribe(Topic)
	e.app.SubscriptionsBroker().Register(cl)
	sc := defaultWeb()
	sc.DedupeMs = -1
	const total = 3 * senderQueue
	for i := 0; i < total; i++ { // nobody reads the channel yet
		_, _ = e.m.Ingest(context.Background(), sc, fmt.Sprintf("STALL%04d", i), IngestOptions{Source: "web"})
	}
	e.m.sendMu.Lock()
	n := len(e.m.senders)
	e.m.sendMu.Unlock()
	if n != 1 {
		t.Fatalf("%d sender goroutines for one client", n)
	}
	// the client wakes up: it gets a bounded backlog that ends with the newest scan
	var last string
	count := 0
	for {
		select {
		case msg := <-cl.Channel():
			var o map[string]any
			_ = json.Unmarshal(msg.Data, &o)
			last = o["code"].(string)
			count++
			continue
		case <-time.After(400 * time.Millisecond):
		}
		break
	}
	if count > senderQueue+2 || last != fmt.Sprintf("STALL%04d", total-1) {
		t.Fatalf("received %d events, last %q", count, last)
	}
	cl.Discard()
	for i := 0; i < 100; i++ {
		e.m.sendMu.Lock()
		n = len(e.m.senders)
		e.m.sendMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the sender goroutine of a discarded client was not released")
}

func TestE7DeletedUserStopsReceiving(t *testing.T) {
	e := setup(t)
	t.Setenv("TOKI_SCAN_TOPIC_AUTH", "auth")
	r, _ := e.app.FindAuthRecordByEmail("users", "test@example.com")
	tok := e.token(t, "users")
	c := e.connectSSE(t)
	if st := c.subscribe(e, tok, Topic); st != 204 {
		t.Fatalf("subscribe %d", st)
	}
	if err := e.app.Delete(r); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Ingest(context.Background(), defaultWeb(), "GONE1234", IngestOptions{Source: "web"}); err != nil {
		t.Fatal(err)
	}
	if g := c.next(400 * time.Millisecond); g != nil {
		t.Fatalf("a deleted user still receives scans: %v", g)
	}
}

func TestE17DevicePathsAndMonotonicClock(t *testing.T) {
	for _, s := range []Scanner{
		{Name: "x", Kind: KindEvdev, Device: "/dev/zero"},
		{Name: "x", Kind: KindEvdev, Device: "/dev/ttyUSB0"},
		{Name: "x", Kind: KindSerial, Device: "/dev/urandom"},
	} {
		s.applyDefaults()
		if err := s.Validate(); err == nil {
			t.Errorf("%+v must be invalid", s)
		}
	}
	e := setup(t)
	m := newModule(e.app)
	if !strings.Contains(m.now().String(), "m=+") {
		t.Error("the default clock lost its monotonic reading")
	}
}
