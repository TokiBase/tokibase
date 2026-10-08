//go:build !no_sync

package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
	"github.com/tokibase/tokibase/tools/hook"
)

// Schema versioning and migration bundles (docs/SYNC_DESIGN.md §3.8).
//
// The hub keeps an integer schema version in `_sync_state`. Every change of the
// synced collections or of the config rows that travel with them produces a
// bundle: a FULL snapshot (not a diff), so applying any version is idempotent.
// A new version is only cut when the content hash differs from the latest
// bundle, so a trigger that changes nothing relevant (an unsynced collection)
// does not bump anything.

// Env of the schema part.
const (
	// EnvMaxBundles is the number of bundles the hub keeps and the longest
	// schema lag a spoke may have before it must re-bootstrap (default 50).
	EnvMaxBundles = "TOKI_SYNC_MAX_BUNDLES"
	// EnvSchemaLock=off lifts the schema lock of an enrolled spoke (escape hatch).
	EnvSchemaLock = "TOKI_SYNC_SCHEMA_LOCK"

	// DefaultMaxBundles is the default of EnvMaxBundles.
	DefaultMaxBundles = 50
)

// configCollections are the rows shipped with a bundle (design §3.8).
var configCollections = []string{PoliciesCollection, "_field_rules", "_batch_rules", "_computed_fields", "_crypto_fields"}

// KindSchemaDroppedField is the conflict kind of a field removed from a patch.
const (
	KindSchemaDroppedField = "schema_dropped_field"
	KindOrphaned           = "orphaned"
)

// ErrSchemaLocked is returned when a local collection change is refused on an
// enrolled spoke.
var ErrSchemaLocked = errors.New("sync: the schema of this spoke is managed by the hub, local collection changes are refused")

func init() {
	ddl = append(ddl,
		// hub: one full snapshot per schema version
		`CREATE TABLE IF NOT EXISTS _sync_schema (
  version INTEGER PRIMARY KEY,
  hash    TEXT NOT NULL,
  bundle  TEXT NOT NULL,
  created TEXT NOT NULL
)`,
		// spoke: the ranges received from the hub (docs/SYNC_DESIGN.md §2.8)
		`CREATE TABLE IF NOT EXISTS _sync_reserved (
  id       TEXT PRIMARY KEY,
  sequence TEXT    NOT NULL,
  start    INTEGER NOT NULL,
  "end"    INTEGER NOT NULL,
  next     INTEGER NOT NULL,
  status   TEXT    NOT NULL DEFAULT 'active',
  expires  TEXT    NOT NULL DEFAULT ''
)`,
		`CREATE INDEX IF NOT EXISTS idx__sync_reserved_seq ON _sync_reserved (sequence, status, start)`,
	)
}

// pr8State is the state of the schema, reservation and drift parts.
type pr8State struct {
	bundleMu stdsync.Mutex
	// defs caches the parsed field maps of the stored bundles (immutable per version).
	defsMu stdsync.Mutex
	defs   map[int64]*bundleDefs
	// bundleTxs marks the transactions that apply a bundle (the schema lock lets
	// them through).
	bundleTxs stdsync.Map // *kernel.TxAppInfo -> struct{}
	// rehash collects the collections to rehash after the bundles were applied.
	rehashMu stdsync.Mutex
	rehash   []string
}

func (m *Module) maxBundles() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvMaxBundles))); err == nil && n > 0 {
		return n
	}
	return DefaultMaxBundles
}

// ---- hub: building and storing bundles --------------------------------------

// secretKeys are the token secrets of an auth collection: they never leave the hub.
var secretKeys = []string{"authToken", "passwordResetToken", "emailChangeToken", "verificationToken", "fileToken"}

// exportCollection is the JSON form of a collection for a bundle: ids kept,
// timestamps and secrets (token secrets, OAuth2 client secrets) removed. A new
// auth collection on a spoke gets fresh local secrets from the model factory.
func exportCollection(col *core.Collection) (map[string]any, error) {
	raw, err := json.Marshal(col)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	delete(out, "created")
	delete(out, "updated")
	for _, k := range secretKeys {
		if tc, ok := out[k].(map[string]any); ok {
			delete(tc, "secret")
		}
	}
	if o, ok := out["oauth2"].(map[string]any); ok {
		if ps, ok := o["providers"].([]any); ok {
			for _, p := range ps {
				if pm, ok := p.(map[string]any); ok {
					delete(pm, "clientSecret")
				}
			}
		}
	}
	return out, nil
}

// syncedCollections lists the collections that have an enabled policy with a
// direction other than none, ordered by policy order then name.
func (m *Module) syncedCollections() ([]*core.Collection, map[string]struct{}, error) {
	recs, err := m.app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return nil, nil, err
	}
	type ent struct {
		col   *core.Collection
		order int
	}
	var ents []ent
	seen := map[string]struct{}{}
	for _, r := range recs {
		if !r.GetBool("enabled") || r.GetString("direction") == DirNone {
			continue
		}
		col, err := m.app.FindCollectionByNameOrId(r.GetString("collection"))
		if err != nil || col == nil {
			continue
		}
		if _, dup := seen[col.Id]; dup {
			continue
		}
		seen[col.Id] = struct{}{}
		ents = append(ents, ent{col, r.GetInt("order")})
	}
	sort.SliceStable(ents, func(i, j int) bool {
		if ents[i].order != ents[j].order {
			return ents[i].order < ents[j].order
		}
		return ents[i].col.Name < ents[j].col.Name
	})
	cols := make([]*core.Collection, len(ents))
	ids := map[string]struct{}{}
	for i, e := range ents {
		cols[i] = e.col
		ids[e.col.Id] = struct{}{}
		ids[e.col.Name] = struct{}{}
	}
	return cols, ids, nil
}

// buildBody renders the current state of the synced schema and config rows.
func (m *Module) buildBody() (*proto.BundleBody, error) {
	cols, refs, err := m.syncedCollections()
	if err != nil {
		return nil, err
	}
	body := &proto.BundleBody{Collections: []map[string]any{}, Config: map[string][]map[string]any{}}
	for _, c := range cols {
		ex, err := exportCollection(c)
		if err != nil {
			return nil, err
		}
		body.Collections = append(body.Collections, ex)
	}
	for _, name := range configCollections {
		if !m.app.HasTable(name) {
			continue
		}
		recs, err := m.app.FindAllRecords(name)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		for _, r := range recs {
			if r.Collection().Fields.GetByName("collection") != nil {
				if _, ok := refs[r.GetString("collection")]; !ok {
					continue
				}
			}
			if name == PoliciesCollection && (!r.GetBool("enabled") || r.GetString("direction") == DirNone) {
				continue
			}
			row := map[string]any{}
			for k, v := range r.FieldsData() {
				if k == "created" || k == "updated" {
					continue
				}
				row[k] = v
			}
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]["id"]) < fmt.Sprint(rows[j]["id"]) })
		body.Config[name] = rows
	}
	return body, nil
}

// buildBundle renders the current schema state as canonical JSON and its hash.
func (m *Module) buildBundle() (canon []byte, hash string, empty bool, err error) {
	body, err := m.buildBody()
	if err != nil {
		return nil, "", false, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, "", false, err
	}
	canon, err = proto.CanonicalJSON(raw)
	if err != nil {
		return nil, "", false, err
	}
	sum := sha256.Sum256(canon)
	nrows := 0
	for _, rows := range body.Config {
		nrows += len(rows)
	}
	return canon, hex.EncodeToString(sum[:]), len(body.Collections) == 0 && nrows == 0, nil
}

// colSigs reduces a bundle to one signature per collection: the field names
// and types plus the part of the policy that decides which values are hashed.
// A collection whose signature changed needs its record hashes recomputed
// (`_sync_meta.hash` holds a hash over the field names).
func colSigs(body *proto.BundleBody) map[string]string {
	pol := map[string]map[string]any{}
	for _, r := range body.Config[PoliciesCollection] {
		ref, _ := r["collection"].(string)
		pol[ref] = r
	}
	out := make(map[string]string, len(body.Collections))
	for _, c := range body.Collections {
		id, _ := c["id"].(string)
		name, _ := c["name"].(string)
		var fields []string
		if fs, ok := c["fields"].([]any); ok {
			for _, f := range fs {
				if fm, ok := f.(map[string]any); ok {
					fields = append(fields, fmt.Sprint(fm["name"], ":", fm["type"]))
				}
			}
		}
		sort.Strings(fields)
		p := pol[id]
		if p == nil {
			p = pol[name]
		}
		sig := map[string]any{"fields": fields}
		if p != nil {
			sig["exclude"], sig["field_types"] = p["exclude"], p["field_types"]
		}
		raw, _ := json.Marshal(sig)
		sum := sha256.Sum256(raw)
		out[id] = hex.EncodeToString(sum[:8])
	}
	return out
}

// changedSigs lists the collections of next whose signature differs from prev.
func changedSigs(prev, next map[string]string) []string {
	var out []string
	for id, s := range next {
		if prev[id] != s {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// rehashCollections recomputes `_sync_meta.hash` of every record of the
// collections (after a field was added, renamed or dropped the stored hash no
// longer matches the record, and `toki sync verify` would report all of them).
func (m *Module) rehashCollections(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	m.pol.invalidate()
	for _, id := range ids {
		col, err := m.app.FindCollectionByNameOrId(id)
		if err != nil || col == nil {
			continue
		}
		p, err := m.pol.For(col)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		for offset := 0; ; offset += 500 {
			recs, err := m.app.FindRecordsByFilter(col, "", "id", 500, offset)
			if err != nil {
				return err
			}
			if len(recs) == 0 {
				break
			}
			err = m.app.RunInTransaction(func(tx kernel.App) error {
				for _, r := range recs {
					h, err := RecordHash(r, p)
					if err != nil {
						return err
					}
					if _, err := tx.NonconcurrentDB().NewQuery("UPDATE _sync_meta SET hash={:h} WHERE collection={:c} AND record={:r}").
						Bind(dbx.Params{"h": h, "c": col.Id, "r": r.Id}).Execute(); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			if len(recs) < 500 {
				break
			}
		}
	}
	return nil
}

// refreshBundle cuts a new schema version when the synced schema or the config
// rows differ from the latest bundle. It is idempotent and cheap, so it runs
// from every trigger and at every handshake.
func (m *Module) refreshBundle() (int64, error) {
	if m.role != RoleHub || !m.ready.Load() {
		return 0, nil
	}
	m.p8.bundleMu.Lock()
	defer m.p8.bundleMu.Unlock()
	canon, hash, empty, err := m.buildBundle()
	if err != nil {
		return 0, err
	}
	cur := m.schemaVersion()
	var stored int64
	_ = m.app.DB().NewQuery("SELECT COALESCE(MAX(version),0) FROM _sync_schema").Row(&stored)
	cur = max(cur, stored)
	if cur > 0 {
		var last string
		if err := m.app.DB().NewQuery("SELECT hash FROM _sync_schema WHERE version={:v}").Bind(dbx.Params{"v": cur}).Row(&last); err == nil && last == hash {
			if m.schemaVersion() != cur {
				return cur, (dbState{db: m.app.NonconcurrentDB()}).Set(keySchemaVersion, strconv.FormatInt(cur, 10))
			}
			return cur, nil
		}
	} else if empty {
		return 0, nil
	}
	next := cur + 1
	var changed []string
	if cur > 0 {
		var prevRaw string
		var prev, nxt proto.BundleBody
		if m.app.DB().NewQuery("SELECT bundle FROM _sync_schema WHERE version={:v}").Bind(dbx.Params{"v": cur}).Row(&prevRaw) == nil &&
			json.Unmarshal([]byte(prevRaw), &prev) == nil && json.Unmarshal(canon, &nxt) == nil {
			changed = changedSigs(colSigs(&prev), colSigs(&nxt))
		}
	}
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		db := tx.NonconcurrentDB()
		if _, err := db.NewQuery("INSERT INTO _sync_schema (version, hash, bundle, created) VALUES ({:v},{:h},{:b},{:c})").
			Bind(dbx.Params{"v": next, "h": hash, "b": string(canon), "c": m.created()}).Execute(); err != nil {
			return err
		}
		if err := (dbState{db: db}).Set(keySchemaVersion, strconv.FormatInt(next, 10)); err != nil {
			return err
		}
		_, err := db.NewQuery("DELETE FROM _sync_schema WHERE version <= {:v}").Bind(dbx.Params{"v": next - int64(m.maxBundles())}).Execute()
		return err
	})
	if err != nil {
		return cur, err
	}
	m.p8.defsMu.Lock()
	m.p8.defs = nil
	m.p8.defsMu.Unlock()
	if err := m.rehashCollections(changed); err != nil {
		m.app.Logger().Error("sync: failed to recompute the record hashes after a schema change", "error", err)
	}
	emit(AuditSchemaBundle, "", "", map[string]any{"version": next, "hash": hash})
	m.app.Logger().Info("sync: schema version", "version", next)
	return next, nil
}

// AuditSchemaBundle is the audit action of a new schema version.
const AuditSchemaBundle = "sync.schema"

// bindSchema binds the hub triggers of new bundles and the spoke schema lock.
func (m *Module) bindSchema() {
	app := m.app
	switch m.role {
	case RoleHub:
		bump := func(e *core.CollectionEvent) error {
			err := e.Next()
			if err == nil {
				m.bumpAsync()
			}
			return err
		}
		app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "bundle", Func: bump})
		app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "bundle", Func: bump})
		app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "bundle", Func: bump})
		rb := func(e *core.RecordEvent) error {
			err := e.Next()
			if err == nil {
				m.bumpAsync()
			}
			return err
		}
		for _, name := range configCollections {
			app.OnRecordAfterCreateSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "bundle-" + name, Func: rb})
			app.OnRecordAfterUpdateSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "bundle-" + name, Func: rb})
			app.OnRecordAfterDeleteSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "bundle-" + name, Func: rb})
		}
	case RoleSpoke:
		lock := func(e *core.CollectionEvent) error {
			if m.schemaLocked(e.App, e.Collection) {
				return ErrSchemaLocked
			}
			return e.Next()
		}
		app.OnCollectionCreate().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "lock", Priority: -1 << 19, Func: lock})
		app.OnCollectionUpdate().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "lock", Priority: -1 << 19, Func: lock})
		app.OnCollectionDelete().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "lock", Priority: -1 << 19, Func: lock})
	}
}

// bumpAsync refreshes the bundle after a trigger. A failure is logged only: the
// next trigger or handshake cuts the version (the hash comparison heals it).
func (m *Module) bumpAsync() {
	if _, err := m.refreshBundle(); err != nil {
		m.app.Logger().Error("sync: failed to cut a schema version", "error", err)
	}
}

// schemaLocked reports whether a collection change must be refused: an
// enrolled spoke takes its schema from the hub (design §3.8). System
// collections and the bundle transaction itself pass.
func (m *Module) schemaLocked(app kernel.App, col *core.Collection) bool {
	if col == nil || col.System || strings.HasPrefix(col.Name, "_") {
		return false
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvSchemaLock))); v == "off" || v == "0" || v == "false" {
		return false
	}
	if info := app.TxInfo(); info != nil {
		if _, ok := m.p8.bundleTxs.Load(info); ok {
			return false
		}
	}
	if !m.ready.Load() || !app.HasTable("_sync_cursors") {
		return false
	}
	var n int
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM _sync_cursors").Row(&n); err != nil {
		return false
	}
	return n > 0
}

// ---- hub: handshake and push -------------------------------------------------

// oldestBundle returns the lowest stored version (0 = none).
func (m *Module) oldestBundle() int64 {
	var v int64
	_ = m.app.DB().NewQuery("SELECT COALESCE(MIN(version),0) FROM _sync_schema").Row(&v)
	return v
}

// handshakeSchema answers the schema section for a node at spokeVersion. It
// also reports whether the node has to re-bootstrap: its version is older than
// the oldest kept bundle or more than max bundles behind. A node at version 0
// (fresh) gets the latest bundle only; being a full snapshot it needs no
// history.
func (m *Module) handshakeSchema(spokeVersion int64) (proto.Schema, bool) {
	out := proto.Schema{Bundles: []proto.SchemaBundle{}}
	if _, err := m.refreshBundle(); err != nil {
		m.app.Logger().Error("sync: failed to refresh the schema bundle", "error", err)
	}
	hub := m.schemaVersion()
	out.Version = hub
	if hub == 0 || spokeVersion >= hub {
		return out, false
	}
	from := spokeVersion + 1
	if spokeVersion <= 0 {
		from = hub // fresh node: the latest full snapshot is enough
	} else if oldest := m.oldestBundle(); from < oldest || hub-spokeVersion > int64(m.maxBundles()) {
		return out, true
	}
	var rows []struct {
		Version int64  `db:"version"`
		Hash    string `db:"hash"`
		Bundle  string `db:"bundle"`
	}
	if err := m.app.DB().NewQuery("SELECT version, hash, bundle FROM _sync_schema WHERE version>={:f} ORDER BY version").
		Bind(dbx.Params{"f": from}).All(&rows); err != nil {
		m.app.Logger().Error("sync: failed to read the schema bundles", "error", err)
		return out, false
	}
	for _, r := range rows {
		out.Bundles = append(out.Bundles, proto.SchemaBundle{Version: r.Version, Hash: r.Hash, Bundle: json.RawMessage(r.Bundle)})
	}
	return out, false
}

// bundleDefs is the field map of one stored bundle: collection id -> name ->
// field id, and the inverse.
type bundleDefs struct {
	byName map[string]map[string]string
}

func parseBundleDefs(raw string) (*bundleDefs, error) {
	var body struct {
		Collections []struct {
			ID     string `json:"id"`
			Fields []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"fields"`
		} `json:"collections"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return nil, err
	}
	d := &bundleDefs{byName: map[string]map[string]string{}}
	for _, c := range body.Collections {
		fm := make(map[string]string, len(c.Fields))
		for _, f := range c.Fields {
			fm[f.Name] = f.ID
		}
		d.byName[c.ID] = fm
	}
	return d, nil
}

// defsOf returns the field map of a stored version (nil when the version was
// pruned or never existed).
func (m *Module) defsOf(tx kernel.App, version int64) (*bundleDefs, error) {
	m.p8.defsMu.Lock()
	if d, ok := m.p8.defs[version]; ok {
		m.p8.defsMu.Unlock()
		return d, nil
	}
	m.p8.defsMu.Unlock()
	var raw string
	err := tx.DB().NewQuery("SELECT bundle FROM _sync_schema WHERE version={:v}").Bind(dbx.Params{"v": version}).Row(&raw)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	d, err := parseBundleDefs(raw)
	if err != nil {
		return nil, err
	}
	m.p8.defsMu.Lock()
	if m.p8.defs == nil {
		m.p8.defs = map[int64]*bundleDefs{}
	}
	m.p8.defs[version] = d
	m.p8.defsMu.Unlock()
	return d, nil
}

// mapSchema maps the field names of a pushed patch from the schema version it
// was captured under (c.SV) to the current ones through the field ids
// (docs/SYNC_DESIGN.md §3.8). Fields that no longer exist are removed from the
// patch and recorded as a resolved `schema_dropped_field` conflict. A change
// older than the oldest kept bundle cannot be mapped: it is refused as
// `orphaned`.
func (m *Module) mapSchema(tx kernel.App, nodeID string, c *hubChange, col *core.Collection) *rejection {
	hub := m.schemaVersion()
	if c.SV <= 0 || hub == 0 || c.SV >= hub || len(c.patch) == 0 {
		return nil
	}
	old, err := m.defsOf(tx, c.SV)
	if err != nil {
		return &rejection{code: CodeApplyError, msg: err.Error(), internal: false}
	}
	if old == nil {
		rj := reject(proto.CodeOrphaned, fmt.Sprintf("the change was captured under schema version %d, which the hub no longer keeps: re-bootstrap", c.SV))
		rj.change = c
		rj.conflict = &ConflictInfo{Kind: KindOrphaned, Strategy: StratLWW, Resolution: ResolutionRejected, Status: ConflictResolved,
			Incoming: c.patch, Note: rj.msg}
		return rj
	}
	oldFields := old.byName[col.Id]
	if oldFields == nil {
		return nil
	}
	cur := make(map[string]string, len(col.Fields))
	for _, f := range col.Fields {
		cur[f.GetId()] = f.GetName()
	}
	var dropped []string
	renamed := map[string]any{}
	for _, k := range sortedKeys(c.patch) {
		id, ok := oldFields[k]
		if !ok {
			continue // unknown at that version: the apply loop ignores it
		}
		nn, ok := cur[id]
		switch {
		case !ok:
			dropped = append(dropped, k)
		case nn != k:
			renamed[k] = nn
		}
	}
	if len(dropped) == 0 && len(renamed) == 0 {
		return nil
	}
	droppedVals := map[string]any{}
	for _, k := range dropped {
		droppedVals[k] = c.patch[k]
		delete(c.patch, k)
	}
	// two passes: a rename onto the name another field just left must not clash
	moved := map[string]any{}
	for k, nn := range renamed {
		moved[nn.(string)] = c.patch[k]
		delete(c.patch, k)
	}
	for k, v := range moved {
		c.patch[k] = v
	}
	if len(dropped) > 0 {
		ci := &ConflictInfo{Kind: KindSchemaDroppedField, Strategy: StratLWW, Resolution: ResolutionAutoMerge, Status: ConflictResolved,
			Incoming: droppedVals, Note: fmt.Sprintf("fields removed from the schema since version %d were dropped from the change: %s", c.SV, strings.Join(dropped, ", "))}
		if err := m.writeConflict(tx, nodeID, c, col, ci); err != nil {
			return &rejection{code: CodeApplyError, msg: err.Error()}
		}
	}
	return nil
}

// ---- spoke: applying a bundle --------------------------------------------------

var _ client.BundleBackend = backend{}

// ApplyBundle applies one bundle inside tx: it removes the local fields the
// bundle no longer has, imports the collections (ids kept), replaces the config
// rows and returns the computed fields whose definition changed (the client
// queues a backfill for them after the commit). The origin is Bundle, which
// lets the schema lock pass.
func (b backend) ApplyBundle(tx kernel.App, sb proto.SchemaBundle) ([][2]string, error) {
	return b.m.applyBundle(tx, sb)
}

// AfterBundles runs after the bundles of a handshake were applied and committed:
// it recomputes the record hashes of the collections that changed.
func (b backend) AfterBundles(ctx context.Context) error {
	m := b.m
	m.p8.rehashMu.Lock()
	ids := m.p8.rehash
	m.p8.rehash = nil
	m.p8.rehashMu.Unlock()
	seen := map[string]bool{}
	var uniq []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	return m.rehashCollections(uniq)
}

func (m *Module) applyBundle(tx kernel.App, sb proto.SchemaBundle) ([][2]string, error) {
	var body proto.BundleBody
	if err := json.Unmarshal(sb.Bundle, &body); err != nil {
		return nil, fmt.Errorf("sync: invalid bundle %d: %w", sb.Version, err)
	}
	// the collections whose fields or hashed values change need new record hashes afterwards
	if cur, err := m.buildBody(); err == nil {
		m.p8.rehashMu.Lock()
		m.p8.rehash = append(m.p8.rehash, changedSigs(colSigs(cur), colSigs(&body))...)
		m.p8.rehashMu.Unlock()
	}
	if info := tx.TxInfo(); info != nil {
		m.p8.bundleTxs.Store(info, struct{}{})
		defer m.p8.bundleTxs.Delete(info)
	}
	ctx := kernel.WithSyncOrigin(context.Background(), &kernel.SyncOrigin{Mode: kernel.SyncModeBundle, Node: m.NodeID(), ChangeID: "bundle:" + strconv.FormatInt(sb.Version, 10)})

	if len(body.Collections) > 0 {
		if err := dropMissingFields(tx, ctx, body.Collections); err != nil {
			return nil, err
		}
		if err := tx.ImportCollections(body.Collections, false); err != nil {
			return nil, fmt.Errorf("sync: bundle %d: %w", sb.Version, err)
		}
	}
	var backfill [][2]string
	// the policies are always replaced (the hub is the authority); the other
	// config collections only when this build has them
	names := append([]string{}, configCollections...)
	for _, name := range names {
		rows, present := body.Config[name]
		if !present && name != PoliciesCollection {
			continue
		}
		if !tx.HasTable(name) {
			continue
		}
		bf, err := replaceRows(tx, ctx, name, rows)
		if err != nil {
			return nil, fmt.Errorf("sync: bundle %d: %s: %w", sb.Version, name, err)
		}
		backfill = append(backfill, bf...)
	}
	m.pol.invalidate()
	return backfill, nil
}

// dropMissingFields removes from the local collections the fields that the
// bundle no longer has. ImportCollections(…, false) keeps existing fields, and
// a stale column would make the record hashes differ from the hub's.
func dropMissingFields(tx kernel.App, ctx context.Context, defs []map[string]any) error {
	for _, d := range defs {
		id, _ := d["id"].(string)
		if id == "" {
			continue
		}
		col, err := tx.FindCollectionByNameOrId(id)
		if err != nil || col == nil {
			continue
		}
		want := map[string]struct{}{}
		if fs, ok := d["fields"].([]any); ok {
			for _, f := range fs {
				if fm, ok := f.(map[string]any); ok {
					if fid, _ := fm["id"].(string); fid != "" {
						want[fid] = struct{}{}
					}
				}
			}
		}
		changed := false
		for _, f := range col.Fields {
			if f.GetSystem() {
				continue
			}
			if _, ok := want[f.GetId()]; !ok {
				col.Fields.RemoveById(f.GetId())
				changed = true
			}
		}
		if changed {
			if err := tx.SaveNoValidateWithContext(ctx, col); err != nil {
				return fmt.Errorf("sync: dropping fields of %q: %w", col.Name, err)
			}
		}
	}
	return nil
}

// replaceRows makes the rows of a config collection equal to rows (by id). For
// `_computed_fields` it returns the (collection, field) pairs that are new or
// changed.
func replaceRows(tx kernel.App, ctx context.Context, name string, rows []map[string]any) ([][2]string, error) {
	col, err := tx.FindCollectionByNameOrId(name)
	if err != nil {
		return nil, err
	}
	existing, err := tx.FindAllRecords(col)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*core.Record, len(existing))
	for _, r := range existing {
		byID[r.Id] = r
	}
	var backfill [][2]string
	keep := map[string]struct{}{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		keep[id] = struct{}{}
		rec := byID[id]
		isNew := rec == nil
		if isNew {
			rec = core.NewRecord(col)
			rec.Set("id", id)
		}
		changed := isNew
		for k, v := range row {
			if k == "id" || k == "created" || k == "updated" {
				continue
			}
			if col.Fields.GetByName(k) == nil {
				continue
			}
			if !isNew && sameValue(rec.Get(k), v) {
				continue
			}
			rec.Set(k, v)
			changed = true
		}
		if !changed {
			continue
		}
		if err := tx.SaveNoValidateWithContext(ctx, rec); err != nil {
			return nil, err
		}
		if name == "_computed_fields" {
			backfill = append(backfill, [2]string{rec.GetString("collection"), rec.GetString("field")})
		}
	}
	for id, r := range byID {
		if _, ok := keep[id]; !ok {
			if err := tx.DeleteWithContext(ctx, r); err != nil {
				return nil, err
			}
		}
	}
	return backfill, nil
}

// sameValue compares a stored record value with a bundle value by JSON form.
func sameValue(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
