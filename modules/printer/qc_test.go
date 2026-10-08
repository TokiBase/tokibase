//go:build !no_printer

package printer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/internal/escpos"
	"github.com/tokibase/tokibase/kernel"
)

func TestE1DataCannotInjectDirectives(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return readyReply }}
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "ticket", "Plat: {{.plate}}")
	res, err := e.pm.Enqueue(context.Background(), Request{Template: "ticket", Data: map[string]any{"plate": "X\n@drawer\n@feed 255\n@cut"}})
	if err != nil {
		t.Fatal(err)
	}
	e.process(t)
	if got := e.job(t, res.ID).GetString("state"); got != StateDone {
		t.Fatalf("state=%s", got)
	}
	p := sc.payloads[0]
	if bytes.Contains(p, []byte{escpos.ESC, 'p'}) || bytes.Contains(p, []byte{escpos.GS, 'V'}) || bytes.Contains(p, []byte{escpos.ESC, 'd', 255}) {
		t.Fatalf("data was executed as directives: % X", p)
	}
}

func TestE3IdempotencyKeyIsPerActor(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	ctx := context.Background()
	a, err := e.pm.Enqueue(ctx, Request{Template: "t", IdempotencyKey: "order-1", Actor: "members/a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.pm.Enqueue(ctx, Request{Template: "t", IdempotencyKey: "order-1", Actor: "members/b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || b.Duplicate {
		t.Fatalf("user b got user a's job: %+v %+v", a, b)
	}
	again, _ := e.pm.Enqueue(ctx, Request{Template: "t", IdempotencyKey: "order-1", Actor: "members/a"})
	if again.ID != a.ID || !again.Duplicate {
		t.Fatalf("same actor must dedupe: %+v", again)
	}
}

func TestE4ErrorsHideAddresses(t *testing.T) {
	t.Setenv("TOKI_PRINT_ALLOW_COLLECTIONS", "members")
	e := setup(t)
	e.pm.Open = func(context.Context, *Printer) (io.ReadWriteCloser, error) {
		return nil, errors.New("dial tcp 10.1.2.3:9100: connect: connection refused")
	}
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", map[string]any{"default": true})
	e.addTemplate(t, "t", "hi")
	h := e.mux(t)
	user, _, super := e.users(t)
	rec := do(h, "POST", "/api/print", `{"template":"t"}`, user)
	var res Result
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	payload, _ := json.Marshal(map[string]string{"id": res.ID})
	_ = e.pm.handle(context.Background(), e.app, &kernel.Job{Payload: payload, Attempt: 1, MaxAttempts: 20})
	if rec = do(h, "GET", "/api/print/"+res.ID, "", user); strings.Contains(rec.Body.String(), "10.1.2") || !strings.Contains(rec.Body.String(), "last_error") {
		t.Fatalf("job leaks the address or has no error: %s", rec.Body)
	}
	if rec = do(h, "GET", "/api/print/printers", "", user); strings.Contains(rec.Body.String(), "10.1.2") || !strings.Contains(rec.Body.String(), `"offline"`) {
		t.Fatalf("printers leak the address: %s", rec.Body)
	}
	if rec = do(h, "GET", "/api/print/"+res.ID, "", super); !strings.Contains(rec.Body.String(), "10.1.2.3:9100") {
		t.Fatalf("superuser must see the full error: %s", rec.Body)
	}
	if rec = do(h, "GET", "/api/print/printers", "", super); !strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatalf("superuser must see the status detail: %s", rec.Body)
	}
}

func TestE2DefaultAuthIsRestrictive(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", map[string]any{"default": true})
	e.addTemplate(t, "t", "hi")
	h := e.mux(t)
	user, _, super := e.users(t)
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 403 {
		t.Fatalf("a self registered user printed by default: %d", rec.Code)
	}
	if rec := do(h, "GET", "/api/print/printers", "", user); rec.Code != 403 {
		t.Fatalf("printers by default: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, super); rec.Code != 200 {
		t.Fatalf("superuser: %d", rec.Code)
	}
	t.Setenv("TOKI_PRINT_ALLOW_COLLECTIONS", "members")
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 200 {
		t.Fatalf("allowlisted collection: %d", rec.Code)
	}
	t.Setenv("TOKI_PRINT_ALLOW_COLLECTIONS", "")
	t.Setenv("TOKI_PRINT_AUTH", "auth")
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 200 {
		t.Fatalf("explicit auth mode: %d", rec.Code)
	}
}

func TestE2QuotaAndThrottle(t *testing.T) {
	t.Setenv("TOKI_PRINT_ALLOW_COLLECTIONS", "members")
	t.Setenv("TOKI_PRINT_MAX_QUEUED_PER_ACTOR", "2")
	t.Setenv("TOKI_PRINT_RATE_PER_MIN", "5")
	e := setup(t)
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", map[string]any{"default": true})
	e.addTemplate(t, "t", "hi")
	h := e.mux(t)
	user, other, _ := e.users(t)
	for i := 0; i < 2; i++ {
		if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 200 {
			t.Fatalf("print %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 429 {
		t.Fatalf("over the queue quota: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, other); rec.Code != 200 {
		t.Fatalf("another actor is not affected: %d", rec.Code)
	}
	// the request rate: the IP bucket (shared by every test client) runs out
	var got429 bool
	for i := 0; i < 10 && !got429; i++ {
		got429 = do(h, "POST", "/api/print", `{"template":"t"}`, other).Code == 429
	}
	if !got429 {
		t.Fatal("no request throttle")
	}
}

// blockConn is a printer file whose write never returns (paper out on usblp).
type blockConn struct{ stop chan struct{} }

func (c *blockConn) Write([]byte) (int, error) { <-c.stop; return 0, io.ErrClosedPipe }
func (c *blockConn) Read([]byte) (int, error)  { return 0, os.ErrDeadlineExceeded }
func (c *blockConn) Close() error {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	return nil
}

func TestE5FileWriteDeadlineAndBusyLock(t *testing.T) {
	e := setup(t)
	e.pm.LockWait = 100 * time.Millisecond
	e.pm.Open = func(context.Context, *Printer) (io.ReadWriteCloser, error) {
		return &blockConn{stop: make(chan struct{})}, nil
	}
	e.addPrinter(t, "lp", "file", "/dev/usb/lp0", map[string]any{"timeout_ms": 200})
	e.addTemplate(t, "t", "x")
	res, _ := e.pm.Enqueue(context.Background(), Request{Template: "t"})
	start := time.Now()
	e.process(t)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("a blocked write held the worker for %v", d)
	}
	r := e.job(t, res.ID)
	if r.GetString("state") != StateFailed || !strings.Contains(r.GetString("last_error"), "timeout") {
		t.Fatalf("state=%s err=%q", r.GetString("state"), r.GetString("last_error"))
	}

	// a busy lock gives the worker back instead of blocking it
	rel, err := e.pm.acquire(context.Background(), "lp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pm.acquire(context.Background(), "lp"); !errors.Is(err, errPrinterBusy) {
		t.Fatalf("second acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.pm.acquire(ctx, "lp"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	rel()
	if rel2, err := e.pm.acquire(context.Background(), "lp"); err != nil {
		t.Fatal(err)
	} else {
		rel2()
	}
}

func TestE6PaperOutAfterWriteIsNotResent(t *testing.T) {
	e := setup(t)
	var calls atomic.Int32
	sc := &script{reply: func() []byte {
		if calls.Add(1) == 1 {
			return readyReply
		}
		return []byte{0x12, 0x32, 0x12, 0x12} // n=2 bit 5: stopped by paper end
	}}
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "receipt")
	res, _ := e.pm.Enqueue(context.Background(), Request{Template: "t"})
	e.process(t)
	r := e.job(t, res.ID)
	if r.GetString("state") != StateUnconfirmed || !strings.Contains(r.GetString("last_error"), "paper out") {
		t.Fatalf("state=%s err=%q", r.GetString("state"), r.GetString("last_error"))
	}
	e.now = e.now.Add(time.Hour)
	if e.process(t) || sc.payloadCount() != 1 {
		t.Fatalf("the job was sent again: %d writes", sc.payloadCount())
	}
	if e.audits(AuditUnconfirmed) != 1 {
		t.Fatalf("audit = %d", e.audits(AuditUnconfirmed))
	}
	// an explicit retry reprints it
	if _, err := e.pm.Retry(context.Background(), res.ID); err != nil {
		t.Fatal(err)
	}
}

func TestE6MechanicalErrorFailsInsteadOfWaiting(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return []byte{0x12, 0x12, 0x1A, 0x12} }} // n=3 bit 3: cutter error
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	res, _ := e.pm.Enqueue(context.Background(), Request{Template: "t"})
	e.process(t)
	r := e.job(t, res.ID)
	if r.GetString("state") != StateFailed || !strings.Contains(r.GetString("last_error"), "cutter error") || sc.payloadCount() != 0 {
		t.Fatalf("state=%s err=%q writes=%d", r.GetString("state"), r.GetString("last_error"), sc.payloadCount())
	}
}

func TestE6WaitsAreBounded(t *testing.T) {
	e := setup(t)
	e.pm.MaxWaits = 2
	sc := &script{reply: func() []byte { return paperOut }}
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	res, _ := e.pm.Enqueue(context.Background(), Request{Template: "t"})
	for i := 0; i < 6; i++ {
		e.process(t)
		e.now = e.now.Add(11 * time.Second)
	}
	r := e.job(t, res.ID)
	if r.GetString("state") != StateDead || e.audits(AuditStuck) != 1 {
		t.Fatalf("state=%s stuck audits=%d", r.GetString("state"), e.audits(AuditStuck))
	}
	if sc.payloadCount() != 0 {
		t.Fatal("bytes sent to a printer without paper")
	}
}

func TestE10SilentPrinterIsNotAskedAgain(t *testing.T) {
	e := setup(t)
	var calls atomic.Int32
	sc := &script{reply: func() []byte { calls.Add(1); return nil }}
	e.pm.Open = sc.open
	e.addPrinter(t, "cheap", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	for i := 0; i < 3; i++ {
		_, _ = e.pm.Enqueue(context.Background(), Request{Template: "t"})
	}
	for i := 0; i < 3; i++ {
		e.process(t)
	}
	if sc.payloadCount() != 3 || calls.Load() != 1 {
		t.Fatalf("writes=%d status queries=%d, want 3 and 1", sc.payloadCount(), calls.Load())
	}
	e.pm.Now = func() time.Time { return time.Now().Add(time.Hour) }
	_, _ = e.pm.Enqueue(context.Background(), Request{Template: "t"})
	e.process(t)
	if calls.Load() != 2 {
		t.Fatalf("the printer was not probed again after the TTL: %d", calls.Load())
	}
}
