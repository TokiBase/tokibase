// Package computed keeps aggregate fields (counters and rollups) on a parent
// collection correct on the server: likes_count, members_count,
// total_distance_km and so on are recomputed from the child collection after
// every committed child write, instead of by client code or hand-written hooks.
//
// Definitions live in the system collection `_computed_fields` and target
// EXISTING number fields; the collection JSON schema is not changed.
package computed

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

// CollectionName is the system collection that stores the definitions.
const CollectionName = "_computed_fields"

const hookId = "__tokiComputed__"

// Audit actions.
const (
	ActionDefCreate = "computed.def.create"
	ActionDefUpdate = "computed.def.update"
	ActionDefDelete = "computed.def.delete"
	ActionBackfill  = "computed.backfill"
	ActionDrift     = "computed.drift"
)

// EnvAllowManual: set to "1" to let superusers write computed fields by hand.
const EnvAllowManual = "TOKI_COMPUTED_ALLOW_MANUAL"

// EnvDriftCron overrides the daily drift check schedule ("off" disables it).
const EnvDriftCron = "TOKI_COMPUTED_DRIFT_CRON"

// Job kinds.
const (
	JobBackfill = "computed.backfill"
	JobDrift    = "computed.drift"
)

const cacheTTL = 5 * time.Second

// Kinds.
const (
	KindCount = "count"
	KindSum   = "sum"
	KindAvg   = "avg"
	KindMin   = "min"
	KindMax   = "max"
	KindLast  = "last"
)

var kinds = []string{KindCount, KindSum, KindAvg, KindMin, KindMax, KindLast}

// Def is one row of `_computed_fields`.
type Def struct {
	Id               string `json:"id,omitempty"`
	Collection       string `json:"collection"`
	Field            string `json:"field"`
	Kind             string `json:"kind"`
	SourceCollection string `json:"source_collection"`
	SourceRelation   string `json:"source_relation"`
	SourceField      string `json:"source_field,omitempty"`
	Filter           string `json:"filter,omitempty"`
}

func (d *Def) key() string { return d.Collection + "." + d.Field }

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects definition changes, backfills and drift to an audit
// log. Modules must not import each other, so the wiring is in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// AllowManual reports whether superusers may write computed fields by hand.
func AllowManual() bool {
	v := strings.TrimSpace(os.Getenv(EnvAllowManual))
	return v == "1" || strings.EqualFold(v, "true")
}

// Module holds the cached definitions and the recompute locks.
type Module struct {
	app core.App

	mu       sync.RWMutex
	bySource map[string][]*Def // source collection name -> defs
	byParent map[string][]*Def // parent collection name -> defs
	loaded   time.Time
	invalid  bool
	gen      uint64
	loadMu   sync.Mutex

	prev sync.Map // *core.Record -> map[relation field]previous parent id

	locksMu sync.Mutex
	locks   map[string]*entry

	// parentWrites counts parent saves made by the module (tests, metrics).
	parentWrites counter
}

type counter struct {
	mu sync.Mutex
	n  int64
}

func (c *counter) add() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *counter) get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// ParentWrites returns how many parent records the module has saved.
func (m *Module) ParentWrites() int64 { return m.parentWrites.get() }

// Register creates the collection (if needed), binds the hooks, registers the
// job handlers and the daily drift check, and returns the module.
func Register(app core.App) *Module {
	m := &Module{app: app, invalid: true, locks: map[string]*entry{}}

	ensure := func() {
		if err := EnsureCollection(app); err != nil {
			app.Logger().Error("computed: failed to initialize "+CollectionName, "error", err)
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
	m.bindHooks()
	m.registerJobs()
	return m
}

// EnsureCollection creates the `_computed_fields` system collection when missing.
func EnsureCollection(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(CollectionName); c != nil {
		return nil
	}
	c := core.NewBaseCollection(CollectionName)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "collection", Required: true},
		&core.TextField{Name: "field", Required: true},
		&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: kinds},
		&core.TextField{Name: "source_collection", Required: true},
		&core.TextField{Name: "source_relation", Required: true},
		&core.TextField{Name: "source_field"},
		&core.TextField{Name: "filter"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_computed_fields_unique", true, "[[collection]], [[field]]", "")
	return app.Save(c)
}

// Invalidate drops the cache; the next lookup reloads it.
func (m *Module) Invalidate() {
	m.mu.Lock()
	m.invalid = true
	m.gen++
	m.mu.Unlock()
}

func toDef(r *core.Record) Def {
	return Def{
		Id: r.Id, Collection: r.GetString("collection"), Field: r.GetString("field"),
		Kind: r.GetString("kind"), SourceCollection: r.GetString("source_collection"),
		SourceRelation: r.GetString("source_relation"), SourceField: r.GetString("source_field"),
		Filter: r.GetString("filter"),
	}
}

// List returns the stored definitions (optionally for one parent collection), sorted.
func List(app core.App, collection string) ([]Def, error) {
	recs, err := app.FindAllRecords(CollectionName)
	if err != nil {
		return nil, err
	}
	out := make([]Def, 0, len(recs))
	for _, r := range recs {
		d := toDef(r)
		if collection == "" || d.Collection == collection {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

func (m *Module) refresh() {
	m.mu.RLock()
	fresh := !m.invalid && time.Since(m.loaded) < cacheTTL
	m.mu.RUnlock()
	if fresh {
		return
	}
	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	m.mu.RLock()
	fresh = !m.invalid && time.Since(m.loaded) < cacheTTL
	gen := m.gen
	m.mu.RUnlock()
	if fresh {
		return
	}
	defs, err := List(m.app, "")
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		// keep serving the previous definitions; retry on the next TTL
		m.loaded = time.Now()
		m.app.Logger().Warn("computed: failed to load definitions", "error", err)
		return
	}
	src, par := map[string][]*Def{}, map[string][]*Def{}
	for i := range defs {
		d := defs[i]
		src[d.SourceCollection] = append(src[d.SourceCollection], &d)
		par[d.Collection] = append(par[d.Collection], &d)
	}
	m.bySource, m.byParent, m.loaded = src, par, time.Now()
	m.invalid = m.gen != gen // an Invalidate raced with this load
}

func (m *Module) defsForSource(name string) []*Def {
	m.refresh()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bySource[name]
}

func (m *Module) defsForParent(name string) []*Def {
	m.refresh()
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byParent[name]
}

// Validate checks d against the live schema and canonicalizes collection
// names (a collection id is replaced by its name).
func Validate(app core.App, d *Def) error {
	d.Kind = strings.ToLower(strings.TrimSpace(d.Kind))
	if !contains(kinds, d.Kind) {
		return fmt.Errorf("kind must be one of %s", strings.Join(kinds, "|"))
	}
	parent, err := app.FindCollectionByNameOrId(d.Collection)
	if err != nil {
		return fmt.Errorf("collection %q not found", d.Collection)
	}
	d.Collection = parent.Name
	pf, ok := parent.Fields.GetByName(d.Field).(*core.NumberField)
	if !ok {
		return fmt.Errorf("field %q of %q must be an existing number field", d.Field, parent.Name)
	}
	_ = pf
	child, err := app.FindCollectionByNameOrId(d.SourceCollection)
	if err != nil {
		return fmt.Errorf("source collection %q not found", d.SourceCollection)
	}
	d.SourceCollection = child.Name
	if child.IsView() {
		return errors.New("source collection must not be a view")
	}
	rel, ok := child.Fields.GetByName(d.SourceRelation).(*core.RelationField)
	if !ok || rel.CollectionId != parent.Id {
		return fmt.Errorf("source_relation %q must be a relation field of %q pointing to %q", d.SourceRelation, child.Name, parent.Name)
	}
	if rel.IsMultiple() {
		return fmt.Errorf("source_relation %q must be a single relation (maxSelect 1)", d.SourceRelation)
	}
	if d.Kind == KindCount {
		if d.SourceField != "" {
			return errors.New("source_field must be empty for kind count")
		}
	} else {
		if d.SourceField == "" {
			return fmt.Errorf("source_field is required for kind %s", d.Kind)
		}
		if _, ok := child.Fields.GetByName(d.SourceField).(*core.NumberField); !ok {
			return fmt.Errorf("source_field %q of %q must be a number field", d.SourceField, child.Name)
		}
	}
	if d.Kind == KindLast && child.Fields.GetByName("created") == nil {
		return fmt.Errorf("kind last needs a `created` field on %q", child.Name)
	}
	if _, err := buildQuery(app, child, d, nil); err != nil {
		return fmt.Errorf("invalid filter: %w", err)
	}
	return nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Add validates and creates (or updates, by (collection, field)) a definition.
func Add(app core.App, d Def) (*Def, error) {
	if err := Validate(app, &d); err != nil {
		return nil, err
	}
	rec, err := app.FindFirstRecordByFilter(CollectionName, "collection={:c} && field={:f}", dbx.Params{"c": d.Collection, "f": d.Field})
	if err != nil || rec == nil {
		coll, cerr := app.FindCollectionByNameOrId(CollectionName)
		if cerr != nil {
			return nil, cerr
		}
		rec = core.NewRecord(coll)
	}
	rec.Set("collection", d.Collection)
	rec.Set("field", d.Field)
	rec.Set("kind", d.Kind)
	rec.Set("source_collection", d.SourceCollection)
	rec.Set("source_relation", d.SourceRelation)
	rec.Set("source_field", d.SourceField)
	rec.Set("filter", d.Filter)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	out := toDef(rec)
	return &out, nil
}

// Remove deletes a definition; false when none existed. The stored values stay as they are.
func Remove(app core.App, collection, field string) (bool, error) {
	if col, err := app.FindCollectionByNameOrId(collection); err == nil {
		collection = col.Name
	}
	rec, err := app.FindFirstRecordByFilter(CollectionName, "collection={:c} && field={:f}", dbx.Params{"c": collection, "f": field})
	if err != nil || rec == nil {
		return false, nil
	}
	return true, app.Delete(rec)
}

// find returns the stored definitions of collection (and field when not empty).
func find(app core.App, collection, field string) ([]Def, error) {
	if col, err := app.FindCollectionByNameOrId(collection); err == nil {
		collection = col.Name
	}
	all, err := List(app, collection)
	if err != nil {
		return nil, err
	}
	var out []Def
	for _, d := range all {
		if field == "" || d.Field == field {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no computed field definition for %s%s", collection, map[bool]string{true: "." + field}[field != ""])
	}
	return out, nil
}

func differs(stored, want float64) bool {
	return math.Abs(stored-want) > 1e-9*math.Max(1, math.Abs(want))
}
