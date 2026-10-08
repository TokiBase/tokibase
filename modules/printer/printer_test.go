//go:build !no_printer

package printer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/internal/escpos"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/jobs"
	"github.com/tokibase/tokibase/tests"
)

type env struct {
	app *tests.TestApp
	jm  *jobs.Module
	pm  *Module
	now time.Time

	mu    sync.Mutex
	audit []string
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	e := &env{app: app, now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e.jm = jobs.Register(app)
	e.jm.Now = func() time.Time { return e.now }
	e.jm.Jitter = func() float64 { return 0.5 }
	e.pm = Register(app)
	SetAuditSink(func(action, col, rec string, d map[string]any) {
		e.mu.Lock()
		e.audit = append(e.audit, action)
		e.mu.Unlock()
	})
	t.Cleanup(func() { SetAuditSink(nil) })
	return e
}

func (e *env) audits(action string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, a := range e.audit {
		if a == action {
			n++
		}
	}
	return n
}

func (e *env) addPrinter(t *testing.T, name, transport, address string, set map[string]any) {
	t.Helper()
	col, err := e.app.FindCachedCollectionByNameOrId(PrintersCollection)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("transport", transport)
	r.Set("address", address)
	r.Set("enabled", true)
	r.Set("cols", 32)
	r.Set("qr_native", true)
	r.Set("timeout_ms", 300)
	for k, v := range set {
		r.Set(k, v)
	}
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
}

func (e *env) addTemplate(t *testing.T, name, body string) *core.Record {
	t.Helper()
	col, err := e.app.FindCachedCollectionByNameOrId(TemplatesCollection)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	r.Set("name", name)
	r.Set("body", body)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

const ticketTpl = "@center\nTICKET {{.plate}}\n@qr {{.id}}\n@feed 2\n@cut"

func (e *env) job(t *testing.T, id string) *core.Record {
	t.Helper()
	r, err := e.app.FindRecordById(JobsCollection, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) process(t *testing.T) bool {
	t.Helper()
	ok, err := e.jm.ProcessOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// ---- fake transport --------------------------------------------------------

var (
	readyReply = []byte{0x12, 0x12, 0x12, 0x12}
	paperOut   = []byte{0x12, 0x12, 0x12, 0x72}
)

type script struct {
	mu       sync.Mutex
	reply    func() []byte // nil result = the printer does not answer
	payloads [][]byte
	opens    int
	inflight int32
	maxIn    int32
	hold     time.Duration
}

func (s *script) payloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.payloads)
}

func (s *script) open(context.Context, *Printer) (io.ReadWriteCloser, error) {
	s.mu.Lock()
	s.opens++
	s.mu.Unlock()
	n := atomic.AddInt32(&s.inflight, 1)
	for {
		m := atomic.LoadInt32(&s.maxIn)
		if n <= m || atomic.CompareAndSwapInt32(&s.maxIn, m, n) {
			break
		}
	}
	return &fakeConn{s: s}, nil
}

type fakeConn struct {
	s      *script
	buf    bytes.Buffer
	closed bool
}

func (c *fakeConn) Write(p []byte) (int, error) {
	if bytes.Equal(p, escpos.StatusQuery()) {
		if r := c.s.reply(); r != nil {
			c.buf.Write(r)
		}
		return len(p), nil
	}
	if c.s.hold > 0 {
		time.Sleep(c.s.hold)
	}
	c.s.mu.Lock()
	c.s.payloads = append(c.s.payloads, append([]byte{}, p...))
	c.s.mu.Unlock()
	return len(p), nil
}

func (c *fakeConn) Read(p []byte) (int, error) {
	if c.buf.Len() == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	return c.buf.Read(p)
}

func (c *fakeConn) Close() error {
	if !c.closed {
		c.closed = true
		atomic.AddInt32(&c.s.inflight, -1)
	}
	return nil
}

// ---- tests -----------------------------------------------------------------

func TestPaperOutThenDone(t *testing.T) {
	e := setup(t)
	var out atomic.Bool
	out.Store(true)
	sc := &script{reply: func() []byte {
		if out.Load() {
			return paperOut
		}
		return readyReply
	}}
	e.pm.Open = sc.open
	e.addPrinter(t, "counter", "tcp", "127.0.0.1:9100", map[string]any{"default": true})
	e.addTemplate(t, "ticket", ticketTpl)

	res, err := e.pm.Enqueue(context.Background(), Request{Template: "ticket", Data: map[string]any{"plate": "B 1 XY", "id": "T-0001"}, Actor: "gate/1"})
	if err != nil {
		t.Fatal(err)
	}
	if !e.process(t) {
		t.Fatal("no job processed")
	}
	r := e.job(t, res.ID)
	if r.GetString("state") != StateWaiting || r.GetString("last_error") != "paper out" {
		t.Fatalf("state=%s err=%q", r.GetString("state"), r.GetString("last_error"))
	}
	if sc.payloadCount() != 0 {
		t.Fatal("nothing may be sent while the paper is out")
	}
	// the wait job runs only after the delay, and burns no attempt of the queue
	if e.process(t) {
		t.Fatal("the waiting job must not run before WaitDelay")
	}
	e.now = e.now.Add(11 * time.Second)
	if !e.process(t) {
		t.Fatal("waiting job did not run")
	}
	if got := e.job(t, res.ID).GetString("state"); got != StateWaiting {
		t.Fatalf("still out of paper, state=%s", got)
	}
	out.Store(false)
	e.now = e.now.Add(11 * time.Second)
	e.process(t)
	r = e.job(t, res.ID)
	if r.GetString("state") != StateDone || r.GetString("printed_at") == "" {
		t.Fatalf("state=%s", r.GetString("state"))
	}
	if sc.payloadCount() != 1 {
		t.Fatalf("payload written %d times", sc.payloadCount())
	}
	p := sc.payloads[0]
	if !bytes.Contains(p, []byte("T-0001")) || !bytes.Contains(p, []byte{escpos.GS, 'V'}) {
		t.Fatalf("payload lacks the QR data or the cut: %q", p)
	}
	if r.GetInt("attempts") != 3 {
		t.Fatalf("attempts = %d", r.GetInt("attempts"))
	}
	if e.audits(AuditJob) != 1 {
		t.Fatalf("audit print.job = %d", e.audits(AuditJob))
	}
}

func TestNoStatusReplyStillPrints(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return nil }}
	e.pm.Open = sc.open
	e.addPrinter(t, "cheap", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "hello {{.x}}")
	res, err := e.pm.Enqueue(context.Background(), Request{Printer: "cheap", Template: "t", Data: map[string]any{"x": "w"}, Copies: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.process(t)
	if got := e.job(t, res.ID).GetString("state"); got != StateDone {
		t.Fatalf("state=%s", got)
	}
	if sc.payloadCount() != 2 {
		t.Fatalf("copies written = %d", sc.payloadCount())
	}
}

func TestPrinterMutexRace(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return readyReply }, hold: 5 * time.Millisecond}
	e.pm.Open = sc.open
	e.addPrinter(t, "a", "tcp", "127.0.0.1:9100", nil)
	e.addPrinter(t, "b", "tcp", "127.0.0.1:9101", nil)
	e.addTemplate(t, "t", "x")
	var ids []string
	for i := 0; i < 8; i++ {
		res, err := e.pm.Enqueue(context.Background(), Request{Printer: "a", Template: "t"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.ID)
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]string{"id": id})
			if err := e.pm.handle(context.Background(), e.app, &kernel.Job{Payload: payload, Attempt: 1, MaxAttempts: 20}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if sc.maxIn != 1 {
		t.Fatalf("up to %d concurrent connections to one printer", sc.maxIn)
	}
	if sc.payloadCount() != 8 {
		t.Fatalf("payloads = %d", sc.payloadCount())
	}
	// a done job is not printed twice when the queue delivers it again
	payload, _ := json.Marshal(map[string]string{"id": ids[0]})
	_ = e.pm.handle(context.Background(), e.app, &kernel.Job{Payload: payload, Attempt: 1, MaxAttempts: 20})
	if sc.payloadCount() != 8 {
		t.Fatal("a done job was printed again")
	}
}

func TestCIDRPolicy(t *testing.T) {
	e := setup(t)
	check := func(name, addr string, wantErr bool) {
		t.Helper()
		err := validatePrinter(&Printer{Name: name, Transport: "tcp", Address: addr})
		if (err != nil) != wantErr {
			t.Errorf("%s: err=%v wantErr=%v", addr, err, wantErr)
		}
	}
	check("a", "192.168.1.50:9100", false)
	check("b", "10.0.0.5:9100", false)
	check("c", "127.0.0.1:9100", false)
	check("d", "8.8.8.8:9100", true)
	check("e", "169.254.169.254:80", true)
	check("f", "192.168.1.50", true)
	t.Setenv("TOKI_PRINT_ALLOW_CIDRS", "10.0.0.0/8")
	check("g", "192.168.1.50:9100", true)
	check("h", "10.1.2.3:9100", false)

	// the dial path enforces it too, whatever the stored row says
	_, err := e.pm.openTransport(context.Background(), &Printer{Name: "x", Transport: "tcp", Address: "8.8.8.8:9100", TimeoutMs: 300})
	if !errors.Is(err, devio.ErrDenied) {
		t.Fatalf("dial of a public address: %v", err)
	}
	_, err = e.pm.openTransport(context.Background(), &Printer{Name: "x", Transport: "file", Address: "/etc/passwd", TimeoutMs: 300})
	if !errors.Is(err, devio.ErrDenied) {
		t.Fatalf("open of a regular file: %v", err)
	}
	// saving a denied printer through the record API fails validation
	col, _ := e.app.FindCachedCollectionByNameOrId(PrintersCollection)
	r := core.NewRecord(col)
	r.Set("name", "bad")
	r.Set("transport", "tcp")
	r.Set("address", "8.8.4.4:9100")
	if err := e.app.Save(r); err == nil {
		t.Fatal("a printer outside the allowed CIDRs was saved")
	}
}

func TestIdempotencyDedupe(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return readyReply }}
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	var wg sync.WaitGroup
	ids := make([]string, 6)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.pm.Enqueue(context.Background(), Request{Printer: "p", Template: "t", IdempotencyKey: "order-7"})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = res.ID
		}()
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("ids differ: %v", ids)
		}
	}
	n, _ := e.app.CountRecords(JobsCollection)
	if n != 1 {
		t.Fatalf("%d jobs for one idempotency key", n)
	}
	st, _ := kernel.Jobs(e.app).Stats(context.Background())
	if st.Queued != 1 {
		t.Fatalf("%d queued kernel jobs", st.Queued)
	}
	res, _ := e.pm.Enqueue(context.Background(), Request{Printer: "p", Template: "t", IdempotencyKey: "order-7"})
	if !res.Duplicate {
		t.Fatal("repeat must be flagged as duplicate")
	}
}

func TestRenderedAtEnqueueAndTemplateVersion(t *testing.T) {
	e := setup(t)
	sc := &script{reply: func() []byte { return readyReply }}
	e.pm.Open = sc.open
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", map[string]any{"cut": true, "drawer": true})
	tpl := e.addTemplate(t, "t", "v1 {{.a}}")
	if tpl.GetInt("version") != 1 {
		t.Fatalf("version = %d", tpl.GetInt("version"))
	}
	res, err := e.pm.Enqueue(context.Background(), Request{Printer: "p", Template: "t", Data: map[string]any{"a": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	tpl.Set("body", "v2 {{.a}}")
	if err := e.app.Save(tpl); err != nil {
		t.Fatal(err)
	}
	if tpl.GetInt("version") != 2 {
		t.Fatalf("version after save = %d", tpl.GetInt("version"))
	}
	e.process(t)
	if sc.payloadCount() != 1 || !bytes.Contains(sc.payloads[0], []byte("v1 x")) || bytes.Contains(sc.payloads[0], []byte("v2")) {
		t.Fatalf("a later template edit changed the queued print: %q", sc.payloads)
	}
	p := sc.payloads[0]
	if !bytes.Contains(p, []byte{escpos.GS, 'V'}) || !bytes.Contains(p, []byte{escpos.ESC, 'p'}) {
		t.Fatal("printer cut/drawer defaults were not applied")
	}
	r := e.job(t, res.ID)
	if r.GetInt("template_version") != 1 {
		t.Fatalf("template_version = %d", r.GetInt("template_version"))
	}
}

func TestEnqueueErrors(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addPrinter(t, "q", "tcp", "127.0.0.1:9101", map[string]any{"enabled": false})
	e.addTemplate(t, "t", "x")
	e.addTemplate(t, "bad", "{{call .x}}")
	cases := []struct {
		name string
		rq   Request
		code int
	}{
		{"missing template", Request{Printer: "p", Template: "nope"}, 404},
		{"missing printer", Request{Printer: "zzz", Template: "t"}, 404},
		{"disabled printer", Request{Printer: "q", Template: "t"}, 400},
		{"no template", Request{Printer: "p"}, 400},
		{"too many copies", Request{Printer: "p", Template: "t", Copies: 99}, 400},
		{"forbidden func", Request{Printer: "p", Template: "bad"}, 400},
		{"raw over limit", Request{Printer: "p", Raw: make([]byte, MaxBytes()+1)}, 400},
	}
	for _, c := range cases {
		_, err := e.pm.Enqueue(context.Background(), c.rq)
		var re *RequestError
		if !errors.As(err, &re) || re.Status != c.code {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// exactly one enabled printer is the implicit default
	if _, err := e.pm.Enqueue(context.Background(), Request{Template: "t"}); err != nil {
		t.Errorf("implicit printer: %v", err)
	}
}

func TestSyncReplicaGuard(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "p", "tcp", "127.0.0.1:9100", nil)
	e.addTemplate(t, "t", "x")
	for _, mode := range []kernel.SyncApplyMode{kernel.SyncModePull, kernel.SyncModeSnapshot, kernel.SyncModeBundle} {
		ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: mode})
		if _, err := e.pm.Enqueue(ctx, Request{Printer: "p", Template: "t"}); !errors.Is(err, ErrSyncReplica) {
			t.Fatalf("mode %d: %v", mode, err)
		}
		// a row applied by sync is refused at the model level as well
		col, _ := e.app.FindCachedCollectionByNameOrId(JobsCollection)
		r := core.NewRecord(col)
		r.Set("printer", "p")
		r.Set("state", StateQueued)
		if err := e.app.SaveWithContext(ctx, r); !errors.Is(err, ErrSyncReplica) {
			t.Fatalf("mode %d: SaveWithContext = %v", mode, err)
		}
	}
	if n, _ := e.app.CountRecords(JobsCollection); n != 0 {
		t.Fatalf("%d jobs were created by sync applies", n)
	}
	st, _ := kernel.Jobs(e.app).Stats(context.Background())
	if st.Queued != 0 {
		t.Fatal("a kernel job was queued by a sync apply")
	}
	// a push replay on the hub is a normal local request of the actor
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush})
	if _, err := e.pm.Enqueue(ctx, Request{Printer: "p", Template: "t"}); err != nil {
		t.Fatalf("push: %v", err)
	}
}

func TestPrune(t *testing.T) {
	e := setup(t)
	col, _ := e.app.FindCachedCollectionByNameOrId(JobsCollection)
	mk := func(state string) string {
		r := core.NewRecord(col)
		r.Set("printer", "p")
		r.Set("state", state)
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
		return r.Id
	}
	oldDone, newDone, oldDead := mk(StateDone), mk(StateDone), mk(StateDead)
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(timeLayout)
	for _, id := range []string{oldDone, oldDead} {
		if _, err := e.app.DB().NewQuery(`UPDATE {{_print_jobs}} SET [[updated]]={:u} WHERE [[id]]={:i}`).
			Bind(map[string]any{"u": old, "i": id}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Prune(e.app, time.Now(), 14*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if _, err := e.app.FindRecordById(JobsCollection, oldDone); err == nil {
		t.Fatal("old done job survived")
	}
	for _, id := range []string{newDone, oldDead} {
		if _, err := e.app.FindRecordById(JobsCollection, id); err != nil {
			t.Fatalf("job %s was pruned", id)
		}
	}
}

// ---- TCP stub printer ------------------------------------------------------

type behavior struct {
	dropAfter int  // close after this many payload bytes (0 = never)
	closeNow  bool // close right after accept
	silent    bool // never answer a status query
	paperOut  bool
}

type stubPrinter struct {
	ln    net.Listener
	mu    sync.Mutex
	got   []byte
	conns int
	next  func(n int) behavior
	wg    sync.WaitGroup
}

func newStub(t *testing.T, next func(n int) behavior) *stubPrinter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stubPrinter{ln: ln, next: next}
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			n := s.conns
			s.mu.Unlock()
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.serve(c, s.next(n)) }()
		}
	}()
	return s
}

func (s *stubPrinter) addr() string { return s.ln.Addr().String() }

func (s *stubPrinter) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte{}, s.got...)
}

func (s *stubPrinter) connCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *stubPrinter) serve(c net.Conn, b behavior) {
	defer c.Close()
	if b.closeNow {
		return
	}
	q := escpos.StatusQuery()
	var pending []byte
	total := 0
	record := func(p []byte) {
		s.mu.Lock()
		s.got = append(s.got, p...)
		s.mu.Unlock()
		total += len(p)
	}
	buf := make([]byte, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, err := c.Read(buf)
		pending = append(pending, buf[:n]...)
		for {
			i := bytes.Index(pending, q)
			if i < 0 {
				break
			}
			record(pending[:i])
			pending = pending[i+len(q):]
			if b.dropAfter > 0 && total >= b.dropAfter {
				return // the connection dies before it can answer the post-write status query
			}
			if !b.silent {
				reply := readyReply
				if b.paperOut {
					reply = paperOut
				}
				_, _ = c.Write(reply)
			}
		}
		if len(pending) >= len(q) {
			record(pending[:len(pending)-len(q)+1])
			pending = pending[len(pending)-len(q)+1:]
		}
		if b.dropAfter > 0 && total+len(pending) >= b.dropAfter {
			return // drop mid-write
		}
		if err != nil {
			record(pending)
			return
		}
	}
}

func TestTCPStubPrintAndRetry(t *testing.T) {
	e := setup(t)
	stub := newStub(t, func(n int) behavior {
		if n == 1 {
			return behavior{dropAfter: 20} // the first connection dies mid-write
		}
		return behavior{}
	})
	e.addPrinter(t, "net", "tcp", stub.addr(), map[string]any{"qr_native": true})
	e.addTemplate(t, "ticket", ticketTpl)
	res, err := e.pm.Enqueue(context.Background(), Request{Printer: "net", Template: "ticket", Data: map[string]any{"plate": "D 9 ZZ", "id": "QR-PAYLOAD-42"}})
	if err != nil {
		t.Fatal(err)
	}
	e.process(t)
	r := e.job(t, res.ID)
	if r.GetString("state") != StateFailed || r.GetString("last_error") == "" {
		t.Fatalf("after a dropped connection: state=%s err=%q", r.GetString("state"), r.GetString("last_error"))
	}
	e.now = e.now.Add(2 * time.Minute) // past the queue backoff
	e.process(t)
	r = e.job(t, res.ID)
	if r.GetString("state") != StateDone || r.GetInt("attempts") != 2 {
		t.Fatalf("state=%s attempts=%d err=%q", r.GetString("state"), r.GetInt("attempts"), r.GetString("last_error"))
	}
	got := stub.bytes()
	if !bytes.Contains(got, []byte("QR-PAYLOAD-42")) || !bytes.Contains(got, []byte{escpos.GS, 'V', 66, 0}) {
		t.Fatalf("captured bytes lack the QR payload or the cut: %q", got)
	}
}

func TestTCPStubDeadLetter(t *testing.T) {
	e := setup(t)
	stub := newStub(t, func(int) behavior { return behavior{closeNow: true} })
	e.addPrinter(t, "net", "tcp", stub.addr(), nil)
	e.addTemplate(t, "t", "x")
	res, err := e.pm.Enqueue(context.Background(), Request{Printer: "net", Template: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxAttempts; i++ {
		if !e.process(t) {
			t.Fatalf("attempt %d: nothing to process", i+1)
		}
		e.now = e.now.Add(2 * time.Hour) // past the 1 h backoff cap
	}
	r := e.job(t, res.ID)
	if r.GetString("state") != StateDead || r.GetInt("attempts") != MaxAttempts {
		t.Fatalf("state=%s attempts=%d", r.GetString("state"), r.GetInt("attempts"))
	}
	if e.audits(AuditDead) != 1 {
		t.Fatalf("audit print.dead = %d", e.audits(AuditDead))
	}
	if e.process(t) {
		t.Fatal("a dead job must not run again")
	}
	// manual retry: a new queue job, the same print row
	if _, err := e.pm.Retry(context.Background(), res.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.job(t, res.ID).GetString("state"); got != StateQueued {
		t.Fatalf("after retry state=%s", got)
	}
	if _, err := e.pm.Retry(context.Background(), res.ID); err == nil {
		t.Fatal("a queued job cannot be retried")
	}
}

func TestTCPStubSilentStatusStillPrints(t *testing.T) {
	e := setup(t)
	stub := newStub(t, func(int) behavior { return behavior{silent: true} })
	e.addPrinter(t, "net", "tcp", stub.addr(), nil)
	e.addTemplate(t, "t", "hello")
	res, _ := e.pm.Enqueue(context.Background(), Request{Printer: "net", Template: "t"})
	e.process(t)
	if got := e.job(t, res.ID).GetString("state"); got != StateDone {
		t.Fatalf("state=%s", got)
	}
	if !bytes.Contains(stub.bytes(), []byte("hello")) {
		t.Fatal("payload not received")
	}
}

func TestTCPStubPaperOut(t *testing.T) {
	e := setup(t)
	stub := newStub(t, func(int) behavior { return behavior{paperOut: true} })
	e.addPrinter(t, "net", "tcp", stub.addr(), nil)
	e.addTemplate(t, "t", "hello")
	res, _ := e.pm.Enqueue(context.Background(), Request{Printer: "net", Template: "t"})
	e.process(t)
	if got := e.job(t, res.ID).GetString("state"); got != StateWaiting {
		t.Fatalf("state=%s", got)
	}
	if len(stub.bytes()) != 0 {
		t.Fatal("bytes were sent to a printer without paper")
	}
	if st := e.pm.LastStatus("net"); st.State != "paper" {
		t.Fatalf("last status %+v", st)
	}
}

// ---- HTTP ------------------------------------------------------------------

func (e *env) mux(t *testing.T) http.Handler {
	t.Helper()
	router, err := apis.NewRouter(e.app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = e.app.OnServe().Trigger(&core.ServeEvent{App: e.app, Router: router}, func(se *core.ServeEvent) error {
		mux, err := se.Router.BuildMux()
		h = mux
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func (e *env) users(t *testing.T) (user, other, super string) {
	t.Helper()
	mc := core.NewAuthCollection("members")
	if err := e.app.Save(mc); err != nil {
		t.Fatal(err)
	}
	tok := func(email string) string {
		r := core.NewRecord(mc)
		r.SetEmail(email)
		r.SetPassword("password12345")
		if err := e.app.Save(r); err != nil {
			t.Fatal(err)
		}
		s, _ := r.NewAuthToken()
		return s
	}
	su, err := e.app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := su.NewAuthToken()
	return tok("a@example.com"), tok("b@example.com"), st
}

func TestHTTPAPI(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", map[string]any{"default": true})
	e.addPrinter(t, "hidden", "tcp", "10.1.2.4:9100", map[string]any{"enabled": false})
	e.addTemplate(t, "t", "hi {{.n}}")
	h := e.mux(t)
	user, other, super := e.users(t)

	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, ""); rec.Code != 401 {
		t.Fatalf("guest: %d", rec.Code)
	}
	rec := do(h, "POST", "/api/print", `{"template":"t","data":{"n":"x"},"idempotency_key":"k1"}`, user)
	if rec.Code != 200 {
		t.Fatalf("print: %d %s", rec.Code, rec.Body)
	}
	var res Result
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.ID == "" || res.State != StateQueued {
		t.Fatalf("result %+v", res)
	}
	var dup Result
	rec = do(h, "POST", "/api/print", `{"template":"t","idempotency_key":"k1"}`, user)
	_ = json.Unmarshal(rec.Body.Bytes(), &dup)
	if rec.Code != 200 || dup.ID != res.ID || !dup.Duplicate {
		t.Fatalf("dedupe: %d %s", rec.Code, rec.Body)
	}
	if rec = do(h, "GET", "/api/print/"+res.ID, "", user); rec.Code != 200 || strings.Contains(rec.Body.String(), "payload") {
		t.Fatalf("get own: %d %s", rec.Code, rec.Body)
	}
	if rec = do(h, "GET", "/api/print/"+res.ID, "", other); rec.Code != 404 {
		t.Fatalf("get other: %d", rec.Code)
	}
	if rec = do(h, "GET", "/api/print/"+res.ID, "", super); rec.Code != 200 {
		t.Fatalf("get super: %d", rec.Code)
	}
	raw := base64.StdEncoding.EncodeToString([]byte("\x1b@RAW\n"))
	if rec = do(h, "POST", "/api/print", `{"raw_b64":"`+raw+`"}`, user); rec.Code != 403 {
		t.Fatalf("raw as user: %d", rec.Code)
	}
	if rec = do(h, "POST", "/api/print", `{"raw_b64":"`+raw+`"}`, super); rec.Code != 200 {
		t.Fatalf("raw as superuser: %d %s", rec.Code, rec.Body)
	}
	if rec = do(h, "POST", "/api/print", `{"template":"nope"}`, user); rec.Code != 404 {
		t.Fatalf("unknown template: %d", rec.Code)
	}

	rec = do(h, "GET", "/api/print/printers", "", user)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "10.1.2") || strings.Contains(rec.Body.String(), "hidden") {
		t.Fatalf("printers as user leaks addresses or disabled printers: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"queue_depth":2`) {
		t.Fatalf("queue depth: %s", rec.Body)
	}
	if rec = do(h, "GET", "/api/print/printers", "", super); !strings.Contains(rec.Body.String(), "10.1.2.3:9100") {
		t.Fatalf("printers as superuser: %s", rec.Body)
	}

	if rec = do(h, "POST", "/api/print/"+res.ID+"/retry", "", user); rec.Code != 409 {
		t.Fatalf("retry of a queued job: %d", rec.Code)
	}
	r := e.job(t, res.ID)
	r.Set("state", StateDead)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	if rec = do(h, "POST", "/api/print/"+res.ID+"/retry", "", other); rec.Code != 404 {
		t.Fatalf("retry of another actor's job: %d", rec.Code)
	}
	if rec = do(h, "POST", "/api/print/"+res.ID+"/retry", "", user); rec.Code != 200 {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body)
	}
}

func TestHTTPSuperuserOnlyMode(t *testing.T) {
	t.Setenv("TOKI_PRINT_AUTH", "superuser")
	e := setup(t)
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", map[string]any{"default": true})
	e.addTemplate(t, "t", "hi")
	h := e.mux(t)
	user, _, super := e.users(t)
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, user); rec.Code != 403 {
		t.Fatalf("user: %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/print", `{"template":"t"}`, super); rec.Code != 200 {
		t.Fatalf("superuser: %d", rec.Code)
	}
}

func TestHealthBlock(t *testing.T) {
	e := setup(t)
	e.addPrinter(t, "counter", "tcp", "10.1.2.3:9100", nil)
	b, _ := json.Marshal(e.pm.Health())
	if !strings.Contains(string(b), `"counter"`) || !strings.Contains(string(b), `"queue"`) || strings.Contains(string(b), "10.1.2.3") {
		t.Fatalf("health: %s", b)
	}
}

func TestTestPage(t *testing.T) {
	p := &Printer{Name: "x", Cols: 32, QRNative: true}
	page, err := testPage(p, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte("tokibase-print-test:x")) || !bytes.Contains(page, []byte{escpos.GS, 'V'}) {
		t.Fatalf("test page: %q", page)
	}
}
