//go:build !no_sync

package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	stdsync "sync"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tools/hook"
)

// Change operations.
const (
	OpCreate = "c"
	OpUpdate = "u"
	OpDelete = "d"
	OpPurge  = "p"
)

// Status of a change row.
const (
	StatusLocal = "local"
)

// ActorNode is the actor of writes that have no authenticated record.
const ActorNode = "node"

// txState groups the changes of one database transaction (an atomic group):
// a /api/batch call, a cascade delete, or several saves in RunInTransaction.
type txState struct {
	mu       stdsync.Mutex
	n        int
	firstSeq int64
	id       string
	floorSet bool
}

func (m *Module) bindCapture() {
	app := m.app
	// request stash: record pointer -> actor (the same pattern as modules/wasm)
	stash := &hook.Handler[*core.RecordRequestEvent]{Id: hookId + "stash", Priority: -1000, Func: func(e *core.RecordRequestEvent) error {
		m.stash.Store(e.Record, actorOf(e.Auth))
		defer m.stash.Delete(e.Record)
		return e.Next()
	}}
	app.OnRecordCreateRequest().Bind(stash)
	app.OnRecordUpdateRequest().Bind(stash)
	app.OnRecordDeleteRequest().Bind(stash)

	app.OnRecordCreateExecute().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Priority: capturePriority, Func: m.onExecute(OpCreate)})
	app.OnRecordUpdateExecute().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Priority: capturePriority, Func: m.onExecute(OpUpdate)})
	app.OnRecordDeleteExecute().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Priority: capturePriority, Func: m.onExecute(OpDelete)})
}

// actorOf is the change actor of an authenticated request record. Grants
// arrive with PR4; until then the actor is "rec:<collectionId>:<id>".
func actorOf(auth *core.Record) string {
	if auth == nil {
		return ActorNode
	}
	return "rec:" + auth.Collection().Id + ":" + auth.Id
}

func (m *Module) actorFor(rec *core.Record) string {
	if v, ok := m.stash.Load(rec); ok {
		return v.(string)
	}
	return ActorNode
}

// onExecute wraps the write (e.Next) and the capture in one transaction. The
// kernel copies e.App to the model event before the DB write (see
// syncModelEventWithRecordEvent), so the insert into the record table runs in
// the same transaction as the _changes row; kernel.onRecordDeleteExecute uses
// the same pattern.
func (m *Module) onExecute(op string) func(e *core.RecordEvent) error {
	return func(e *core.RecordEvent) error {
		if !m.ready.Load() {
			return e.Next()
		}
		p := m.pol.For(e.Record.Collection())
		if p == nil {
			return e.Next()
		}
		origin := kernel.SyncOriginFrom(e.Context)
		if origin != nil && origin.Mode == kernel.SyncModePush {
			return e.Next() // hub replay of a pushed change: PR3
		}
		orig := e.App
		err := e.App.RunInTransaction(func(tx kernel.App) error {
			e.App = tx
			return m.capture(tx, e, op, p, origin)
		})
		e.App = orig
		return err
	}
}

func (m *Module) capture(tx kernel.App, e *core.RecordEvent, op string, p *policy, origin *kernel.SyncOrigin) error {
	rec := e.Record
	col := rec.Collection()
	replica := kernel.IsSyncReplica(e.Context)
	fields := syncedFields(col, p)
	db := tx.NonconcurrentDB()
	id := rec.Id

	var pre map[string]any
	var baseHLC int64
	switch op {
	case OpCreate:
		if !replica {
			if err := guardTombstone(db, col.Id, id); err != nil {
				return err
			}
		}
	case OpUpdate:
		if !replica {
			old, err := tx.FindRecordById(col.Id, id)
			if err != nil {
				return err
			}
			if pre, err = fieldValues(old, fields); err != nil {
				return err
			}
		}
		baseHLC = metaHLC(db, col.Id, id)
	case OpDelete:
		baseHLC = metaHLC(db, col.Id, id)
	}

	if err := e.Next(); err != nil {
		return err
	}

	if replica {
		return m.captureReplica(tx, op, rec, p, origin)
	}

	var post map[string]any
	var hash []byte
	patch := map[string]any{}
	if op != OpDelete {
		var err error
		if post, err = fieldValues(rec, fields); err != nil {
			return err
		}
		hash = canonicalHash(col.Id, id, post)
	}
	switch op {
	case OpCreate:
		for k, v := range post {
			patch[k] = v
		}
	case OpUpdate:
		var meaningful bool
		patch, meaningful = diffPatch(fields, p, pre, post)
		if !meaningful {
			return nil // nothing but derived/autodate changes: no change row
		}
	}

	clock := m.Clock()
	h := int64(clock.Now())
	node := m.NodeID()
	enc, err := encodePatch(patch)
	if err != nil {
		return err
	}
	if err := m.insertChange(tx, &change{
		node: node, hlc: h, baseHLC: baseHLC, collection: col.Id, record: id, op: op,
		patch: enc, hash: hash, actor: m.actorFor(rec),
	}); err != nil {
		return err
	}

	switch op {
	case OpDelete:
		if err := putTombstone(db, col.Id, id, "delete", h, node, m.actorFor(rec), "", m.created()); err != nil {
			return err
		}
		_, err = db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c} AND record={:r}").
			Bind(dbx.Params{"c": col.Id, "r": id}).Execute()
		return err
	default:
		return upsertMeta(db, col.Id, id, h, node, hash)
	}
}

// captureReplica handles pull/snapshot/bundle applies: no _changes row (spokes
// are never a source for others), only the per record clock and tombstones.
func (m *Module) captureReplica(tx kernel.App, op string, rec *core.Record, p *policy, origin *kernel.SyncOrigin) error {
	col := rec.Collection()
	db := tx.NonconcurrentDB()
	h := int64(0)
	node, actor := "", ""
	if origin != nil {
		h, node, actor = int64(origin.HLC), origin.Node, origin.Actor
		m.Clock().Observe(hlc.HLC(origin.HLC))
	}
	if op == OpDelete {
		if err := putTombstone(db, col.Id, rec.Id, "delete", h, node, actor, "", m.created()); err != nil {
			return err
		}
		_, err := db.NewQuery("DELETE FROM _sync_meta WHERE collection={:c} AND record={:r}").
			Bind(dbx.Params{"c": col.Id, "r": rec.Id}).Execute()
		return err
	}
	hash, err := RecordHash(rec, p)
	if err != nil {
		return err
	}
	return upsertMeta(db, col.Id, rec.Id, h, node, hash)
}

func (m *Module) created() string {
	return m.Clock().WallNow().UTC().Format("2006-01-02 15:04:05.000Z")
}

// diffPatch builds the update patch from the stored (pre) and new (post)
// values. It also reports whether anything besides autodate fields changed;
// a derived-only or autodate-only update is not worth a change row.
func diffPatch(fields []core.Field, p *policy, pre, post map[string]any) (map[string]any, bool) {
	patch := map[string]any{}
	meaningful := false
	for _, f := range fields {
		name := f.GetName()
		a, b := pre[name], post[name]
		if bytes.Equal(canonicalJSON(a), canonicalJSON(b)) {
			continue
		}
		if f.Type() != kernel.FieldTypeAutodate {
			meaningful = true
		}
		patch[name] = typedDelta(p.Types[name], a, b)
	}
	return patch, meaningful
}

// typedDelta renders a changed value. counter fields become {"$inc": delta}
// and set fields {"$add": [...], "$rm": [...]}; anything else is the new value.
func typedDelta(typ string, before, after any) any {
	switch typ {
	case TypeCounter:
		a, aok := before.(float64)
		b, bok := after.(float64)
		if aok && bok {
			return map[string]any{"$inc": b - a}
		}
		if bok && before == nil {
			return map[string]any{"$inc": b}
		}
	case TypeSet:
		as, aok := before.([]any)
		bs, bok := after.([]any)
		if (aok || before == nil) && (bok || after == nil) {
			inA, inB := map[string]bool{}, map[string]bool{}
			for _, v := range as {
				inA[string(canonicalJSON(v))] = true
			}
			for _, v := range bs {
				inB[string(canonicalJSON(v))] = true
			}
			add, rm := []any{}, []any{}
			for _, v := range bs {
				if !inA[string(canonicalJSON(v))] {
					add = append(add, v)
				}
			}
			for _, v := range as {
				if !inB[string(canonicalJSON(v))] {
					rm = append(rm, v)
				}
			}
			out := map[string]any{}
			if len(add) > 0 {
				out["$add"] = add
			}
			if len(rm) > 0 {
				out["$rm"] = rm
			}
			return out
		}
	}
	return after
}

func encodePatch(patch map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(patch); err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

type change struct {
	node       string
	hlc        int64
	baseHLC    int64
	collection string
	record     string
	op         string
	patch      string
	hash       []byte
	actor      string
}

// insertChange appends the row, assigns the atomic group id and persists the
// clock floor when it is due. All of it happens in the caller's transaction.
func (m *Module) insertChange(tx kernel.App, c *change) error {
	db := tx.NonconcurrentDB()
	var last int64
	if err := db.NewQuery("SELECT COALESCE((SELECT seq FROM sqlite_sequence WHERE name='_changes'),0)").Row(&last); err != nil {
		return err
	}
	seq := last + 1

	var schemaVersion int64
	if v, ok, err := (dbState{db: db}).Get(keySchemaVersion); err == nil && ok {
		schemaVersion, _ = strconv.ParseInt(v, 10, 64)
	}

	st := m.txStateOf(tx)
	txid := ""
	if st != nil {
		st.mu.Lock()
		st.n++
		switch st.n {
		case 1:
			st.firstSeq = seq
		case 2:
			st.id = randomHex(8)
			if _, err := db.NewQuery("UPDATE _changes SET tx={:t} WHERE seq={:s}").
				Bind(dbx.Params{"t": st.id, "s": st.firstSeq}).Execute(); err != nil {
				st.mu.Unlock()
				return err
			}
			txid = st.id
		default:
			txid = st.id
		}
		st.mu.Unlock()
	}

	var hashArg any
	if c.hash != nil {
		hashArg = c.hash
	}
	_, err := db.NewQuery(`INSERT INTO _changes
  (seq, node, origin_seq, hlc, base_hlc, collection, record, op, patch, hash, schema_version, actor, tx, status, created)
  VALUES ({:seq}, {:node}, {:seq}, {:hlc}, {:base}, {:col}, {:rec}, {:op}, {:patch}, {:hash}, {:sv}, {:actor}, {:tx}, {:status}, {:created})`).
		Bind(dbx.Params{
			"seq": seq, "node": c.node, "hlc": c.hlc, "base": c.baseHLC, "col": c.collection, "rec": c.record,
			"op": c.op, "patch": c.patch, "hash": hashArg, "sv": schemaVersion, "actor": c.actor, "tx": txid,
			"status": StatusLocal, "created": m.created(),
		}).Execute()
	if err != nil {
		return err
	}
	return m.maybePersistFloor(tx, st)
}

// maybePersistFloor writes hlc_floor in the running transaction every N ticks.
func (m *Module) maybePersistFloor(tx kernel.App, st *txState) error {
	clock := m.Clock()
	h, due := clock.NeedsFloor()
	if !due || (st != nil && st.floorSet) {
		return nil
	}
	if err := hlc.SaveFloor(dbState{db: tx.NonconcurrentDB()}, h); err != nil {
		return err
	}
	if st != nil {
		st.floorSet = true
	}
	if info := tx.TxInfo(); info != nil {
		info.OnComplete(func(txErr error) error {
			if txErr == nil {
				clock.Persisted(h)
			}
			return nil
		})
	}
	return nil
}

// txStateOf returns the group state of the transaction behind tx, creating it
// (and its cleanup) on first use.
func (m *Module) txStateOf(tx kernel.App) *txState {
	info := tx.TxInfo()
	if info == nil {
		return nil
	}
	if v, ok := m.txs.Load(info); ok {
		return v.(*txState)
	}
	st := &txState{}
	if v, loaded := m.txs.LoadOrStore(info, st); loaded {
		return v.(*txState)
	}
	info.OnComplete(func(error) error {
		m.txs.Delete(info)
		return nil
	})
	return st
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// metaHLC returns the record clock the writer saw (0 when unknown).
func metaHLC(db dbx.Builder, colId, id string) int64 {
	var h int64
	if err := db.NewQuery("SELECT hlc FROM _sync_meta WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&h); err != nil {
		return 0
	}
	return h
}

func upsertMeta(db dbx.Builder, colId, id string, h int64, node string, hash []byte) error {
	_, err := db.NewQuery(`INSERT INTO _sync_meta (collection, record, hlc, node, hash) VALUES ({:c}, {:r}, {:h}, {:n}, {:x})
  ON CONFLICT(collection, record) DO UPDATE SET hlc=excluded.hlc, node=excluded.node, hash=excluded.hash`).
		Bind(dbx.Params{"c": colId, "r": id, "h": h, "n": node, "x": hash}).Execute()
	return err
}

// guardTombstone refuses to create a record whose id has a tombstone.
func guardTombstone(db dbx.Builder, colId, id string) error {
	var n int
	if err := db.NewQuery("SELECT COUNT(*) FROM _sync_tombstones WHERE collection={:c} AND record={:r}").
		Bind(dbx.Params{"c": colId, "r": id}).Row(&n); err != nil {
		return err
	}
	if n > 0 {
		return validation.Errors{"id": validation.NewError("validation_sync_tombstoned",
			"This record id was deleted (tombstone) and cannot be created again.")}
	}
	return nil
}

func putTombstone(db dbx.Builder, colId, id, kind string, h int64, node, actor, reason, created string) error {
	_, err := db.NewQuery(`INSERT INTO _sync_tombstones (collection, record, kind, hlc, node, actor, reason, created)
  VALUES ({:c}, {:r}, {:k}, {:h}, {:n}, {:a}, {:why}, {:t})
  ON CONFLICT(collection, record) DO UPDATE SET hlc=excluded.hlc, node=excluded.node, actor=excluded.actor, created=excluded.created
  WHERE kind='delete'`).
		Bind(dbx.Params{"c": colId, "r": id, "k": kind, "h": h, "n": node, "a": actor, "why": reason, "t": created}).Execute()
	return err
}
