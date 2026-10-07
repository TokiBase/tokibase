//go:build !no_batchguard

// Package batchguard validates atomic `/api/batch` calls as a whole.
// PocketBase rules see one record at a time; checkout style invariants
// (order + items + stock) need a check that sees every sub-request.
//
// Rules live in the system collection `_batch_rules` (superusers only).
package batchguard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
}

// Register creates the collection (if needed) and binds the batch hook.
func Register(app core.App) *Module {
	m := &Module{app: app, timeout: EvalTimeout}

	ensure := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("batchguard: failed to initialize "+CollectionName, "error", err)
		}
	}
	if app.IsBootstrapped() {
		ensure()
	}
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
func parseRequests(app core.App, batch []*core.InternalRequest) []reqView {
	out := make([]reqView, len(batch))
	for i, ir := range batch {
		v := reqView{Index: i, Method: strings.ToUpper(ir.Method), Data: ir.Body}
		v.Deleted = v.Method == http.MethodDelete
		if mm := recordsURL.FindStringSubmatch(ir.URL); mm != nil {
			v.Collection, v.ID = mm[1], mm[2]
			if col, err := app.FindCachedCollectionByNameOrId(mm[1]); err == nil {
				v.Collection = col.Name
			}
			if v.Method == http.MethodPut { // upsert: upstream decides on the id in the body
				id, _ := ir.Body["id"].(string)
				v.Method = http.MethodPost
				if id != "" {
					v.ID = id
					if _, err := app.FindRecordById(v.Collection, id); err == nil {
						v.Method = http.MethodPatch
					}
				}
			}
		}
		out[i] = v
	}
	return out
}

func applies(app core.App, r Rule, reqs []reqView) bool {
	if !r.Enabled || len(r.Match) == 0 {
		return false
	}
	c := &evalCtx{app: app}
	for _, m := range r.Match {
		coll := c.canon(m.Collection)
		method := strings.ToUpper(m.Method)
		if method == "PUT" {
			method = ""
		}
		found := false
		for _, q := range reqs {
			if q.Collection == coll && (method == "" || q.Method == method) {
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

func (m *Module) check(app core.App, r Rule, src, phase string, reqs []reqView, auth *core.Record) error {
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

func (m *Module) onBatch(e *core.BatchRequestEvent) error {
	var rules []Rule
	var err error
	if c, _ := e.App.FindCachedCollectionByNameOrId(CollectionName); c != nil {
		rules, err = List(e.App)
	}
	if err != nil {
		// a broken rules table must not silently disable the guard
		return reject("", "Batch rules could not be loaded.")
	}
	reqs := parseRequests(e.App, e.Batch)
	var active []Rule
	for _, r := range rules {
		if applies(e.App, r, reqs) {
			active = append(active, r)
		}
	}
	hasPost := false
	for _, r := range active {
		if r.AssertPost != "" {
			hasPost = true
		}
	}
	if len(active) == 0 && kernel.OnBatch.Length() == 0 {
		return e.Next()
	}

	origApp, origResp := e.App, e.Response
	var bw *bufWriter
	if hasPost || kernel.OnBatch.Length() > 0 {
		bw = &bufWriter{h: e.Response}
		e.Response = bw
	}
	defer func() { e.App, e.Response = origApp, origResp }()

	err = origApp.RunInTransaction(func(txKernel kernel.App) error {
		txApp := core.AsApp(txKernel)
		e.App = txApp

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

		if bw == nil {
			return nil
		}
		// phase 2: after the last sub-request, still inside the transaction
		post := m.readBack(txApp, reqs, bw.buf.Bytes())
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
	if kernel.OnBatch.Length() == 0 {
		return nil
	}
	ev := &kernel.BatchEvent{Name: name, App: app, Auth: auth, Requests: make([]kernel.BatchRequest, len(reqs))}
	for i, r := range reqs {
		ev.Requests[i] = kernel.BatchRequest{Index: r.Index, Collection: r.Collection, Method: r.Method, ID: r.ID, Body: r.Data, Deleted: r.Deleted}
	}
	err := kernel.OnBatch.Trigger(ev)
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
// Record ids come from the batch response (an array of {status, body}).
func (m *Module) readBack(app core.App, reqs []reqView, resp []byte) []reqView {
	var results []struct {
		Body map[string]any `json:"body"`
	}
	_ = json.Unmarshal(resp, &results)
	out := make([]reqView, len(reqs))
	for i, r := range reqs {
		r.Data = nil
		if i < len(results) && results[i].Body != nil {
			if id, _ := results[i].Body["id"].(string); id != "" {
				r.ID = id
			}
		}
		switch {
		case r.Method == http.MethodDelete || r.Collection == "" || r.ID == "":
			r.Deleted = r.Method == http.MethodDelete
		default:
			if rec, err := app.FindRecordById(r.Collection, r.ID); err == nil {
				r.Data = rec.FieldsData()
			} else {
				r.Deleted = true
			}
		}
		out[i] = r
	}
	return out
}
