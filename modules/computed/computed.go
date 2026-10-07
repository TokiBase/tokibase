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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/dbutils"
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
	Collection       string `json:"collection"` // name (resolved from the stored id)
	CollectionId     string `json:"collection_id,omitempty"`
	Field            string `json:"field"`
	Kind             string `json:"kind"`
	SourceCollection string `json:"source_collection"` // name (resolved from the stored id)
	SourceCollId     string `json:"source_collection_id,omitempty"`
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

	// prev holds, per record object, the FIFO queue of relation values captured
	// before each update (one entry per update call, consumed by its after
	// hook), so several updates of one record in a transaction and updates of
	// self-relations never lose or leak an entry.
	prev sync.Map // *core.Record -> *prevQueue

	// clientSaves marks the record objects that are being saved by a client
	// request (set by the request hooks, read by the execute hook).
	clientSaves sync.Map // *core.Record -> struct{}

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
		m.bootWarn()
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

// collRef resolves a stored collection reference (id; a name is accepted for
// rows written by hand or by older versions) to its id and current name.
func collRef(app kernel.App, ref string) (id, name string) {
	if c, err := app.FindCachedCollectionByNameOrId(ref); err == nil && c != nil {
		return c.Id, c.Name
	}
	return "", ref
}

// toDef reads a definition row. Definitions are stored by collection id, so a
// collection rename keeps them working; the names are resolved here.
func toDef(app kernel.App, r *core.Record) Def {
	cid, cname := collRef(app, r.GetString("collection"))
	sid, sname := collRef(app, r.GetString("source_collection"))
	return Def{
		Id: r.Id, Collection: cname, CollectionId: cid, Field: r.GetString("field"),
		Kind: r.GetString("kind"), SourceCollection: sname, SourceCollId: sid,
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
		d := toDef(app, r)
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
	d.Collection, d.CollectionId = parent.Name, parent.Id
	pf, ok := parent.Fields.GetByName(d.Field).(*core.NumberField)
	if !ok {
		return fmt.Errorf("field %q of %q must be an existing number field", d.Field, parent.Name)
	}
	_ = pf
	child, err := app.FindCollectionByNameOrId(d.SourceCollection)
	if err != nil {
		return fmt.Errorf("source collection %q not found", d.SourceCollection)
	}
	d.SourceCollection, d.SourceCollId = child.Name, child.Id
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
	for _, f := range kernel.SensitiveFieldsOf(child.Id) {
		if d.Filter != "" && wordRe(f).MatchString(d.Filter) {
			return fmt.Errorf("filter references the encrypted field %q of %q: it would compare ciphertext", f, child.Name)
		}
	}
	if _, err := buildQuery(app, child, d, nil); err != nil {
		return fmt.Errorf("invalid filter: %w", err)
	}
	// run it once against no parent: catches what compiling alone does not
	// (deleted columns, joins that no longer resolve)
	if _, err := aggregate(app, d, []string{"__validate__"}); err != nil {
		return fmt.Errorf("invalid definition: %w", err)
	}
	return nil
}

func wordRe(w string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(w) + `($|[^A-Za-z0-9_])`)
}

// MissingIndex returns a warning when the child has no index that starts with
// the relation column: every child write aggregates the children of one parent
// and without that index it scans the whole child table.
func MissingIndex(app kernel.App, d Def) string {
	child, err := app.FindCollectionByNameOrId(d.SourceCollection)
	if err != nil {
		return ""
	}
	for _, ix := range child.Indexes {
		cols := dbutils.ParseIndex(ix).Columns
		if len(cols) > 0 && strings.EqualFold(cols[0].Name, d.SourceRelation) {
			return ""
		}
	}
	return fmt.Sprintf("%s.%s has no index starting with the relation column: every write to %s scans the whole table. Run `toki computed index %s %s`.",
		child.Name, d.SourceRelation, child.Name, child.Name, d.SourceRelation)
}

// EnsureIndex creates a plain index on the child collection (relation column
// first, then the optional extra columns) through the collection indexes API.
// It returns the index name and whether it was created.
func EnsureIndex(app core.App, collection, field string, extra ...string) (string, bool, error) {
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return "", false, fmt.Errorf("collection %q not found", collection)
	}
	if col.Fields.GetByName(field) == nil {
		return "", false, fmt.Errorf("collection %q has no field %q", col.Name, field)
	}
	cols := append([]string{field}, extra...)
	for _, c := range extra {
		if col.Fields.GetByName(c) == nil {
			return "", false, fmt.Errorf("collection %q has no field %q", col.Name, c)
		}
	}
	for _, ix := range col.Indexes {
		parsed := dbutils.ParseIndex(ix)
		if len(parsed.Columns) >= len(cols) {
			same := true
			for i, c := range cols {
				if !strings.EqualFold(parsed.Columns[i].Name, c) {
					same = false
				}
			}
			if same {
				return parsed.IndexName, false, nil
			}
		}
	}
	name := "idx_computed_" + col.Name + "_" + strings.Join(cols, "_")
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "[[" + c + "]]"
	}
	col.AddIndex(name, false, strings.Join(quoted, ", "), "")
	if err := app.Save(col); err != nil {
		return "", false, err
	}
	return name, true, nil
}

func (m *Module) bootWarn() {
	c, err := m.app.FindCollectionByNameOrId(CollectionName)
	if err != nil || c == nil {
		return
	}
	for name, r := range map[string]*string{"listRule": c.ListRule, "viewRule": c.ViewRule, "createRule": c.CreateRule, "updateRule": c.UpdateRule, "deleteRule": c.DeleteRule} {
		if r != nil {
			m.app.Logger().Warn("computed: "+CollectionName+" has an API rule set; it must stay null (superusers only), "+
				"a definition can point at any number field", "rule", name)
		}
	}
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
	rec, err := app.FindFirstRecordByFilter(CollectionName, "(collection={:c} || collection={:n}) && field={:f}",
		dbx.Params{"c": d.CollectionId, "n": d.Collection, "f": d.Field})
	if err != nil || rec == nil {
		coll, cerr := app.FindCollectionByNameOrId(CollectionName)
		if cerr != nil {
			return nil, cerr
		}
		rec = core.NewRecord(coll)
	}
	rec.Set("collection", d.CollectionId) // ids: a collection rename must not orphan the definition
	rec.Set("field", d.Field)
	rec.Set("kind", d.Kind)
	rec.Set("source_collection", d.SourceCollId)
	rec.Set("source_relation", d.SourceRelation)
	rec.Set("source_field", d.SourceField)
	rec.Set("filter", d.Filter)
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	out := toDef(app, rec)
	return &out, nil
}

// Remove deletes a definition; false when none existed. The stored values stay as they are.
func Remove(app core.App, collection, field string) (bool, error) {
	id := collection
	if col, err := app.FindCollectionByNameOrId(collection); err == nil {
		collection, id = col.Name, col.Id
	}
	rec, err := app.FindFirstRecordByFilter(CollectionName, "(collection={:c} || collection={:n}) && field={:f}",
		dbx.Params{"c": id, "n": collection, "f": field})
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
