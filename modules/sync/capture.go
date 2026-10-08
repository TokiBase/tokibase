//go:build !no_sync

package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	stdsync "sync"
	"sync/atomic"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
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
	floorH   hlc.HLC // floor written by this tx (0 = none); reset when rolled back
}

func (m *Module) bindCapture() {
	app := m.app
	// request stash: record pointer -> actor (the same pattern as modules/wasm)
	stash := &hook.Handler[*core.RecordRequestEvent]{Id: hookId + "stash", Priority: -1000, Func: func(e *core.RecordRequestEvent) error {
		m.stash.Store(e.Record, m.actorIDFor(e.App, e.Auth))
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
		col := e.Record.Collection()
		if !m.ready.Load() {
			if b := m.initErr.Load(); b != nil && eligible(col) {
				// fail closed: a node whose capture failed to initialize must
				// not accept writes it cannot record
				return errf("not initialized, write to %q refused: %w", col.Name, b.err)
			}
			return e.Next()
		}
		p, err := m.pol.For(col)
		if err != nil {
			return errf("policy unavailable, write to %q refused: %w", col.Name, err)
		}
		if p == nil {
			return e.Next()
		}
		origin := kernel.SyncOriginFrom(e.Context)
		if origin != nil && origin.Mode == kernel.SyncModePush {
			// hub replay of a pushed change (apis.ReplayRecordRequests): the hub apply
			// writes the hub `_changes` row itself. Origin autodate values go in
			// the record before the autodate interceptor sees it.
			applyOriginDates(e.Record, origin)
			return e.Next()
		}
		orig := e.App
		nested := orig.TxInfo() != nil
		err = orig.RunInTransaction(func(tx kernel.App) error {
			e.App = tx
			return m.atomically(tx, nested, func() error {
				return m.capture(tx, e, op, p, origin)
			})
		})
		e.App = orig
		return err
	}
}

var savepointSeq atomic.Uint64

// atomically runs fn (record write + change rows) so that it either fully
// happens or leaves nothing behind in tx.
//
//   - The first statement is a write (no-op update of `_sync_state`), which
//     takes the SQLite write lock before any SELECT. A deferred transaction
//     that reads first and upgrades later fails at once with
//     SQLITE_BUSY_SNAPSHOT when another connection committed in between
//     (busy_timeout and the lock retry do not help inside a stale snapshot).
//   - Inside an outer transaction (batch, hook, user RunInTransaction) the
//     work runs in a SAVEPOINT: when fn fails, the record write is rolled back
//     even if the caller swallows the error and commits the outer transaction.
func (m *Module) atomically(tx kernel.App, nested bool, fn func() error) error {
	db := tx.NonconcurrentDB()
	if _, err := db.NewQuery("UPDATE _sync_state SET value=value WHERE key={:k}").
		Bind(dbx.Params{"k": keyNodeID}).Execute(); err != nil {
		return err
	}
	if !nested {
		return fn()
	}
	sp := fmt.Sprintf("sync_cap_%d", savepointSeq.Add(1))
	if _, err := db.NewQuery("SAVEPOINT " + sp).Execute(); err != nil {
		return err
	}
	var saved txState
	st := m.txStateOf(tx)
	if st != nil {
		st.mu.Lock()
		saved.n, saved.firstSeq, saved.id, saved.floorH = st.n, st.firstSeq, st.id, st.floorH
		st.mu.Unlock()
	}
	if err := fn(); err != nil {
		if _, rerr := db.NewQuery("ROLLBACK TO " + sp).Execute(); rerr != nil {
			return fmt.Errorf("%w (savepoint rollback failed: %v)", err, rerr)
		}
		_, _ = db.NewQuery("RELEASE " + sp).Execute()
		if st != nil {
			st.mu.Lock()
			st.n, st.firstSeq, st.id, st.floorH = saved.n, saved.firstSeq, saved.id, saved.floorH
			st.mu.Unlock()
		}
		return err
	}
	_, err := db.NewQuery("RELEASE " + sp).Execute()
	return err
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
			if pre, err = fieldValues(old, fields, p.Types); err != nil {
				return err
			}
		}
		var err error
		if baseHLC, err = metaHLC(db, col.Id, id); err != nil {
			return err
		}
	case OpDelete:
		var err error
		if baseHLC, err = metaHLC(db, col.Id, id); err != nil {
			return err
		}
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
		if post, err = fieldValues(rec, fields, p.Types); err != nil {
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
			// nothing but derived/autodate changes: no change row, but the
			// stored row did change, so keep _sync_meta.hash equal to its hash
			return refreshMetaHash(db, col.Id, id, hash)
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
		return deleteMeta(db, col.Id, id)
	default:
		if err := upsertMeta(db, col.Id, id, h, node, hash); err != nil {
			return err
		}
		if p.Strategy == StratFieldMerge {
			// a local write on the hub raises the clock of the plain fields it
			// changed, so that a concurrent push cannot silently overwrite it
			return bumpFieldClocks(db, col.Id, id, plainFields(p.Types, patch), hlc.HLC(h))
		}
		return nil
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
		return deleteMeta(db, col.Id, rec.Id)
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
	if !due || (st != nil && st.floorH != 0) {
		return nil
	}
	if err := hlc.SaveFloor(dbState{db: tx.NonconcurrentDB()}, h); err != nil {
		return err
	}
	if st != nil {
		st.mu.Lock()
		st.floorH = h
		st.mu.Unlock()
	}
	if info := tx.TxInfo(); info != nil {
		info.OnComplete(func(txErr error) error {
			if txErr != nil {
				return nil
			}
			if st != nil {
				st.mu.Lock()
				kept := st.floorH == h // false when a savepoint rolled the write back
				st.mu.Unlock()
				if !kept {
					return nil
				}
			}
			clock.Persisted(h)
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

// OriginFieldKey is the key of kernel.SyncOrigin.Fields for the origin value of
// an autodate field of one record: "<collectionId>/<recordId>/<field>".
func OriginFieldKey(colID, recID, field string) string { return colID + "/" + recID + "/" + field }

// applyOriginDates puts the origin created/updated values of a replayed
// change into rec with SetRaw, so the autodate interceptor keeps them (it only
// regenerates a value equal to the one loaded).
func applyOriginDates(rec *core.Record, o *kernel.SyncOrigin) {
	if len(o.Fields) == 0 {
		return
	}
	col := rec.Collection()
	for _, f := range col.Fields {
		if f.Type() != kernel.FieldTypeAutodate {
			continue
		}
		v, ok := o.Fields[OriginFieldKey(col.Id, rec.Id, f.GetName())]
		if !ok {
			continue
		}
		if s, ok := v.(string); ok {
			if dt, err := types.ParseDateTime(s); err == nil && !dt.IsZero() {
				rec.SetRaw(f.GetName(), dt)
			}
		}
	}
}
