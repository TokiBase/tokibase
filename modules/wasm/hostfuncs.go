//go:build !no_wasm

package wasm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/netguard"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/mailer"
	"github.com/tokibase/tokibase/tools/search"
)

func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

// internalSaves marks records written by a guest so that their own writes do
// not re-trigger WASM hooks (loop guard). Other hooks still fire.
var internalSaves sync.Map

type hostFn func(ctx context.Context, c *call, req []byte) (map[string]any, error)

// instantiateHost registers module "toki" on rt.
func (h *Host) instantiateHost(ctx context.Context, rt wazero.Runtime) error {
	b := rt.NewHostModuleBuilder("toki")
	reg := func(name, need string, fn hostFn) {
		b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, st []uint64) {
			st[0] = h.dispatch(ctx, m, name, need, fn, uint32(st[0]), uint32(st[1]))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI64}).Export(name)
	}
	reg("records_find", "records", h.recordsFind)
	reg("records_save", "records", h.recordsSave)
	reg("records_delete", "records", h.recordsDelete)
	reg("http_fetch", "http", h.httpFetch)
	reg("mail_send", "mail", h.mailSend)
	reg("kv_get", "kv", h.kvGet)
	reg("kv_set", "kv", h.kvSet)
	reg("jobs_enqueue", "jobs", h.jobsEnqueue)
	b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, st []uint64) {
		c, _ := ctx.Value(callKey{}).(*call)
		if c == nil {
			return
		}
		n := uint32(st[2])
		if n > 64<<10 {
			n = 64 << 10
		}
		msg, _ := m.Memory().Read(uint32(st[1]), n)
		c.h.guestLog(c.mod, int(st[0]), string(msg))
	}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, nil).Export("log")
	_, err := b.Instantiate(ctx)
	return err
}

func (h *Host) guestLog(m *Module, level int, msg string) {
	l := h.app.Logger().With("wasm_module", m.Name)
	switch {
	case level <= 0:
		l.Debug(msg)
	case level == 1:
		l.Info(msg)
	case level == 2:
		l.Warn(msg)
	default:
		l.Error(msg)
	}
}

// dispatch reads the request from guest memory, runs fn and writes the JSON
// response back through the guest's toki_alloc; returns ptr<<32|len (0 on
// failure to allocate).
func (h *Host) dispatch(ctx context.Context, m api.Module, name, need string, fn hostFn, ptr, n uint32) uint64 {
	c, _ := ctx.Value(callKey{}).(*call)
	var resp map[string]any
	switch {
	case c == nil:
		resp = errResp("no call context")
	case !c.mod.Has(need):
		resp = errResp(fmt.Sprintf("capability %q not granted: add it to needs in %s.toml", need, c.mod.Name))
	case n > MaxHostReqBytes:
		resp = errResp("request too large")
	case ctx.Err() != nil:
		resp = errResp("call deadline exceeded")
	default:
		req, ok := m.Memory().Read(ptr, n)
		if !ok {
			resp = errResp("invalid request pointer")
			break
		}
		req = append([]byte(nil), req...)
		var err error
		resp, err = fn(ctx, c, req)
		if err == nil && ctx.Err() != nil {
			err = errors.New("call deadline exceeded")
		}
		if err != nil {
			resp = errResp(err.Error())
		}
	}
	resp["ok"] = resp["error"] == nil
	out, _ := json.Marshal(resp)
	alloc := m.ExportedFunction("toki_alloc")
	if alloc == nil {
		h.app.Logger().Warn("wasm: guest does not export toki_alloc", "host_fn", name)
		return 0
	}
	r, err := alloc.Call(ctx, uint64(len(out)))
	if err != nil || len(r) == 0 || !m.Memory().Write(uint32(r[0]), out) {
		return 0
	}
	return r[0]<<32 | uint64(len(out))
}

func errResp(msg string) map[string]any { return map[string]any{"error": msg} }

func decode(req []byte, v any) error {
	if err := json.Unmarshal(req, v); err != nil {
		return fmt.Errorf("invalid request JSON: %w", err)
	}
	return nil
}

// ---- records ----
//
// needs=records is NOT superuser: system collections (names starting with "_"
// or System=true: _superusers, _webhooks, _agents, ...) are never reachable,
// writes run the record request hook chain as a guest (fieldperm, computed,
// crypto and ruleguard guards apply) and reservedFields cannot be set.

var errSystemCollection = errors.New("system collections (names starting with \"_\") are not accessible to guests")

// hostCollection resolves a collection a guest may touch.
func hostCollection(app core.App, name string) (*core.Collection, error) {
	if name == "" || strings.HasPrefix(name, "_") {
		return nil, errSystemCollection
	}
	col, err := app.FindCachedCollectionByNameOrId(name)
	if err != nil {
		return nil, err
	}
	if col.System || strings.HasPrefix(col.Name, "_") {
		return nil, errSystemCollection
	}
	return col, nil
}

func (h *Host) recordsFind(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Collection string         `json:"collection"`
		ID         string         `json:"id"`
		Filter     string         `json:"filter"`
		Params     map[string]any `json:"params"`
		Sort       string         `json:"sort"`
		Limit      int            `json:"limit"`
		Offset     int            `json:"offset"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	app := c.db()
	col, err := hostCollection(app, r.Collection)
	if err != nil {
		return nil, err
	}
	if r.ID != "" {
		rec := &core.Record{}
		if err := app.RecordQuery(col).AndWhere(dbx.HashExp{col.Name + ".id": r.ID}).Limit(1).WithContext(ctx).One(rec); err != nil {
			return nil, err
		}
		return map[string]any{"records": []any{rec.PublicExport()}}, nil
	}
	if r.Limit <= 0 || r.Limit > 500 {
		r.Limit = 100
	}
	recs, err := findByFilter(ctx, app, col, r.Filter, r.Sort, r.Limit, r.Offset, dbx.Params(r.Params))
	if err != nil {
		return nil, err
	}
	out := make([]any, len(recs))
	for i, rec := range recs {
		out[i] = rec.PublicExport()
	}
	return map[string]any{"records": out}, nil
}

// findByFilter is FindRecordsByFilter with a context (the query is canceled
// with the call deadline).
func findByFilter(ctx context.Context, app core.App, col *core.Collection, filter, sort string, limit, offset int, params dbx.Params) ([]*core.Record, error) {
	q := app.RecordQuery(col)
	resolver := core.NewRecordFieldResolver(app, col, nil, true)
	if filter != "" {
		expr, err := search.FilterData(filter).BuildExpr(resolver, params)
		if err != nil {
			return nil, fmt.Errorf("invalid filter expression: %w", err)
		}
		q.AndWhere(expr)
	}
	if sort != "" {
		for _, sf := range search.ParseSortFromString(sort) {
			expr, err := sf.BuildExpr(resolver)
			if err != nil {
				return nil, err
			}
			if expr != "" {
				q.AndOrderBy(expr)
			}
		}
	}
	if err := resolver.UpdateQuery(q); err != nil {
		return nil, err
	}
	if offset > 0 {
		q.Offset(int64(offset))
	}
	if limit > 0 {
		q.Limit(int64(limit))
	}
	recs := []*core.Record{}
	if err := q.WithContext(ctx).All(&recs); err != nil {
		return nil, err
	}
	return recs, nil
}

// guestWriteGuard runs the record create/update request hook chain (with a
// no-op terminal handler) as a guest request carrying data as the body, so
// the request-level guards of other modules apply to guest writes.
func guestWriteGuard(ctx context.Context, app core.App, col *core.Collection, rec *core.Record, data map[string]any, create bool) error {
	method, hk := http.MethodPatch, app.OnRecordUpdateRequest()
	if create {
		method, hk = http.MethodPost, app.OnRecordCreateRequest()
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	hreq, err := http.NewRequestWithContext(ctx, method, "http://localhost/wasm", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json")
	ev := new(core.RecordRequestEvent)
	ev.RequestEvent = &core.RequestEvent{App: app}
	ev.Request = hreq
	ev.Collection = col
	ev.Record = rec
	if err := hk.Trigger(ev, func(*core.RecordRequestEvent) error { return nil }); err != nil {
		return fmt.Errorf("write denied: %v", err)
	}
	return nil
}

func (h *Host) recordsSave(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Collection string         `json:"collection"`
		ID         string         `json:"id"`
		Data       map[string]any `json:"data"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	app := c.db()
	col, err := hostCollection(app, r.Collection)
	if err != nil {
		return nil, err
	}
	for k := range r.Data {
		if reservedFields[k] {
			return nil, fmt.Errorf("field %q cannot be set by a guest", k)
		}
	}
	if c.dry {
		c.addEffect("records_save", map[string]any{"collection": r.Collection, "id": r.ID, "data": r.Data})
		return map[string]any{"record": r.Data, "dry_run": true}, nil
	}
	var rec *core.Record
	if r.ID != "" {
		rec = &core.Record{}
		if err := app.RecordQuery(col).AndWhere(dbx.HashExp{col.Name + ".id": r.ID}).Limit(1).WithContext(ctx).One(rec); err != nil {
			return nil, err
		}
	} else {
		rec = core.NewRecord(col)
	}
	for k, v := range r.Data {
		rec.Set(k, v)
	}
	if err := guestWriteGuard(ctx, app, col, rec, r.Data, r.ID == ""); err != nil {
		return nil, err
	}
	internalSaves.Store(rec, struct{}{})
	defer internalSaves.Delete(rec)
	if err := app.SaveWithContext(ctx, rec); err != nil {
		return nil, err
	}
	return map[string]any{"record": rec.PublicExport()}, nil
}

func (h *Host) recordsDelete(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Collection string `json:"collection"`
		ID         string `json:"id"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	app := c.db()
	col, err := hostCollection(app, r.Collection)
	if err != nil {
		return nil, err
	}
	if c.dry {
		c.addEffect("records_delete", map[string]any{"collection": r.Collection, "id": r.ID})
		return map[string]any{"dry_run": true}, nil
	}
	rec := &core.Record{}
	if err := app.RecordQuery(col).AndWhere(dbx.HashExp{col.Name + ".id": r.ID}).Limit(1).WithContext(ctx).One(rec); err != nil {
		return nil, err
	}
	internalSaves.Store(rec, struct{}{})
	defer internalSaves.Delete(rec)
	return map[string]any{}, app.DeleteWithContext(ctx, rec)
}

// ---- mail ----

func (h *Host) mailSend(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		To      []string `json:"to"`
		Subject string   `json:"subject"`
		HTML    string   `json:"html"`
		Text    string   `json:"text"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	if len(r.To) == 0 || len(r.To) > 50 {
		return nil, errors.New("to must hold 1 to 50 addresses")
	}
	msg := &mailer.Message{
		From:    mail.Address{Name: h.app.Settings().Meta.SenderName, Address: h.app.Settings().Meta.SenderAddress},
		Subject: r.Subject, HTML: r.HTML, Text: r.Text,
	}
	for _, a := range r.To {
		addr, err := mail.ParseAddress(a)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q", a)
		}
		msg.To = append(msg.To, *addr)
	}
	if c.dry {
		c.addEffect("mail_send", map[string]any{"to": r.To, "subject": r.Subject})
		return map[string]any{"dry_run": true}, nil
	}
	// the mailer has no context: bound it by the call deadline
	done := make(chan error, 1)
	go func() { done <- h.app.NewMailClient().Send(msg) }()
	select {
	case err := <-done:
		return map[string]any{}, err
	case <-ctx.Done():
		return nil, errors.New("mail_send: call deadline exceeded")
	}
}

// ---- kv ----

func (h *Host) kvGet(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Key string `json:"key"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	if r.Key == "" || len(r.Key) > 256 {
		return nil, errors.New("key must be 1 to 256 bytes")
	}
	var row struct {
		Value   string `db:"value"`
		Expires int64  `db:"expires"`
	}
	err := h.app.AuxDB().NewQuery(`SELECT [[value]], [[expires]] FROM {{_wasm_kv}} WHERE [[module]]={:m} AND [[key]]={:k}`).
		WithContext(ctx).Bind(dbx.Params{"m": c.mod.Name, "k": r.Key}).One(&row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil || (row.Expires > 0 && row.Expires < time.Now().Unix()) {
		return map[string]any{"found": false, "value": ""}, nil
	}
	return map[string]any{"found": true, "value": row.Value}, nil
}

func (h *Host) kvSet(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Key   string `json:"key"`
		Value string `json:"value"`
		TTL   int64  `json:"ttl_s"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	if r.Key == "" || len(r.Key) > 256 {
		return nil, errors.New("key must be 1 to 256 bytes")
	}
	if len(r.Value) > 64<<10 {
		return nil, errors.New("value exceeds 64 KiB")
	}
	if c.dry {
		c.addEffect("kv_set", map[string]any{"key": r.Key, "value": r.Value, "ttl_s": r.TTL})
		return map[string]any{"dry_run": true}, nil
	}
	var exp int64
	if r.TTL > 0 {
		exp = time.Now().Unix() + r.TTL
	}
	_, err := h.app.AuxNonconcurrentDB().NewQuery(`INSERT INTO {{_wasm_kv}} ([[module]],[[key]],[[value]],[[expires]]) VALUES ({:m},{:k},{:v},{:e})
		ON CONFLICT([[module]],[[key]]) DO UPDATE SET [[value]]=excluded.[[value]], [[expires]]=excluded.[[expires]]`).
		WithContext(ctx).Bind(dbx.Params{"m": c.mod.Name, "k": r.Key, "v": r.Value, "e": exp}).Execute()
	return map[string]any{}, err
}

// ---- jobs ----

func (h *Host) jobsEnqueue(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
		DelayS  int             `json:"delay_s"`
		Unique  string          `json:"unique"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	if len(r.Payload) > 64<<10 {
		return nil, errors.New("payload exceeds 64 KiB")
	}
	if r.DelayS > 30*24*3600 {
		return nil, errors.New("delay_s exceeds 30 days")
	}
	declared := false
	for _, e := range c.mod.Parsed {
		if e.Kind == KindJob && e.Job == r.Name {
			declared = true
		}
	}
	if !declared {
		return nil, fmt.Errorf("job %q is not declared: add \"job:%s\" to events in %s.toml", r.Name, r.Name, c.mod.Name)
	}
	if c.dry {
		c.addEffect("jobs_enqueue", map[string]any{"name": r.Name, "payload": r.Payload, "delay_s": r.DelayS})
		return map[string]any{"id": "dry-run", "dry_run": true}, nil
	}
	opts := []kernel.EnqueueOption{}
	if r.DelayS > 0 {
		opts = append(opts, kernel.Delay(time.Duration(r.DelayS)*time.Second))
	}
	if r.Unique != "" {
		opts = append(opts, kernel.Unique("wasm:"+c.mod.Name+":"+r.Unique))
	}
	id, err := kernel.Jobs(c.db()).Enqueue(ctx, jobKindJob, jobPayload{Module: c.mod.Name, Name: r.Name, Payload: r.Payload}, opts...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id}, nil
}

// ---- http ----

func (h *Host) httpFetch(ctx context.Context, c *call, req []byte) (map[string]any, error) {
	var r struct {
		Method    string            `json:"method"`
		URL       string            `json:"url"`
		Headers   map[string]string `json:"headers"`
		Body      string            `json:"body"`
		TimeoutMS int               `json:"timeout_ms"`
	}
	if err := decode(req, &r); err != nil {
		return nil, err
	}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("url must be an absolute http(s) URL")
	}
	if !hostAllowed(u.Hostname()) {
		return nil, fmt.Errorf("host %q is not in TOKI_WASM_HTTP_ALLOW", u.Hostname())
	}
	if r.Method == "" {
		r.Method = "GET"
	}
	if c.dry && r.Method != "GET" && r.Method != "HEAD" {
		c.addEffect("http_fetch", map[string]any{"method": r.Method, "url": r.URL})
		return map[string]any{"status": 0, "headers": map[string]string{}, "body": "", "dry_run": true}, nil
	}
	timeout := 10 * time.Second
	if r.TimeoutMS > 0 && time.Duration(r.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(r.TimeoutMS) * time.Millisecond
	}
	// bound by the call's own deadline too
	hreq, err := http.NewRequestWithContext(ctx, r.Method, r.URL, strings.NewReader(r.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range r.Headers {
		hreq.Header.Set(k, v)
	}
	resp, err := newHTTPClient(timeout).Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxStdoutBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxStdoutBytes {
		return nil, errors.New("response body exceeds 1 MiB")
	}
	hdr := map[string]string{}
	for k := range resp.Header {
		hdr[k] = resp.Header.Get(k)
	}
	return map[string]any{"status": resp.StatusCode, "headers": hdr, "body": string(body)}, nil
}

// hostAllowed matches host against TOKI_WASM_HTTP_ALLOW (comma separated
// patterns: "api.example.com", "*.example.com"). Empty denies everything.
func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, p := range strings.Split(os.Getenv("TOKI_WASM_HTTP_ALLOW"), ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		switch {
		case p == "":
		case p == host:
			return true
		case strings.HasPrefix(p, "*.") && strings.HasSuffix(host, p[1:]) && len(host) > len(p)-1:
			return true
		}
	}
	return false
}

var errBlockedTarget = errors.New("target resolves to a private, loopback or link-local address (set TOKI_WASM_ALLOW_PRIVATE=1 to allow)")

func newHTTPClient(timeout time.Duration) *http.Client {
	return netguard.NewClient(timeout, "TOKI_WASM_ALLOW_PRIVATE", errBlockedTarget)
}
