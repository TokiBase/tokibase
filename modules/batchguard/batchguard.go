//go:build !no_batchguard

// Package batchguard validates atomic `/api/batch` calls as a whole.
// PocketBase rules see one record at a time; checkout style invariants
// (order + items + stock) need a check that sees every sub-request.
//
// Rules live in the system collection `_batch_rules` (superusers only).
package batchguard

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/spf13/cast"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

// CollectionName is the system collection that stores the rules.
const CollectionName = "_batch_rules"

// ErrCode is the validation code of a rejected batch.
const ErrCode = "validation_batch_rule"

const hookId = "__tokiBatchGuard__"

// Match is one requirement of a rule: the batch must contain a request for
// Collection with Method (empty Method = any).
type Match struct {
	Collection string `json:"collection"`
	Method     string `json:"method,omitempty"`
}

// Rule is one row of `_batch_rules`.
type Rule struct {
	Id         string  `json:"id,omitempty"`
	Name       string  `json:"name"`
	Enabled    bool    `json:"enabled"`
	Match      []Match `json:"match"`
	Assert     string  `json:"assert,omitempty"`
	AssertPost string  `json:"assert_post,omitempty"`
	Message    string  `json:"message,omitempty"`
}

// Module holds the module state.
type Module struct {
	app     core.App
	timeout time.Duration
	// listRules loads the rules (replaceable in tests).
	listRules func(core.App) ([]Rule, error)
}

// Register creates the collection (if needed) and binds the batch hook.
func Register(app core.App) *Module {
	m := &Module{app: app, timeout: EvalTimeout, listRules: List}

	ensure := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("batchguard: failed to initialize "+CollectionName, "error", err)
		}
	}
	if app.IsBootstrapped() {
		ensure()
	}
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId + "release",
		Func: func(e *core.TerminateEvent) error {
			kernel.ReleaseBatchHooks(app)
			return e.Next()
		},
	})
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			ensure()
			return nil
		},
	})

	app.OnRecordValidate(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "validate",
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if err := ValidateRule(toRule(e.Record)); err != nil {
				return validation.Errors{"assert": validation.NewError("validation_batch_rule_definition", err.Error())}
			}
			return nil
		},
	})

	app.OnBatchRequest().Bind(&hook.Handler[*core.BatchRequestEvent]{
		Id: hookId, Priority: -10000,
		Func: m.onBatch,
	})
	return m
}

// EnsureCollection creates the `_batch_rules` system collection when missing.
func EnsureCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(CollectionName); c != nil {
		return nil
	}
	c := core.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 100},
		&core.BoolField{Name: "enabled"},
		&core.JSONField{Name: "match", MaxSize: 16384},
		&core.TextField{Name: "assert", Max: MaxExprLen},
		&core.TextField{Name: "assert_post", Max: MaxExprLen},
		&core.TextField{Name: "message", Max: 500},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_batch_rules_name", true, "[[name]]", "")
	return app.Save(c)
}

// ValidateRule checks a definition: expressions parse, limits hold, match is well formed.
func ValidateRule(r Rule) error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("name is required")
	}
	if r.Assert == "" && r.AssertPost == "" {
		return errors.New("assert or assert_post is required")
	}
	if r.Assert != "" {
		if _, err := Parse(r.Assert); err != nil {
			return fmt.Errorf("assert: %w", err)
		}
	}
	if r.AssertPost != "" {
		if _, err := Parse(r.AssertPost); err != nil {
			return fmt.Errorf("assert_post: %w", err)
		}
	}
	for i, m := range r.Match {
		if strings.TrimSpace(m.Collection) == "" {
			return fmt.Errorf("match[%d]: collection is required", i)
		}
		switch strings.ToUpper(m.Method) {
		case "", "POST", "PATCH", "PUT", "DELETE":
		default:
			return fmt.Errorf("match[%d]: method must be POST, PATCH, PUT, DELETE or empty", i)
		}
	}
	return nil
}

func toRule(rec *core.Record) Rule {
	r := Rule{
		Id: rec.Id, Name: rec.GetString("name"), Enabled: rec.GetBool("enabled"),
		Assert: rec.GetString("assert"), AssertPost: rec.GetString("assert_post"),
		Message: rec.GetString("message"),
	}
	_ = rec.UnmarshalJSONField("match", &r.Match)
	return r
}

// List returns all rules sorted by name.
func List(app core.App) ([]Rule, error) {
	recs, err := app.FindAllRecords(CollectionName)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(recs))
	for _, r := range recs {
		out = append(out, toRule(r))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Save creates or replaces (by name) a rule after validating it.
func Save(app core.App, r Rule) (*Rule, error) {
	if err := ValidateRule(r); err != nil {
		return nil, err
	}
	col, err := app.FindCollectionByNameOrId(CollectionName)
	if err != nil {
		return nil, err
	}
	rec, err := app.FindFirstRecordByData(CollectionName, "name", r.Name)
	if err != nil {
		rec = core.NewRecord(col)
	}
	rec.Set("name", r.Name)
	rec.Set("enabled", r.Enabled)
	if r.Match == nil {
		r.Match = []Match{}
	}
	rec.Set("match", r.Match)
	rec.Set("assert", r.Assert)
	rec.Set("assert_post", r.AssertPost)
	rec.Set("message", r.Message)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	out := toRule(rec)
	return &out, nil
}

// Remove deletes a rule by name.
func Remove(app core.App, name string) (bool, error) {
	rec, err := app.FindFirstRecordByData(CollectionName, "name", name)
	if err != nil {
		return false, nil
	}
	return true, app.Delete(rec)
}

// ----- batch parsing -----

var recordsURL = regexp.MustCompile(`^/api/collections/([^/?]+)/records(?:/([^/?]+))?(?:\?.*)?$`)

// parseRequests reduces the upstream requests to the views rules evaluate.
// It replays the batch the way upstream executes it (in order), so a PUT
// upsert is classified by the state it will meet at execution time: records
// deleted or created by EARLIER sub-requests of the same batch are taken into
// account. Call it inside the transaction for an authoritative result.
func parseRequests(app core.App, batch []*core.InternalRequest) []reqView {
	out := make([]reqView, len(batch))
	gone := map[string]bool{}  // collection/id deleted earlier in this batch
	added := map[string]bool{} // collection/id created earlier in this batch
	for i, ir := range batch {
		v := reqView{Index: i, Method: strings.ToUpper(ir.Method), Data: ir.Body}
		v.Deleted = v.Method == http.MethodDelete
		if mm := recordsURL.FindStringSubmatch(ir.URL); mm != nil {
			v.Collection, v.ID = mm[1], mm[2]
			if col, err := app.FindCachedCollectionByNameOrId(mm[1]); err == nil {
				v.Collection = col.Name
			}
			key := func(id string) string { return v.Collection + "/" + id }
			switch v.Method {
			case http.MethodPut: // upsert: upstream decides on the id in the body, at execution time
				id := cast.ToString(ir.Body["id"])
				v.Method = http.MethodPost
				v.Upsert = true
				if id != "" {
					v.ID = id
					exists := added[key(id)]
					if !exists && !gone[key(id)] {
						_, err := app.FindRecordById(v.Collection, id)
						exists = err == nil
					}
					if exists {
						v.Method = http.MethodPatch
					}
					added[key(id)], gone[key(id)] = true, false
				}
			case http.MethodPost:
				if id := cast.ToString(ir.Body["id"]); id != "" {
					added[key(id)], gone[key(id)] = true, false
				}
			case http.MethodDelete:
				if v.ID != "" {
					gone[key(v.ID)], added[key(v.ID)] = true, false
				}
			}
		}
		out[i] = v
	}
	return out
}

// applies reports whether the rule's match holds. With loose, an upsert
// (PUT) request matches any method (used before the transaction, where its
// final classification is not known yet).
func applies(app core.App, r Rule, reqs []reqView, loose ...bool) bool {
	if !r.Enabled || len(r.Match) == 0 {
		return false
	}
	lo := len(loose) > 0 && loose[0]
	c := &evalCtx{app: app}
	for _, m := range r.Match {
		coll := c.canon(m.Collection)
		method := strings.ToUpper(m.Method)
		if method == "PUT" {
			method = ""
		}
		found := false
		for _, q := range reqs {
			if q.Collection == coll && (method == "" || q.Method == method || (lo && q.Upsert)) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// modifierBase returns the field name a PocketBase body key modifies
// (`qty+`, `qty-`, `+tags`, `slug:autogenerate`), or key when it is plain.
func modifierBase(key string) string {
	b := strings.TrimPrefix(key, "+")
	if i := strings.Index(b, ":"); i > 0 {
		b = b[:i]
	}
	b = strings.TrimRight(b, "+-")
	if b == "" {
		return key
	}
	return b
}

// modifierConflict returns an error when a body uses a modifier key for a
// field the pre-check (`assert`) reads: the guard would evaluate the raw
// value while PocketBase stores a different one.
func modifierConflict(rules []Rule, reqs []reqView) error {
	names := map[string]bool{}
	for _, r := range rules {
		if r.Assert == "" {
			continue
		}
		if n, err := Parse(r.Assert); err == nil {
			referencedNames(n, names)
		}
	}
	if len(names) == 0 {
		return nil
	}
	for _, q := range reqs {
		if q.Deleted {
			continue
		}
		for k := range q.Data {
			if base := modifierBase(k); base != k && names[base] {
				return reject("", fmt.Sprintf("Request %d uses the modifier key %q on field %q, which a batch rule reads in `assert`. Send a plain value, or use `assert_post` for this rule.", q.Index, k, base))
			}
		}
	}
	return nil
}

// ----- errors -----

type ruleError struct {
	rule, message string
}

func (e *ruleError) Error() string { return e.message }
func (e *ruleError) Code() string  { return ErrCode }
func (e *ruleError) Resolve(map[string]any) any {
	return map[string]any{"code": ErrCode, "message": e.message, "rule": e.rule}
}

func reject(rule, message string) error {
	return router.NewBadRequestError("Batch rejected.", map[string]any{"batch": &ruleError{rule: rule, message: message}})
}

func (m *Module) failure(r Rule, phase string, err error) error {
	if err == nil {
		if r.Message != "" {
			return reject(r.Name, r.Message)
		}
		return reject(r.Name, "Batch rule "+r.Name+" failed.")
	}
	m.app.Logger().Warn("batchguard: rule could not be evaluated", "rule", r.Name, "phase", phase, "error", err)
	return reject(r.Name, "Batch rule "+r.Name+" could not be evaluated ("+phase+"): "+err.Error())
}

func (m *Module) check(app core.App, r Rule, src, phase string, reqs []reqView, auth *core.Record) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = m.failure(r, phase, fmt.Errorf("internal error: %v", p))
		}
	}()
	if src == "" {
		return nil
	}
	n, err := Parse(src)
	if err != nil {
		return m.failure(r, phase, err)
	}
	c := &evalCtx{app: app, reqs: reqs, auth: auth, deadline: time.Now().Add(m.timeout)}
	ok, err := evalBool(n, c)
	if err != nil {
		return m.failure(r, phase, err)
	}
	if !ok {
		return m.failure(r, phase, nil)
	}
	return nil
}

// ----- the hook -----

type bufWriter struct {
	h      http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (b *bufWriter) Header() http.Header { return b.h.Header() }
func (b *bufWriter) WriteHeader(c int) {
	if b.status == 0 {
		b.status = c
	}
}
func (b *bufWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.buf.Write(p)
}
func (b *bufWriter) flush() {
	if b.status == 0 {
		return
	}
	b.h.WriteHeader(b.status)
	_, _ = b.h.Write(b.buf.Bytes())
}

func (m *Module) loadRules(app core.App) ([]Rule, error) {
	if _, err := app.FindCachedCollectionByNameOrId(CollectionName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // collection not created yet (first boot)
		}
		return nil, err
	}
	return m.listRules(app)
}

// fieldsWithID appends `id` to the `fields` query param of a records URL so
// the batch response always carries the id of a written record.
func fieldsWithID(raw string) string {
	i := strings.Index(raw, "?")
	if i < 0 {
		return raw
	}
	q, err := url.ParseQuery(raw[i+1:])
	if err != nil || q.Get("fields") == "" {
		return raw
	}
	q.Set("fields", q.Get("fields")+",id")
	return raw[:i] + "?" + q.Encode()
}

func (m *Module) onBatch(e *core.BatchRequestEvent) error {
	hooks := kernel.OnBatchFor(m.app)
	rules, err := m.loadRules(e.App)
	if err != nil {
		// a broken rules table must not silently disable the guard
		m.app.Logger().Error("batchguard: rules could not be loaded, batch refused", "error", err)
		return router.NewInternalServerError("Batch rules could not be loaded.", nil)
	}
	if len(rules) == 0 && hooks.Length() == 0 {
		return e.Next()
	}
	// cheap pre-filter outside the transaction (upserts match loosely)
	pre := parseRequests(e.App, e.Batch)
	possible := false
	for _, r := range rules {
		if applies(e.App, r, pre, true) {
			possible = true
			break
		}
	}
	if !possible && hooks.Length() == 0 {
		return e.Next()
	}

	origApp, origResp := e.App, e.Response
	var bw *bufWriter
	needPost := hooks.Length() > 0
	for _, r := range rules {
		if r.Enabled && r.AssertPost != "" {
			needPost = true
		}
	}
	if needPost {
		bw = &bufWriter{h: e.Response}
		e.Response = bw
	}
	defer func() { e.App, e.Response = origApp, origResp }()

	err = origApp.RunInTransaction(func(txKernel kernel.App) error {
		txApp := core.AsApp(txKernel)
		e.App = txApp

		// authoritative classification, inside the transaction
		reqs := parseRequests(txApp, e.Batch)
		var active []Rule
		for _, r := range rules {
			if applies(txApp, r, reqs) {
				active = append(active, r)
			}
		}
		hasPost := hooks.Length() > 0
		for _, r := range active {
			if r.AssertPost != "" {
				hasPost = true
			}
		}
		if err := modifierConflict(active, reqs); err != nil {
			return err
		}
		if hasPost {
			// the response must carry record ids even when a sub-request picks `fields`
			for _, ir := range e.Batch {
				if recordsURL.MatchString(ir.URL) {
					ir.URL = fieldsWithID(ir.URL)
				}
			}
		}

		// phase 1: before any sub-request, inside the transaction
		for _, r := range active {
			if err := m.check(txApp, r, r.Assert, "assert", reqs, e.Auth); err != nil {
				return err
			}
		}
		if err := m.emit(kernel.BatchBefore, txApp, reqs, e.Auth); err != nil {
			return err
		}

		if err := e.Next(); err != nil {
			return err
		}

		if !hasPost || bw == nil {
			return nil
		}
		// phase 2: after the last sub-request, still inside the transaction
		post, err := m.readBack(txApp, reqs, bw.buf.Bytes())
		if err != nil {
			m.app.Logger().Warn("batchguard: written records could not be identified", "error", err)
			return reject("", "Batch rejected: the written records could not be identified for validation.")
		}
		for _, r := range active {
			if err := m.check(txApp, r, r.AssertPost, "assert_post", post, e.Auth); err != nil {
				return err
			}
		}
		return m.emit(kernel.BatchAfter, txApp, post, e.Auth)
	})
	if err != nil {
		return err
	}
	if bw != nil {
		e.Response = origResp
		bw.flush()
	}
	return nil
}

func (m *Module) emit(name string, app core.App, reqs []reqView, auth *core.Record) error {
	hooks := kernel.OnBatchFor(m.app)
	if hooks.Length() == 0 {
		return nil
	}
	ev := &kernel.BatchEvent{Name: name, App: app, Auth: auth, Requests: make([]kernel.BatchRequest, len(reqs))}
	for i, r := range reqs {
		ev.Requests[i] = kernel.BatchRequest{Index: r.Index, Collection: r.Collection, Method: r.Method, ID: r.ID, Body: r.Data, Deleted: r.Deleted}
	}
	err := hooks.Trigger(ev)
	if err == nil {
		return nil
	}
	var ae *router.ApiError
	if errors.As(err, &ae) {
		return err
	}
	return reject(name, err.Error())
}

// readBack re-reads, inside the transaction, every record the batch wrote.
// Record ids come from the batch response (an array of {status, body}); the
// sub-request URLs were rewritten so `fields=` cannot hide the id. When the id
// of a written record cannot be determined it returns an error (fail closed).
func (m *Module) readBack(app core.App, reqs []reqView, resp []byte) ([]reqView, error) {
	var results []struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(resp, &results); err != nil {
		return nil, fmt.Errorf("unreadable batch response: %w", err)
	}
	if len(results) != len(reqs) {
		return nil, fmt.Errorf("batch response has %d results for %d requests", len(results), len(reqs))
	}
	out := make([]reqView, len(reqs))
	for i, r := range reqs {
		r.Data = nil
		if results[i].Body != nil {
			if id, _ := results[i].Body["id"].(string); id != "" {
				r.ID = id
			}
		}
		switch {
		case r.Method == http.MethodDelete:
			r.Deleted = true
		case r.Collection == "":
			// not a records URL: nothing to read back
		case r.ID == "":
			return nil, fmt.Errorf("request %d (%s %s): no record id", i, r.Method, r.Collection)
		default:
			if rec, err := app.FindRecordById(r.Collection, r.ID); err == nil {
				r.Data = rec.FieldsData()
			} else {
				r.Deleted = true
			}
		}
		out[i] = r
	}
	return out, nil
}
