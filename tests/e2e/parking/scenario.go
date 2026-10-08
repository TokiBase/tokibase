//go:build !no_sync

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/embed"
)

var flagVals = []string{"vip", "damaged", "lost_ticket", "disputed", "review", "manual", "refund", "fraud"}

type ticket struct {
	id, creator string
	fee         float64
	flags       map[string]bool
	seed        bool
}

type ledger struct {
	mu sync.Mutex
	t  map[string]*ticket
}

func (l *ledger) add(t *ticket) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.flags == nil {
		t.flags = map[string]bool{}
	}
	l.t[t.id] = t
}

func (l *ledger) inc(id string, d float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.t[id].fee += d
}

func (l *ledger) flag(id, f string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.t[id].flags[f] = true
}

type gate struct {
	name, dir, url string
	port           int
	proxy          *proxyHandle
	proc           *proc
	tok            string
	open           []string
}

type result struct {
	id, title string
	ok        bool
	detail    string
}

type scenario struct {
	clockFile string
	clock     *simClock
	led       *ledger

	hubDir, hubURL, hubTok string
	hub                    *proc
	hubEnvv                []string
	gates                  []*gate
	phone                  *embed.Instance
	phoneProxy             *proxyHandle
	phoneSU, phoneTok      string // phone superuser token (reads), officer local token (writes)
	officerHubTok          string
	aid                    string

	// ids of the injected cases
	tx, tp, pd string
	pool       []string // seed tickets the phone flags
	seeds      []string
	carRate    string
	nodeIDs    map[string]string

	sinkMu  sync.Mutex
	sinkIDs map[string]int
	sinkN   int
	sink    *httptest.Server

	results  []result
	lastSeed string
	started  time.Time
}

func newScenario() *scenario {
	_ = os.MkdirAll(cfg.work, 0o755)
	s := &scenario{clockFile: filepath.Join(cfg.work, "clock"), led: &ledger{t: map[string]*ticket{}},
		nodeIDs: map[string]string{}, sinkIDs: map[string]int{}, started: time.Now()}
	s.clock = &simClock{file: s.clockFile}
	_ = s.clock.set(0)
	return s
}

func (s *scenario) cleanup() {
	if s.phone != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_ = s.phone.Stop(ctx)
		cancel()
	}
	for _, g := range s.gates {
		if g.proc != nil {
			g.proc.kill()
		}
		if g.proxy != nil {
			g.proxy.p.kill()
		}
	}
	if s.phoneProxy != nil {
		s.phoneProxy.p.kill()
	}
	if s.hub != nil {
		s.hub.kill()
	}
	if s.sink != nil {
		s.sink.Close()
	}
}

func (s *scenario) record(id, title string, ok bool, format string, a ...any) {
	s.results = append(s.results, result{id: id, title: title, ok: ok, detail: fmt.Sprintf(format, a...)})
}

func (s *scenario) report() bool {
	fmt.Fprintln(os.Stderr, "\n==== parking exit gate ====")
	pass := true
	for _, r := range s.results {
		st := "PASS"
		if !r.ok {
			st, pass = "FAIL", false
		}
		fmt.Fprintf(os.Stderr, "(%s) %-4s %s: %s\n", r.id, st, r.title, r.detail)
	}
	fmt.Fprintf(os.Stderr, "total %s, %d simulated hours\n", time.Since(s.started).Round(time.Second), cfg.hours)
	if pass {
		fmt.Fprintln(os.Stderr, "[parking] OK")
	}
	return pass
}

// ---- hub -----------------------------------------------------------------

func (s *scenario) startHub() error {
	s.hubDir = filepath.Join(cfg.work, "hub", "pb_data") // a sibling pb_migrations of the solo binary must not reach the edge nodes
	port := freePort()
	s.hubURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	s.hubEnvv = childEnv("TOKI_SYNC_ROLE=hub", "TOKI_SYNC_TEST=1", "TOKI_SYNC_TEST_CLOCK_FILE="+s.clockFile,
		"TOKI_SYNC_INSECURE=1", "TOKI_WEBHOOK_ALLOW_PRIVATE=1")
	wasmDir := filepath.Join(cfg.work, "hub-wasm")
	if err := os.MkdirAll(wasmDir, 0o755); err != nil {
		return err
	}
	guest, err := os.ReadFile(cfg.wasmGuest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wasmDir, "payments_conflict.wasm"), guest, 0o644); err != nil {
		return err
	}
	toml := "events = [\"sync.conflict.payments\"]\ntimeout_ms = 3000\n[env]\nMODE = \"double_payment\"\n"
	if err := os.WriteFile(filepath.Join(wasmDir, "payments_conflict.toml"), []byte(toml), 0o644); err != nil {
		return err
	}
	if _, err := runCLI(cfg.hubBin, s.hubEnvv, s.hubDir, "superuser", "upsert", adminEmail, adminPass); err != nil {
		return err
	}
	s.hub = &proc{name: "hub", bin: cfg.hubBin, env: s.hubEnvv,
		args:    []string{"serve", "--dir", s.hubDir, "--http", fmt.Sprintf("127.0.0.1:%d", port), "--wasmHooksDir", wasmDir, "--dev=false"},
		logPath: filepath.Join(cfg.work, "hub.log"), pidPath: filepath.Join(cfg.work, "hub.pid")}
	if err := s.hub.start(); err != nil {
		return err
	}
	if err := waitHealth(s.hubURL, 60*time.Second); err != nil {
		return err
	}
	s.hubTok, err = superToken(s.hubURL)
	return err
}

func (s *scenario) hubAPI(method, path string, body any) (map[string]any, error) {
	return api(method, s.hubURL+path, s.hubTok, body)
}

func fld(name, typ string, extra map[string]any) map[string]any {
	f := map[string]any{"name": name, "type": typ}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

const authRule = `@request.auth.id != ""`

func (s *scenario) hubSchema() error {
	collections := []map[string]any{
		{"name": "officers", "type": "auth", "fields": []any{fld("name", "text", nil)}},
		{"name": "gate_devices", "type": "auth", "fields": []any{fld("name", "text", nil)}},
		{"id": "pbc_tickets", "name": "tickets", "type": "base",
			"listRule": authRule, "viewRule": authRule, "createRule": authRule, "updateRule": authRule, "deleteRule": authRule,
			"fields": []any{
				fld("no", "text", nil), fld("plate", "text", nil), fld("entry_at", "date", nil), fld("exit_at", "date", nil),
				fld("status", "text", nil), fld("fee", "number", nil),
				fld("flags", "select", map[string]any{"maxSelect": 8, "values": flagVals}),
				fld("branch", "text", nil), fld("note", "text", nil),
				fld("created", "autodate", map[string]any{"onCreate": true}),
				fld("updated", "autodate", map[string]any{"onCreate": true, "onUpdate": true}),
			},
			"indexes": []string{"CREATE UNIQUE INDEX idx_tickets_no ON tickets (no)", "CREATE INDEX idx_tickets_branch ON tickets (branch)"}},
		{"id": "pbc_payments", "name": "payments", "type": "base",
			"listRule": authRule, "viewRule": authRule, "createRule": authRule, "updateRule": authRule, "deleteRule": authRule,
			"fields": []any{
				fld("ticket", "text", nil), fld("amount", "number", nil), fld("status", "text", nil),
				fld("provider_ref", "text", nil), fld("note", "text", nil),
				fld("created", "autodate", map[string]any{"onCreate": true}),
				fld("updated", "autodate", map[string]any{"onCreate": true, "onUpdate": true}),
			}},
		// rates: only the hub writes (superuser rules), spokes pull
		{"id": "pbc_rates", "name": "rates", "type": "base", "listRule": authRule, "viewRule": authRule,
			"fields": []any{
				fld("code", "text", nil), fld("price", "number", nil),
				fld("created", "autodate", map[string]any{"onCreate": true}),
				fld("updated", "autodate", map[string]any{"onCreate": true, "onUpdate": true}),
			}},
	}
	for _, c := range collections {
		if _, err := s.hubAPI("POST", "/api/collections", c); err != nil {
			return fmt.Errorf("collection %v: %w", c["name"], err)
		}
	}
	policies := []map[string]any{
		{"collection": "tickets", "direction": "both", "strategy": "field-merge", "enabled": true,
			"partition": "branch = @node.branch", "field_types": map[string]string{"fee": "counter", "flags": "set", "no": "reserve:tickets"}},
		{"collection": "payments", "direction": "both", "strategy": "hook", "hook": "payments_conflict", "enabled": true},
		{"collection": "rates", "direction": "pull", "strategy": "hub-wins", "enabled": true},
		// the auth collection of the officers must exist on the phone before it can take an actor grant
		{"collection": "officers", "direction": "pull", "strategy": "hub-wins", "enabled": true},
	}
	for _, p := range policies {
		if _, err := s.hubAPI("POST", "/api/collections/_sync_policies/records", p); err != nil {
			return fmt.Errorf("policy %v: %w", p["collection"], err)
		}
	}
	if _, err := runCLI(cfg.hubBin, s.hubEnvv, s.hubDir, "sync", "reserve", "create-seq", "tickets", "--block", "1500", "--max-open", "2", "--max-block", "5000"); err != nil {
		return err
	}
	return nil
}

var codeRe = regexp.MustCompile(`(?m)^code:\s+(\S+)`)

func (s *scenario) enrollCode(name, profile string, params []string, actor string) (string, error) {
	args := []string{"sync", "enroll", "--name", name, "--profile", profile}
	for _, p := range params {
		args = append(args, "--param", p)
	}
	if actor != "" {
		args = append(args, "--actor", actor)
	}
	out, err := runCLI(cfg.hubBin, s.hubEnvv, s.hubDir, args...)
	if err != nil {
		return "", err
	}
	m := codeRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("no enrollment code in: %s", out)
	}
	return m[1], nil
}

func (s *scenario) startWebhookSink() error {
	s.sink = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			Event    string `json:"event"`
			RecordID string `json:"record_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&ev)
		if ev.Event == "record.create" {
			s.sinkMu.Lock()
			s.sinkIDs[ev.RecordID]++
			s.sinkN++
			s.sinkMu.Unlock()
		}
		w.WriteHeader(200)
	}))
	_, err := s.hubAPI("POST", "/api/collections/_webhooks/records", map[string]any{
		"name": "parking-sink", "url": s.sink.URL, "secret": "parking-sink-secret-1234", "events": []string{"record.create"},
		"collections": []string{"tickets"}, "enabled": true,
	})
	if err != nil {
		return err
	}
	time.Sleep(6 * time.Second) // the webhook cache is refreshed every 5 s
	return nil
}

// ---- hub data -------------------------------------------------------------

func (s *scenario) hubSeed() error {
	var err error
	if _, err = s.hubAPI("POST", "/api/collections/officers/records", map[string]any{
		"email": "officer@example.com", "password": "OfficerPass123", "passwordConfirm": "OfficerPass123", "name": "Officer"}); err != nil {
		return err
	}
	gids := []string{}
	for i := 1; i <= 3; i++ { // gate-1, gate-2 and the phone device (the service actor that lets the phone pull under the auth view rule)
		r, err := s.hubAPI("POST", "/api/collections/gate_devices/records", map[string]any{
			"email": fmt.Sprintf("gate%d@example.com", i), "password": "GatePass12345", "passwordConfirm": "GatePass12345", "name": fmt.Sprintf("Gate %d", i)})
		if err != nil {
			return err
		}
		gids = append(gids, r["id"].(string))
	}
	s.nodeIDs["gate_devices/1"], s.nodeIDs["gate_devices/2"], s.nodeIDs["gate_devices/3"] = gids[0], gids[1], gids[2]
	for i := 0; i < 30; i++ {
		r, err := s.hubAPI("POST", "/api/collections/tickets/records", map[string]any{
			"no": fmt.Sprintf("SEED-%03d", i), "plate": fmt.Sprintf("B %04d SD", i), "entry_at": s.clock.date(), "status": "open", "branch": "B1", "fee": 0})
		if err != nil {
			return fmt.Errorf("seed ticket: %w", err)
		}
		id := r["id"].(string)
		s.seeds = append(s.seeds, id)
		s.led.add(&ticket{id: id, creator: "hub", seed: true})
	}
	s.tx, s.tp = s.seeds[0], s.seeds[1]
	s.pool = s.seeds[2:]
	pay, err := s.hubAPI("POST", "/api/collections/payments/records", map[string]any{
		"ticket": s.seeds[2], "amount": 50000, "status": "pending", "provider_ref": ""})
	if err != nil {
		return err
	}
	s.pd = pay["id"].(string)
	for _, r := range []struct {
		code  string
		price int
	}{{"car", 5000}, {"moto", 2000}, {"truck", 9000}} {
		rr, err := s.hubAPI("POST", "/api/collections/rates/records", map[string]any{"code": r.code, "price": r.price})
		if err != nil {
			return err
		}
		if r.code == "car" {
			s.carRate = rr["id"].(string)
		}
	}
	// the officer logs in on the hub (this token goes to AddActor)
	lr, err := api("POST", s.hubURL+"/api/collections/officers/auth-with-password", "", map[string]any{"identity": "officer@example.com", "password": "OfficerPass123"})
	if err != nil {
		return err
	}
	s.officerHubTok, _ = lr["token"].(string)
	return nil
}

// ---- gates ---------------------------------------------------------------------

func (s *scenario) gateEnv() []string {
	return childEnv("TOKI_SYNC_ROLE=spoke", "TOKI_SYNC_TEST=1", "TOKI_SYNC_TEST_CLOCK_FILE="+s.clockFile,
		"TOKI_SYNC_INSECURE=1", "TOKI_SYNC_INTERVAL=2s", "TOKI_SYNC_PAGE=100")
}

func (s *scenario) startGate(g *gate) error {
	g.proc = &proc{name: g.name, bin: cfg.gateBin, env: s.gateEnv(),
		args:    []string{"serve", "--dir", g.dir, "--http", fmt.Sprintf("127.0.0.1:%d", g.port), "--dev=false"},
		logPath: filepath.Join(cfg.work, g.name+".log"), pidPath: filepath.Join(cfg.work, g.name+".pid")}
	if err := g.proc.start(); err != nil {
		return err
	}
	if err := waitHealth(g.url, 60*time.Second); err != nil {
		return err
	}
	var err error
	g.tok, err = superToken(g.url)
	return err
}

func (s *scenario) addGate(idx int, hubTarget string) error {
	name := fmt.Sprintf("gate-%d", idx)
	g := &gate{name: name, dir: filepath.Join(cfg.work, name, "pb_data"), port: freePort()}
	g.url = fmt.Sprintf("http://127.0.0.1:%d", g.port)
	var err error
	if g.proxy, err = startProxy(name+"-proxy", hubTarget); err != nil {
		return err
	}
	code, err := s.enrollCode(name, "edge", []string{"branch=B1"}, "gate_devices/"+s.nodeIDs[fmt.Sprintf("gate_devices/%d", idx)])
	if err != nil {
		return err
	}
	if _, err := runCLI(cfg.gateBin, s.gateEnv(), g.dir, "superuser", "upsert", adminEmail, adminPass); err != nil {
		return err
	}
	if _, err := runCLI(cfg.gateBin, s.gateEnv(), g.dir, "sync", "join", "http://"+g.proxy.listen, code); err != nil {
		return err
	}
	if err := s.startGate(g); err != nil {
		return err
	}
	s.gates = append(s.gates, g)
	return nil
}

// ---- phone ---------------------------------------------------------------------

func (s *scenario) startPhone(hubTarget string) error {
	var err error
	if s.phoneProxy, err = startProxy("phone-proxy", hubTarget); err != nil {
		return err
	}
	s.phone, err = embed.Start(s.phoneOptions("http://" + s.phoneProxy.listen))
	if err != nil {
		return err
	}
	if err := s.phone.Superuser(adminEmail, adminPass); err != nil {
		return err
	}
	code, err := s.enrollCode("phone", "nano", []string{"branch=B1"}, "gate_devices/"+s.nodeIDs["gate_devices/3"])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.phone.Sync().Enroll(ctx, "", code); err != nil {
		return fmt.Errorf("phone enroll: %w", err)
	}
	r, err := s.pcall("", "POST", "/api/collections/_superusers/auth-with-password", map[string]any{"identity": adminEmail, "password": adminPass})
	if err != nil {
		return err
	}
	s.phoneSU, _ = r["token"].(string)
	// the schema bundle brings the officers collection: wait for it, then take the grant
	if err := waitFor(60*time.Second, "phone schema", func() bool {
		_, err := s.pcall(s.phoneSU, "GET", "/api/collections/rates/records?perPage=1", nil)
		return err == nil
	}); err != nil {
		return err
	}
	var aerr error
	if err := waitFor(30*time.Second, "phone actor grant", func() bool {
		var aid string
		aid, aerr = s.phone.Sync().AddActor(ctx, s.officerHubTok)
		if aerr != nil {
			return false
		}
		s.aid = aid
		return true
	}); err != nil {
		return fmt.Errorf("%w (last error: %v)", err, aerr)
	}
	s.phoneTok, err = s.phone.Sync().LocalToken(s.aid)
	return err
}

func (s *scenario) pcall(token, method, path string, body any) (map[string]any, error) {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	h := map[string]string{}
	if token != "" {
		h["Authorization"] = token
	}
	st, _, rb, err := s.phone.Call(method, path, h, raw)
	if err != nil {
		return nil, err
	}
	if st/100 != 2 {
		return nil, fmt.Errorf("phone %s %s: HTTP %d: %s", method, path, st, strings.TrimSpace(string(rb)))
	}
	out := map[string]any{}
	_ = json.Unmarshal(rb, &out)
	return out, nil
}

func waitFor(d time.Duration, what string, f func() bool) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s waiting for %s", d, what)
}
