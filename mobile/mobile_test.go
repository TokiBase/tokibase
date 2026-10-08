package mobile

import (
	"encoding/json"
	"testing"
	"time"
)

type cb struct{ ch chan []byte }

func (c cb) OnEvent(d []byte) { c.ch <- d }

func TestFacade(t *testing.T) {
	h, err := Start(t.TempDir(), "127.0.0.1:0", `{"logLevel":"error"}`)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = h.Stop()
		}
	}()
	if h.URL() == "" {
		t.Fatal("empty url")
	}

	r, err := h.Call("GET", "/api/health", "", nil)
	if err != nil || r.Status != 200 {
		t.Fatalf("health: %v %+v", err, r)
	}
	var hdr map[string]string
	if err := json.Unmarshal([]byte(r.HeadersJSON), &hdr); err != nil {
		t.Fatal(err)
	}

	if err := h.Superuser("a@example.com", "supersecret123"); err != nil {
		t.Fatal(err)
	}
	r, _ = h.Call("POST", "/api/collections/_superusers/auth-with-password", "", []byte(`{"identity":"a@example.com","password":"supersecret123"}`))
	var auth struct{ Token string }
	_ = json.Unmarshal(r.Body, &auth)
	if r.Status != 200 || auth.Token == "" {
		t.Fatalf("auth: %d %s", r.Status, r.Body)
	}
	ah, _ := json.Marshal(map[string]string{"Authorization": auth.Token})

	r, _ = h.Call("POST", "/api/collections", string(ah), []byte(`{"name":"notes","type":"base","listRule":"","createRule":"","fields":[{"name":"t","type":"text"}]}`))
	if r.Status != 200 {
		t.Fatalf("collection: %d %s", r.Status, r.Body)
	}

	c := cb{make(chan []byte, 2)}
	id, err := h.Subscribe("notes/*", c)
	if err != nil {
		t.Fatal(err)
	}
	r, _ = h.Call("POST", "/api/collections/notes/records", string(ah), []byte(`{"t":"x"}`))
	if r.Status != 200 {
		t.Fatalf("record: %d %s", r.Status, r.Body)
	}
	select {
	case <-c.ch:
	case <-time.After(5 * time.Second):
		t.Fatal("callback not invoked")
	}
	h.Unsubscribe(id)
	h.Unsubscribe(id)

	if _, err := h.Call("GET", "/", "not json", nil); err == nil {
		t.Fatal("bad headersJSON must error")
	}
	if _, err := h.Subscribe("notes/*", nil); err == nil {
		t.Fatal("nil callback must error")
	}
	stopped = true
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("notes/*", c); err == nil {
		t.Fatal("Subscribe after Stop must error")
	}
	h.Unsubscribe(id)
}

func TestBadEnv(t *testing.T) {
	if _, err := Start(t.TempDir(), "", "[1]"); err == nil {
		t.Fatal("bad envJSON must error")
	}
}

// The sync wrappers fail cleanly on an instance that is not a spoke.
func TestSyncWrappersWithoutSpoke(t *testing.T) {
	h, err := Start(t.TempDir(), "", `{"logLevel":"error"}`)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop()
	if _, err := h.SyncStatus(); err == nil {
		t.Fatal("SyncStatus must fail on a node that is not a spoke")
	}
	if err := h.SyncEnroll("http://127.0.0.1:1", "x"); err == nil {
		t.Fatal("SyncEnroll must fail on a node that is not a spoke")
	}
	if _, err := h.SyncNext("seq"); err == nil {
		t.Fatal("SyncNext must fail")
	}
	h.SyncSetConditions(true, false, false, false)
	id, err := h.SyncSubscribe(cb{make(chan []byte, 1)})
	if err != nil {
		t.Fatal(err)
	}
	h.Unsubscribe(id)
}
