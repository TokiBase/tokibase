// Package fieldperm adds per-field read and write rules on top of the
// collection rules, using the same rule language, without touching the
// collection JSON schema (Admin UI and SDKs keep working).
//
// Rules live in the system collection `_field_rules` (superusers only).
package fieldperm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

// CollectionName is the system collection that stores the rules.
const CollectionName = "_field_rules"

const hookId = "__tokiFieldPerm__"

// ActionDenied is the audit action recorded for a denied write.
const ActionDenied = "field.denied"

// EnvSuperuser: set to "enforce" to apply the rules to superusers too.
const EnvSuperuser = "TOKI_FIELDPERM_SUPERUSER"

// cacheTTL bounds the staleness of the in-memory cache for changes made by
// another process (for example the CLI while a server is running). Changes
// made through this process invalidate the cache immediately.
const cacheTTL = 5 * time.Second

// failTTL is how long a failed rule load is remembered before it is retried
// (avoids a retry storm and keeps the exclusive lock out of the request path).
const failTTL = 5 * time.Second

// auditEvery is the sampling window per (collection, field, user).
const auditEvery = time.Minute

// Rule is one row of `_field_rules`.
//
// Read/Write semantics: nil = inherit (no field rule), "" = read: always
// readable; write: locked (superusers only), any other value = rule expression.
type Rule struct {
	Id         string  `json:"id,omitempty"`
	Collection string  `json:"collection"`
	Field      string  `json:"field"`
	Read       *string `json:"read_rule"`
	Write      *string `json:"write_rule"`
	Note       string  `json:"note,omitempty"`
}

// Module holds the cached rules.
type Module struct {
	app core.App
	now func() time.Time

	mu         sync.RWMutex
	byColl     map[string]map[string]*Rule
	loaded     time.Time
	invalid    bool
	gen        uint64    // bumped by Invalidate, so a load racing with it stays invalid
	loadedOnce bool      // rules were loaded successfully at least once
	failUntil  time.Time // do not retry a failed load before this time

	loadMu sync.Mutex             // serializes reloads (DB I/O happens outside mu)
	load   func() ([]Rule, error) // overridable in tests

	reqCache sync.Map // *core.RequestInfo -> *reqEntry (see eval cache in hooks.go)

	auditMu   sync.Mutex
	auditLast map[string]time.Time
}

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects denied writes to an external audit log.
// Modules must not import each other, so the wiring happens in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

// EnforceSuperuser reports whether superusers are subject to the rules.
func EnforceSuperuser() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(EnvSuperuser)), "enforce")
}

// Register creates the collection (if needed), binds the hooks and returns the module.
func Register(app core.App) *Module {
	m := &Module{app: app, now: time.Now, invalid: true, auditLast: map[string]time.Time{}}
	m.load = func() ([]Rule, error) { return List(app, "") }

	ensure := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("fieldperm: failed to initialize "+CollectionName, "error", err)
		}
		m.Invalidate()
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

	inval := func(e *core.RecordEvent) error {
		err := e.Next()
		m.Invalidate()
		return err
	}
	app.OnRecordAfterCreateSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
	app.OnRecordAfterUpdateSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
	app.OnRecordAfterDeleteSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})

	m.bindHooks()
	return m
}

// EnsureCollection creates the `_field_rules` system collection when missing.
func EnsureCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(CollectionName); c != nil {
		return nil
	}
	c := core.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "collection", Required: true},
		&core.TextField{Name: "field", Required: true},
		&core.JSONField{Name: "read_rule", MaxSize: 4096},
		&core.JSONField{Name: "write_rule", MaxSize: 4096},
		&core.TextField{Name: "note"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_field_rules_unique", true, "[[collection]], [[field]]", "")
	return app.Save(c)
}

// Invalidate drops the cache; the next lookup reloads it.
func (m *Module) Invalidate() {
	m.mu.Lock()
	m.invalid = true
	m.failUntil = time.Time{}
	m.gen++
	m.mu.Unlock()
}

// rulesFor returns the rules of c. closed is true when the rules could not be
// loaded even once: the caller must then fail CLOSED (it cannot know which
// fields are protected). After a good load, a failed refresh keeps serving
// the previous rules.
func (m *Module) rulesFor(c *core.Collection) (rules map[string]*Rule, closed bool) {
	if r, c2, ok := m.cached(c); ok {
		return r, c2
	}

	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	if r, c2, ok := m.cached(c); ok { // someone else reloaded while we waited
		return r, c2
	}

	m.mu.RLock()
	gen := m.gen
	m.mu.RUnlock()
	rs, err := m.load()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.failUntil = m.now().Add(failTTL)
		m.app.Logger().Warn("fieldperm: failed to load rules", "error", err, "failClosed", !m.loadedOnce)
		return m.lookup(c), !m.loadedOnce
	}
	by := map[string]map[string]*Rule{}
	for i := range rs {
		r := rs[i]
		if by[r.Collection] == nil {
			by[r.Collection] = map[string]*Rule{}
		}
		by[r.Collection][r.Field] = &r
	}
	m.byColl, m.loaded, m.loadedOnce, m.failUntil = by, m.now(), true, time.Time{}
	m.invalid = m.gen != gen
	return m.lookup(c), false
}

// cached answers from memory when the cache is fresh or a failed load is still backing off.
func (m *Module) cached(c *core.Collection) (map[string]*Rule, bool, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now()
	if !m.invalid && now.Sub(m.loaded) < cacheTTL {
		return m.lookup(c), false, true
	}
	if now.Before(m.failUntil) {
		return m.lookup(c), !m.loadedOnce, true
	}
	return nil, false, false
}

func (m *Module) lookup(c *core.Collection) map[string]*Rule {
	if r := m.byColl[c.Name]; len(r) > 0 {
		return r
	}
	return m.byColl[c.Id]
}

// ----- storage -----

func decodeRule(v any) *string {
	var raw []byte
	switch t := v.(type) {
	case types.JSONRaw:
		raw = t
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		return nil
	}
	if len(raw) == 0 {
		return nil
	}
	var s *string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return s
}

func encodeRule(s *string) types.JSONRaw {
	b, _ := json.Marshal(s)
	return types.JSONRaw(b)
}

func toRule(r *core.Record) Rule {
	return Rule{
		Id: r.Id, Collection: r.GetString("collection"), Field: r.GetString("field"),
		Read: decodeRule(r.GetRaw("read_rule")), Write: decodeRule(r.GetRaw("write_rule")),
		Note: r.GetString("note"),
	}
}

// List returns the stored rules (optionally for one collection), sorted.
func List(app core.App, collection string) ([]Rule, error) {
	recs, err := app.FindAllRecords(CollectionName)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(recs))
	for _, r := range recs {
		x := toRule(r)
		if collection == "" || x.Collection == collection {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Collection != out[j].Collection {
			return out[i].Collection < out[j].Collection
		}
		return out[i].Field < out[j].Field
	})
	return out, nil
}

// SetOptions selects what Set changes; nil pointers are left untouched
// (a new row starts as inherit/inherit).
type SetOptions struct {
	Read, Write       *string
	ReadSet, WriteSet bool
	Note              *string
}

// Set creates or updates the rule for (collection, field).
func Set(app core.App, collection, field string, o SetOptions) (*Rule, error) {
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return nil, fmt.Errorf("collection %q not found", collection)
	}
	collection = col.Name
	if col.Fields.GetByName(field) == nil {
		return nil, fmt.Errorf("collection %q has no field %q", collection, field)
	}
	rec, err := app.FindFirstRecordByFilter(CollectionName, "collection={:c} && field={:f}", map[string]any{"c": collection, "f": field})
	if err != nil || rec == nil {
		coll, cerr := app.FindCollectionByNameOrId(CollectionName)
		if cerr != nil {
			return nil, cerr
		}
		rec = core.NewRecord(coll)
		rec.Set("collection", collection)
		rec.Set("field", field)
		rec.Set("read_rule", encodeRule(nil))
		rec.Set("write_rule", encodeRule(nil))
	}
	if o.ReadSet {
		rec.Set("read_rule", encodeRule(o.Read))
	}
	if o.WriteSet {
		rec.Set("write_rule", encodeRule(o.Write))
	}
	if o.Note != nil {
		rec.Set("note", *o.Note)
	}
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	r := toRule(rec)
	return &r, nil
}

// Remove deletes the rule for (collection, field); false when none existed.
func Remove(app core.App, collection, field string) (bool, error) {
	if col, err := app.FindCollectionByNameOrId(collection); err == nil {
		collection = col.Name
	}
	rec, err := app.FindFirstRecordByFilter(CollectionName, "collection={:c} && field={:f}", map[string]any{"c": collection, "f": field})
	if err != nil || rec == nil {
		return false, nil
	}
	return true, app.Delete(rec)
}

var errNilInfo = errors.New("fieldperm: missing request info")
