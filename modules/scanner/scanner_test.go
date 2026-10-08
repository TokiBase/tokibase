//go:build !no_scanner

package scanner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	app *tests.TestApp
	m   *Module
	clk *clock
	srv *httptest.Server
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)
	clk := &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	m.now = clk.now

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		var err error
		h, err = se.Router.BuildMux()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{app: app, m: m, clk: clk, srv: srv}
}

func (e *env) token(t *testing.T, collection string) string {
	t.Helper()
	r, err := e.app.FindAuthRecordByEmail(collection, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := r.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *env) post(t *testing.T, tok string, body map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/scan", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func (e *env) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := e.app.DB().NewQuery("SELECT COUNT(*) FROM _scan_events WHERE " + where).Row(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) saveScanner(t *testing.T, vals map[string]any) *core.Record {
	t.Helper()
	col, err := e.app.FindCollectionByNameOrId(ConfigCollection)
	if err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(col)
	for k, v := range vals {
		r.Set(k, v)
	}
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func testScanner(kind string) *Scanner {
	s := &Scanner{Name: "gate", Kind: kind, Enabled: true, DedupeMs: -1}
	s.applyDefaults()
	return s
}

func TestCleanFilters(t *testing.T) {
	cases := []struct {
		sc     Scanner
		in     string
		want   string
		reason string
	}{
		{Scanner{Prefix: "]Q1", Suffix: "!"}, "]Q1AB-12!\r", "AB-12", ""},
		{Scanner{MinLen: 5}, "abcd", "", "too_short"},
		{Scanner{MaxLen: 4}, "abcde", "", "too_long"},
		{Scanner{Charset: `^[0-9]+$`}, "12a4", "", "charset"},
		{Scanner{Charset: `^[0-9]+$`}, "1234", "1234", ""},
		{Scanner{}, "a\x01b", "", "control_char"},
		{Scanner{}, " \r\n", "", "empty"},
		{Scanner{Prefix: "X"}, "X", "", "empty"},
	}
	for i, c := range cases {
		sc := c.sc
		sc.applyDefaults()
		got, reason := sc.Clean(c.in)
		if got != c.want || reason != c.reason {
			t.Errorf("case %d: got %q/%q want %q/%q", i, got, reason, c.want, c.reason)
		}
	}
}

func TestValidate(t *testing.T) {
	bad := []Scanner{
		{Name: "a b", Kind: KindWeb},
		{Name: "x", Kind: "usb"},
		{Name: "x", Kind: KindSerial},
		{Name: "x", Kind: KindSerial, Device: "/etc/passwd"},
		{Name: "x", Kind: KindSerial, Device: "/dev/../etc/passwd"},
		{Name: "x", Kind: KindSerial, Device: "/dev/ttyUSB0", Baud: 1234},
		{Name: "x", Kind: KindWeb, Charset: "("},
		{Name: "x", Kind: KindWeb, Terminator: "tab"},
		{Name: "x", Kind: KindWeb, MinLen: 9, MaxLen: 3},
		{Name: "x", Kind: KindEvdev, Device: "/dev/input/event0", Layout: "de"},
	}
	for i, s := range bad {
		s.applyDefaults()
		if err := s.Validate(); err == nil {
			t.Errorf("case %d: %+v must be invalid", i, s)
		}
	}
	ok := Scanner{Name: "door-1", Kind: KindSerial, Device: "/dev/serial/by-id/usb-x", Baud: 115200}
	ok.applyDefaults()
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigCollectionValidatesRows(t *testing.T) {
	e := setup(t)
	col, _ := e.app.FindCollectionByNameOrId(ConfigCollection)
	r := core.NewRecord(col)
	r.Set("name", "x")
	r.Set("kind", KindSerial) // no device
	if err := e.app.Save(r); err == nil {
		t.Fatal("serial without device must be rejected")
	}
	e.saveScanner(t, map[string]any{"name": "ok", "kind": KindWeb, "enabled": true})
	if col.ListRule != nil || col.CreateRule != nil || !col.System {
		t.Fatal("_scanners must be a system collection with null rules")
	}
}

func TestDedupeWindowInjectedClock(t *testing.T) {
	e := setup(t)
	sc := defaultWeb()
	ctx := context.Background()
	r1, err := e.m.Ingest(ctx, sc, "ABC123", IngestOptions{Source: "web"})
	if err != nil || r1.Duplicate {
		t.Fatalf("first: %v %+v", err, r1)
	}
	e.clk.add(800 * time.Millisecond)
	r2, _ := e.m.Ingest(ctx, sc, "ABC123", IngestOptions{Source: "web"})
	e.clk.add(600 * time.Millisecond) // 1.4 s after the first: still inside 1500 ms
	r3, _ := e.m.Ingest(ctx, sc, "ABC123", IngestOptions{Source: "web"})
	if !r2.Duplicate || !r3.Duplicate || r2.ID != r1.ID || r3.DupCount != 2 {
		t.Fatalf("r2=%+v r3=%+v", r2, r3)
	}
	if n := e.count(t, "1=1"); n != 1 {
		t.Fatalf("%d events inside the window", n)
	}
	var dups int
	_ = e.app.DB().NewQuery("SELECT dup_count FROM _scan_events WHERE id={:id}").Bind(map[string]any{"id": r1.ID}).Row(&dups)
	if dups != 2 {
		t.Fatalf("dup_count %d", dups)
	}
	e.clk.add(200 * time.Millisecond) // 1.6 s: outside
	r4, _ := e.m.Ingest(ctx, sc, "ABC123", IngestOptions{Source: "web"})
	if r4.Duplicate || r4.ID == r1.ID {
		t.Fatalf("r4=%+v", r4)
	}
	// another scanner or another code is never a duplicate
	other := *sc
	other.Name = "other"
	r5, _ := e.m.Ingest(ctx, &other, "ABC123", IngestOptions{})
	r6, _ := e.m.Ingest(ctx, sc, "ABC124", IngestOptions{})
	if r5.Duplicate || r6.Duplicate {
		t.Fatal("different scanner/code must not dedupe")
	}
	if n := e.count(t, "1=1"); n != 4 {
		t.Fatalf("%d events", n)
	}
}

func TestDedupeDisabled(t *testing.T) {
	e := setup(t)
	sc := testScanner(KindWeb)
	for i := 0; i < 3; i++ {
		if r, _ := e.m.Ingest(context.Background(), sc, "SAME", IngestOptions{}); r.Duplicate {
			t.Fatal("dedupe_ms < 0 disables de-duplication")
		}
	}
	if n := e.count(t, "1=1"); n != 3 {
		t.Fatalf("%d", n)
	}
}

func TestClientSeqIdempotent(t *testing.T) {
	e := setup(t)
	sc := testScanner(KindWeb)
	a, _ := e.m.Ingest(context.Background(), sc, "X1234", IngestOptions{Actor: "users/a", ClientSeq: "7"})
	e.clk.add(5 * time.Minute) // far outside any dedupe window, inside the 10 minute retry memory
	b, _ := e.m.Ingest(context.Background(), sc, "X1234", IngestOptions{Actor: "users/a", ClientSeq: "7"})
	c, _ := e.m.Ingest(context.Background(), sc, "X1234", IngestOptions{Actor: "users/b", ClientSeq: "7"})
	if !b.Duplicate || b.ID != a.ID || c.Duplicate {
		t.Fatalf("a=%+v b=%+v c=%+v", a, b, c)
	}
	if n := e.count(t, "1=1"); n != 2 {
		t.Fatalf("%d events", n)
	}
}

func TestSyncReplicaGuard(t *testing.T) {
	e := setup(t)
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePull})
	if _, err := e.m.Ingest(ctx, defaultWeb(), "NOPE1", IngestOptions{}); err != ErrReplica {
		t.Fatalf("got %v", err)
	}
	if n := e.count(t, "1=1"); n != 0 {
		t.Fatal("a replica apply must not create a scan")
	}
	// a push apply is a normal local write path, not a replica
	ctx = kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModePush})
	if _, err := e.m.Ingest(ctx, defaultWeb(), "OK123", IngestOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIScanDedupeAndAuth(t *testing.T) {
	e := setup(t)
	tok := e.token(t, "users")
	if st, _ := e.post(t, "", map[string]any{"code": "ZZZ999"}); st != 401 && st != 400 {
		t.Fatalf("guest got %d", st)
	}
	st, r1 := e.post(t, tok, map[string]any{"code": "ZZZ999"})
	if st != 200 || r1["duplicate"] != false || r1["symbology"] != "unknown" {
		t.Fatalf("%d %v", st, r1)
	}
	e.clk.add(500 * time.Millisecond)
	st, r2 := e.post(t, tok, map[string]any{"code": "ZZZ999"})
	if st != 200 || r2["duplicate"] != true || r2["id"] != r1["id"] {
		t.Fatalf("%d %v", st, r2)
	}
	if n := e.count(t, "1=1"); n != 1 {
		t.Fatalf("%d events inside the window", n)
	}
	e.clk.add(2 * time.Second)
	st, r3 := e.post(t, tok, map[string]any{"code": "ZZZ999"})
	if st != 200 || r3["duplicate"] != false || r3["id"] == r1["id"] {
		t.Fatalf("%d %v", st, r3)
	}
	if n := e.count(t, "1=1"); n != 2 {
		t.Fatalf("%d events outside the window", n)
	}
	// the actor is the authenticated record
	var actor string
	_ = e.app.DB().NewQuery("SELECT actor FROM _scan_events LIMIT 1").Row(&actor)
	if !strings.HasPrefix(actor, "users/") {
		t.Fatalf("actor %q", actor)
	}
	// numeric client_seq retry
	_, a := e.post(t, tok, map[string]any{"code": "SEQ0001", "client_seq": 5})
	e.clk.add(time.Minute)
	_, b := e.post(t, tok, map[string]any{"code": "SEQ0001", "client_seq": 5})
	if a["id"] != b["id"] || b["duplicate"] != true {
		t.Fatalf("%v %v", a, b)
	}
	if st, o := e.post(t, tok, map[string]any{"code": "EAN", "symbology": "ean13", "scanner": "nope"}); st != 404 {
		t.Fatalf("unknown scanner: %d %v", st, o)
	}
	if st, _ := e.post(t, tok, map[string]any{"code": ""}); st != 400 {
		t.Fatalf("empty code: %d", st)
	}
}

func TestAPIFiltersUseConfiguredWebScanner(t *testing.T) {
	e := setup(t)
	tok := e.token(t, "users")
	e.saveScanner(t, map[string]any{"name": "kiosk", "kind": KindWeb, "enabled": true, "min_len": 6, "charset": `^[A-Z0-9]+$`, "prefix": "T-"})
	e.saveScanner(t, map[string]any{"name": "belt", "kind": KindSerial, "device": "/dev/ttyUSB9", "enabled": false})
	st, o := e.post(t, tok, map[string]any{"code": "AB"})
	if st != 400 || o["data"].(map[string]any)["reason"] != "too_short" {
		t.Fatalf("%d %v", st, o)
	}
	st, o = e.post(t, tok, map[string]any{"code": "T-abcdef"})
	if st != 400 || o["data"].(map[string]any)["reason"] != "charset" {
		t.Fatalf("%d %v", st, o)
	}
	st, o = e.post(t, tok, map[string]any{"code": "T-ABC123", "scanner": "kiosk"})
	if st != 200 || o["code"] != "ABC123" || o["scanner"] != "kiosk" {
		t.Fatalf("%d %v", st, o)
	}
	if st, _ := e.post(t, tok, map[string]any{"code": "ABC123", "scanner": "belt"}); st != 409 {
		t.Fatalf("disabled scanner: %d", st)
	}
	if n := e.count(t, "scanner='kiosk'"); n != 1 {
		t.Fatalf("%d", n)
	}
}

func (e *env) get(t *testing.T, tok, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	if tok != "" {
		req.Header.Set("Authorization", tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestEventsSinceCatchUp(t *testing.T) {
	e := setup(t)
	tok := e.token(t, "users")
	var ids []string
	for _, c := range []string{"C0001", "C0002", "C0003", "C0004"} {
		e.clk.add(5 * time.Second)
		_, r := e.post(t, tok, map[string]any{"code": c})
		ids = append(ids, r["id"].(string))
	}
	if st, _ := e.get(t, "", "/api/scan/events"); st != 401 {
		t.Fatalf("guest got %d", st)
	}
	st, o := e.get(t, tok, "/api/scan/events?since="+ids[1])
	items := o["items"].([]any)
	if st != 200 || len(items) != 2 || items[0].(map[string]any)["code"] != "C0003" || o["gap"] != false {
		t.Fatalf("%d %v", st, o)
	}
	_, o = e.get(t, tok, "/api/scan/events?limit=2")
	items = o["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["code"] != "C0003" || items[1].(map[string]any)["code"] != "C0004" {
		t.Fatalf("latest two oldest-first: %v", o)
	}
	_, o = e.get(t, tok, "/api/scan/events?since=gone")
	if o["gap"] != true || len(o["items"].([]any)) != 4 {
		t.Fatalf("unknown since: %v", o)
	}
	if _, o = e.get(t, tok, "/api/scan/events?since="+ids[3]); len(o["items"].([]any)) != 0 {
		t.Fatalf("nothing after the last: %v", o)
	}
}

func TestPruneRetention(t *testing.T) {
	e := setup(t)
	t.Setenv("TOKI_SCAN_RETENTION_HOURS", "2")
	sc := testScanner(KindWeb)
	_, _ = e.m.Ingest(context.Background(), sc, "OLD001", IngestOptions{})
	e.clk.add(3 * time.Hour)
	_, _ = e.m.Ingest(context.Background(), sc, "NEW001", IngestOptions{})
	n, err := e.m.Prune()
	if err != nil || n != 1 {
		t.Fatalf("pruned %d %v", n, err)
	}
	if c := e.count(t, "code='NEW001'"); c != 1 {
		t.Fatal("the recent event must stay")
	}
}

func TestWedgeJSServed(t *testing.T) {
	e := setup(t)
	res, err := http.Get(e.srv.URL + "/scan/wedge.js")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(res.Header.Get("Content-Type"), "javascript") ||
		!strings.Contains(string(b), "TokiScan") || len(strings.Split(string(b), "\n")) > 60 {
		t.Fatalf("%d %s %d bytes", res.StatusCode, res.Header.Get("Content-Type"), len(b))
	}
}

// ---- serial / evdev readers ---------------------------------------------

func waitEvents(t *testing.T, e *env, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.count(t, "1=1") >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d events, have %d", n, e.count(t, "1=1"))
}

func codes(t *testing.T, e *env) []string {
	t.Helper()
	items, _, err := e.m.Events("", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Code)
	}
	return out
}

func TestSerialSplitterWithPipe(t *testing.T) {
	e := setup(t)
	pr, pw := io.Pipe()
	sc := testScanner(KindSerial)
	sc.Prefix, sc.Suffix, sc.MaxLen = "<", ">", 20
	var rejected, accepted int
	done := make(chan error, 1)
	go func() {
		done <- e.m.readScans(context.Background(), sc, pr, 0, func(_ string, rj bool) {
			if rj {
				rejected++
			} else {
				accepted++
			}
		})
	}()
	long := strings.Repeat("Z", 500)
	// CR, LF, CRLF, empty lines, a partial write split across two writes, an over-long line
	for _, chunk := range []string{"<AAA111>\r", "<BBB2", "22>\n", "\r\n\n", "<CCC333>\r\n", long + "\r", "<DDD444>\n", "toolongtoolongtoolongtoolong\n"} {
		if _, err := pw.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	pw.Close()
	if err := <-done; err != io.EOF {
		t.Fatalf("got %v", err)
	}
	got := strings.Join(codes(t, e), ",")
	if got != "AAA111,BBB222,CCC333,DDD444" {
		t.Fatalf("codes %q", got)
	}
	if accepted != 4 || rejected != 2 {
		t.Fatalf("accepted %d rejected %d", accepted, rejected)
	}
}

func evdevStream(size int, evs ...[2]int32) []byte {
	var b bytes.Buffer
	for _, ev := range evs {
		rec := make([]byte, size)
		off := size - 8
		binary.LittleEndian.PutUint16(rec[off:], 1) // EV_KEY
		binary.LittleEndian.PutUint16(rec[off+2:], uint16(ev[0]))
		binary.LittleEndian.PutUint32(rec[off+4:], uint32(ev[1]))
		b.Write(rec)
		syn := make([]byte, size) // EV_SYN, ignored
		b.Write(syn)
	}
	return b.Bytes()
}

func press(code int32) [][2]int32 { return [][2]int32{{code, 1}, {code, 0}} }

func TestEvdevFixtures16And24(t *testing.T) {
	const (
		kA, k1, k2, kB, kEnter, kShift, kCaps, kSlash = 30, 2, 3, 48, 28, 42, 58, 53
	)
	var evs [][2]int32
	add := func(e ...[2]int32) { evs = append(evs, e...) }
	add([2]int32{kShift, 1})
	add(press(kA)...)
	add([2]int32{kShift, 0})
	add(press(k1)...)
	add(press(k2)...)
	add([2]int32{k2, 2}) // key repeat is ignored
	add(press(kEnter)...)
	add(press(kCaps)...) // caps lock on
	add(press(kB)...)
	add([2]int32{kShift, 1}, [2]int32{kB, 1}, [2]int32{kB, 0}, [2]int32{kShift, 0}) // shift under caps: lower case
	add(press(k1)...)
	add(press(kEnter)...)
	add([2]int32{kShift, 1})
	add(press(kSlash)...) // shift+/ = ?
	add([2]int32{kShift, 0})
	add(press(kEnter)...)
	for _, size := range []int{16, 24} {
		e := setup(t)
		sc := testScanner(KindEvdev)
		err := e.m.readScans(context.Background(), sc, bytes.NewReader(evdevStream(size, evs...)), size, nil)
		if err != io.EOF {
			t.Fatalf("size %d: %v", size, err)
		}
		if got := strings.Join(codes(t, e), ","); got != "A12,Bb1,?" {
			t.Fatalf("size %d: codes %q", size, got)
		}
	}
	// the host layout is what OpenEvdev uses
	if (strings.Contains(runtimeArch(), "64") && evdevSize() != 24) || evdevSize() == 0 {
		t.Fatalf("event size %d", evdevSize())
	}
}

func TestEvdevScanTooLongIsDropped(t *testing.T) {
	e := setup(t)
	sc := testScanner(KindEvdev)
	sc.MaxLen = 4 // the decoder cap is MaxLen+16 = 20
	var evs [][2]int32
	for i := 0; i < 30; i++ {
		evs = append(evs, press(30)...)
	}
	evs = append(evs, press(28)...)
	evs = append(evs, press(2)...)
	evs = append(evs, press(3)...)
	evs = append(evs, press(28)...)
	var rejected int
	_ = e.m.readScans(context.Background(), sc, bytes.NewReader(evdevStream(24, evs...)), 24, func(_ string, rj bool) {
		if rj {
			rejected++
		}
	})
	if got := strings.Join(codes(t, e), ","); got != "12" || rejected != 1 {
		t.Fatalf("codes %q rejected %d", got, rejected)
	}
}

func TestReaderReconnectsAfterUnplug(t *testing.T) {
	e := setup(t)
	oldMin := backoffMin
	backoffMin = 10 * time.Millisecond
	t.Cleanup(func() { backoffMin = oldMin })

	e.saveScanner(t, map[string]any{"name": "belt", "kind": KindSerial, "device": "/dev/ttyUSB9", "enabled": true, "dedupe_ms": -1})
	var mu sync.Mutex
	var writers []*io.PipeWriter
	opens := 0
	e.m.open = func(sc *Scanner) (closer, error) {
		mu.Lock()
		defer mu.Unlock()
		opens++
		if opens == 2 {
			return nil, io.ErrClosedPipe // still unplugged
		}
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		return pr, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.m.reconcile(ctx)
	writer := func(i int) *io.PipeWriter {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			if len(writers) > i {
				w := writers[i]
				mu.Unlock()
				return w
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no device session %d", i)
		return nil
	}
	w0 := writer(0)
	_, _ = w0.Write([]byte("BEFORE1\r\n"))
	waitEvents(t, e, 1)
	w0.CloseWithError(io.ErrUnexpectedEOF) // unplug
	_, _ = writer(1).Write([]byte("AFTER01\r\n"))
	waitEvents(t, e, 2)
	if got := strings.Join(codes(t, e), ","); got != "BEFORE1,AFTER01" {
		t.Fatalf("codes %q", got)
	}
	var st []ReaderStatus
	for i := 0; i < 500; i++ { // the counters are updated just after the insert
		if st = e.m.Status(); len(st) == 1 && st[0].Scans == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(st) != 1 || st[0].Name != "belt" || st[0].State != "connected" || st[0].Reconnects < 2 || st[0].Scans != 2 {
		t.Fatalf("status %+v", st)
	}
	// changing the config restarts the reader; disabling stops it
	r, _ := e.app.FindFirstRecordByData(ConfigCollection, "name", "belt")
	r.Set("enabled", false)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	e.m.reconcile(ctx)
	e.m.supMu.Lock()
	n := len(e.m.running)
	e.m.supMu.Unlock()
	if n != 0 {
		t.Fatalf("%d readers still running", n)
	}
	if st := e.m.Status(); st[0].State != "disabled" {
		t.Fatalf("status %+v", st)
	}
}

// ---- realtime ------------------------------------------------------------

type sse struct {
	t    *testing.T
	body io.ReadCloser
	br   *bufio.Reader
	cid  string
}

func (e *env) connectSSE(t *testing.T) *sse {
	t.Helper()
	res, err := http.Get(e.srv.URL + "/api/realtime")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	s := &sse{t: t, body: res.Body, br: bufio.NewReader(res.Body)}
	ev, data := s.read()
	if ev != "PB_CONNECT" {
		t.Fatalf("first event %q", ev)
	}
	var c struct {
		ClientID string `json:"clientId"`
	}
	_ = json.Unmarshal([]byte(data), &c)
	s.cid = c.ClientID
	return s
}

func (s *sse) read() (string, string) {
	var ev, data string
	for {
		line, err := s.br.ReadString('\n')
		if err != nil {
			return "", ""
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev != "" {
				return ev, data
			}
		case strings.HasPrefix(line, "event:"):
			ev = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[5:])
		}
	}
}

func (s *sse) subscribe(e *env, tok string, topics ...string) int {
	body, _ := json.Marshal(map[string]any{"clientId": s.cid, "subscriptions": topics})
	r, _ := http.NewRequest("POST", e.srv.URL+"/api/realtime", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if tok != "" {
		r.Header.Set("Authorization", tok)
	}
	rs, err := http.DefaultClient.Do(r)
	if err != nil {
		s.t.Fatal(err)
	}
	rs.Body.Close()
	return rs.StatusCode
}

// next returns the next "@scan" payload within d, or nil.
func (s *sse) next(d time.Duration) map[string]any {
	ch := make(chan map[string]any, 1)
	go func() {
		for {
			ev, data := s.read()
			if ev == "" {
				ch <- nil
				return
			}
			if topicName(ev) == Topic {
				var m map[string]any
				_ = json.Unmarshal([]byte(data), &m)
				ch <- m
				return
			}
		}
	}()
	select {
	case m := <-ch:
		return m
	case <-time.After(d):
		return nil
	}
}

func TestRealtimeScanTopicNeedsAuth(t *testing.T) {
	e := setup(t)
	user := e.token(t, "users")

	guest := e.connectSSE(t)
	if st := guest.subscribe(e, "", Topic); st != 403 {
		t.Fatalf("a guest must not subscribe to @scan (got %d)", st)
	}
	if st := guest.subscribe(e, "garbage", Topic); st != 403 && st != 401 {
		t.Fatalf("a bad token must not subscribe (got %d)", st)
	}
	auth := e.connectSSE(t)
	if st := auth.subscribe(e, user, Topic); st != 204 {
		t.Fatalf("an authenticated client subscribes (got %d)", st)
	}

	st, posted := e.post(t, user, map[string]any{"code": "LIVE1234"})
	if st != 200 {
		t.Fatalf("post %d", st)
	}
	got := auth.next(3 * time.Second)
	if got == nil || got["code"] != "LIVE1234" || got["id"] != posted["id"] || got["scanner"] != "web" || got["symbology"] == nil || got["ts"] == nil {
		t.Fatalf("payload %v", got)
	}
	if len(got) != 5 {
		t.Fatalf("payload must be exactly {id, scanner, code, symbology, ts}: %v", got)
	}
	if g := guest.next(300 * time.Millisecond); g != nil {
		t.Fatalf("the guest received %v", g)
	}
	// a duplicate emits nothing
	e.post(t, user, map[string]any{"code": "LIVE1234"})
	if g := auth.next(300 * time.Millisecond); g != nil {
		t.Fatalf("a duplicate must not publish: %v", g)
	}
}

func TestRealtimeScanTopicSuperuserMode(t *testing.T) {
	t.Setenv("TOKI_SCAN_TOPIC_AUTH", "superuser")
	e := setup(t)
	user, su := e.token(t, "users"), e.token(t, core.CollectionNameSuperusers)

	c1 := e.connectSSE(t)
	if st := c1.subscribe(e, user, Topic); st != 403 {
		t.Fatalf("a plain user must not subscribe in superuser mode (got %d)", st)
	}
	c2 := e.connectSSE(t)
	if st := c2.subscribe(e, su, Topic+"?options=%7B%7D"); st != 204 {
		t.Fatalf("superuser: %d", st)
	}
	if st, _ := e.post(t, user, map[string]any{"code": "SUPER123"}); st != 200 {
		t.Fatalf("a user can still post: %d", st)
	}
	if got := c2.next(3 * time.Second); got == nil || got["code"] != "SUPER123" {
		t.Fatalf("payload %v", got)
	}
	if st, _ := e.get(t, user, "/api/scan/events"); st != 403 {
		t.Fatalf("events for a plain user in superuser mode: %d", st)
	}
	if st, _ := e.get(t, su, "/api/scan/events"); st != 200 {
		t.Fatalf("events for superuser: %d", st)
	}
}

func TestAllowedAuth(t *testing.T) {
	if allowedAuth(nil) || allowedAuth((*core.Record)(nil)) {
		t.Fatal("no auth, no data")
	}
}

func TestScannersStatusHidesDeviceFromNonSuperusers(t *testing.T) {
	e := setup(t)
	e.saveScanner(t, map[string]any{"name": "belt", "kind": KindSerial, "device": "/dev/ttyUSB9", "enabled": true})
	_, o := e.get(t, e.token(t, "users"), "/api/scan/scanners")
	it := o["items"].([]any)[0].(map[string]any)
	if it["name"] != "belt" || it["device"] != nil {
		t.Fatalf("%v", it)
	}
	_, o = e.get(t, e.token(t, core.CollectionNameSuperusers), "/api/scan/scanners")
	if o["items"].([]any)[0].(map[string]any)["device"] != "/dev/ttyUSB9" {
		t.Fatalf("%v", o)
	}
}

func runtimeArch() string { return runtime.GOARCH }
func evdevSize() int      { return devio.EventSize }
