//go:build !no_crypto

package crypto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

var allowedTypes = map[string]bool{
	core.FieldTypeText: true, core.FieldTypeEditor: true, core.FieldTypeJSON: true,
	core.FieldTypeEmail: true, core.FieldTypeURL: true,
}

func isCryptoSystem(name string) bool {
	return name == FieldsCollection || name == KeysCollection
}

func storedOf(rec *core.Record, name string) string {
	switch v := rec.GetRaw(name).(type) {
	case nil:
		return ""
	case string:
		return v
	case types.JSONRaw:
		return string(v)
	case []byte:
		return string(v)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func setStored(rec *core.Record, name, stored string, isJSON bool) {
	if isJSON {
		if stored == "" {
			rec.SetRaw(name, types.JSONRaw(nil))
			return
		}
		rec.SetRaw(name, types.JSONRaw(stored))
		return
	}
	rec.SetRaw(name, stored)
}

func isJSONField(col *core.Collection, name string) bool {
	f := col.Fields.GetByName(name)
	return f != nil && f.Type() == core.FieldTypeJSON
}

func (m *Module) bindHooks() {
	a := m.app

	a.OnRecordValidate().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: 0, Func: m.onValidate,
	})
	a.OnRecordCreateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: 0, Func: m.onWrite,
	})
	a.OnRecordUpdateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: 0, Func: m.onWrite,
	})
	a.OnRecordDeleteExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: 0, Func: m.onDelete,
	})
	a.OnRecordEnrich().Bind(&hook.Handler[*core.RecordEnrichEvent]{
		Id: hookId, Priority: 0,
		Func: func(e *core.RecordEnrichEvent) error {
			if e.Record != nil && !(e.RequestInfo != nil && e.RequestInfo.HasSuperuserAuth() && !AdminPlaintext()) {
				_ = m.decryptRecord(e.Record)
			}
			return e.Next()
		},
	})
	a.OnCollectionUpdateExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId, Priority: -1 << 10, Func: m.guardCollectionSave,
	})
	a.OnCollectionCreateExecute().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId, Priority: -1 << 10, Func: m.guardCollectionSave,
	})
	a.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			err := e.Next()
			if err == nil {
				m.dropCollection(e.Collection)
			}
			return err
		},
	})
	m.bindHTTP()
}

// dropCollection removes configuration, keys and index rows of a deleted collection.
func (m *Module) dropCollection(c *core.Collection) {
	if isCryptoSystem(c.Name) {
		return
	}
	db := m.app.NonconcurrentDB()
	for _, q := range []string{
		"DELETE FROM {{" + FieldsCollection + "}} WHERE collection={:c}",
		"DELETE FROM {{" + KeysCollection + "}} WHERE collection={:c}",
		"DELETE FROM {{" + IndexTable + "}} WHERE collection={:c}",
	} {
		if _, err := db.NewQuery(q).Bind(dbx.Params{"c": c.Id}).Execute(); err != nil {
			m.app.Logger().Warn("crypto: cleanup after collection delete failed", "collection", c.Name, "error", err)
		}
	}
	m.Invalidate()
}

func (m *Module) aad(col *core.Collection, field, recId string) []byte {
	return aadFor(col.Id, field, recId)
}

// openStored decrypts one stored column value.
func (m *Module) openStored(col *core.Collection, field, recId, stored string, isJSON bool) (string, error) {
	ct, ok := ctOf(stored, isJSON)
	if !ok {
		return stored, nil // legacy plaintext (enable still backfilling)
	}
	k, err := m.keysFor(col.Id)
	if err != nil {
		return "", err
	}
	p, err := open(ct, m.aad(col, field, recId), k.keyFor)
	if err != nil {
		return "", err
	}
	return string(p), nil
}

// decryptRecord replaces every ciphertext of the record's encrypted fields by
// the plaintext, in place, including the records of its expand tree. A value
// that cannot be decrypted becomes the sentinel [Undecryptable] (never "", so a
// client that sends the whole record back cannot erase the stored value) and is
// logged (and sampled into the audit sink); it never panics.
func (m *Module) decryptRecord(rec *core.Record) error {
	var errs []error
	m.decryptTree(rec, &errs, 0)
	return errors.Join(errs...)
}

func (m *Module) decryptTree(rec *core.Record, errs *[]error, depth int) {
	if rec == nil || depth > 6 {
		return
	}
	if err := m.decryptOne(rec); err != nil {
		*errs = append(*errs, err)
	}
	for _, v := range rec.Expand() {
		switch t := v.(type) {
		case *core.Record:
			m.decryptTree(t, errs, depth+1)
		case []*core.Record:
			for _, r := range t {
				m.decryptTree(r, errs, depth+1)
			}
		}
	}
}

func (m *Module) decryptOne(rec *core.Record) error {
	col := rec.Collection()
	if col == nil || isCryptoSystem(col.Name) {
		return nil
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil || len(cfg) == 0 {
		return nil
	}
	var errs []error
	for field := range cfg {
		f := col.Fields.GetByName(field)
		if f == nil {
			continue
		}
		isJSON := f.Type() == core.FieldTypeJSON
		stored := storedOf(rec, field)
		if _, ok := ctOf(stored, isJSON); !ok {
			continue
		}
		var plain string
		if !m.Active() {
			err = ErrNoMasterKey
		} else {
			plain, err = m.openStored(col, field, rec.Id, stored, isJSON)
		}
		if err != nil {
			m.decryptFailed(col, field, rec.Id, err)
			if isJSON {
				setStored(rec, field, storedFromCT(Undecryptable, true), true)
			} else {
				setStored(rec, field, Undecryptable, false)
			}
			errs = append(errs, fmt.Errorf("%s.%s: %w", col.Name, field, err))
			continue
		}
		setStored(rec, field, plain, isJSON)
	}
	return errors.Join(errs...)
}

func (m *Module) decryptFailed(col *core.Collection, field, recId string, err error) {
	m.app.Logger().Error("crypto: decryption failed, value replaced by the undecryptable sentinel",
		"collection", col.Name, "field", field, "record", recId, "error", err)
	key := col.Id + "\x00" + field
	m.auditMu.Lock()
	last := m.auditLast[key]
	send := time.Since(last) >= time.Minute
	if send {
		m.auditLast[key] = time.Now()
	}
	m.auditMu.Unlock()
	if send {
		emit(ActionDecryptFailed, col.Name, recId, map[string]any{"field": field, "error": err.Error()})
	}
}

// Decrypt replaces ciphertext by plaintext in place on record, for Go and JS
// consumers. Inside record hooks, record.Get(field) returns ciphertext until
// this is called. Failed values become "[undecryptable]" and the joined error is returned.
func Decrypt(app kernel.App, record *core.Record) error {
	m := From(app)
	if m == nil || record == nil {
		return nil
	}
	return m.decryptRecord(record)
}

// onValidate decrypts untouched ciphertext so that field validation (email,
// url, pattern, max length) runs on the plaintext.
func (m *Module) onValidate(e *core.RecordEvent) error {
	col := e.Record.Collection()
	origin := kernel.SyncOriginFrom(e.Context)
	if col == nil || isCryptoSystem(col.Name) || (e.Record.IsNew() && origin == nil) || !m.Active() {
		return e.Next()
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil || len(cfg) == 0 {
		return e.Next()
	}
	for field := range cfg {
		isJSON := isJSONField(col, field)
		stored := storedOf(e.Record, field)
		ct, ok := ctOf(stored, isJSON)
		if !ok {
			continue
		}
		if p, err := m.openStored(col, field, e.Record.Id, stored, isJSON); err == nil {
			if origin != nil {
				// a sync apply: the stored form must stay the ciphertext that came over the
				// wire (same bytes, same hash on every node), see onWrite
				origin.Remember(syncCTKey{e.Record, field}, ct)
			}
			setStored(e.Record, field, p, isJSON)
		} // on failure the ciphertext stays; execute treats it as unchanged
	}
	return e.Next()
}

type indexOp struct {
	field string
	plain string
	drop  bool
}

func (m *Module) onWrite(e *core.RecordEvent) error {
	col := e.Record.Collection()
	if col == nil || isCryptoSystem(col.Name) {
		return e.Next()
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil {
		return err
	}
	if len(cfg) == 0 {
		return e.Next()
	}
	if !m.Active() {
		return fmt.Errorf("crypto: collection %q has encrypted fields but no master key is configured; refusing to write", col.Name)
	}
	if e.Record.Id == "" {
		return errors.New("crypto: record id is not set")
	}
	ver, dek, err := m.activeKey(col.Id)
	if err != nil {
		return err
	}
	isNew := e.Record.IsNew()
	var orig *core.Record
	if !isNew {
		orig = e.Record.Original()
	}
	var ops []indexOp
	origin := kernel.SyncOriginFrom(e.Context)

	for field, mode := range cfg {
		f := col.Fields.GetByName(field)
		if f == nil {
			continue
		}
		isJSON := f.Type() == core.FieldTypeJSON
		if m.stateOf(col.Id, field) == StateDisabling {
			continue // being disabled: new values are stored as plaintext, the sweep decrypts the rest
		}
		cur := storedOf(e.Record, field)
		origStored := ""
		if !isNew {
			origStored = storedOf(orig, field)
		}
		if origin != nil {
			op, handled, err := m.syncField(origin, col, e.Record, field, mode, cur, origStored, isNew, isJSON)
			if err != nil {
				return err
			}
			if handled {
				if op != nil {
					ops = append(ops, *op)
				}
				continue
			}
		}
		if !isNew && isSentinel(cur, isJSON) {
			if _, isCT := ctOf(origStored, isJSON); isCT {
				setStored(e.Record, field, origStored, isJSON) // a failed read came back: keep what is stored
				continue
			}
		}
		if _, isCT := ctOf(cur, isJSON); isCT && !isNew && cur == origStored {
			continue // untouched ciphertext
		}
		if emptyValue(cur, isJSON) {
			if mode == ModeBlindIndex {
				ops = append(ops, indexOp{field: field, drop: true})
			}
			continue
		}
		if !isNew && origStored != "" {
			if p, err := m.openStored(col, field, e.Record.Id, origStored, isJSON); err == nil && p == cur {
				if _, isCT := ctOf(origStored, isJSON); isCT {
					setStored(e.Record, field, origStored, isJSON) // same plaintext: keep the stored ciphertext
					continue
				}
			}
		}
		ct, err := seal(dek, ver, m.aad(col, field, e.Record.Id), []byte(cur))
		if err != nil {
			return err
		}
		setStored(e.Record, field, storedFromCT(ct, isJSON), isJSON)
		if mode == ModeBlindIndex {
			ops = append(ops, indexOp{field: field, plain: cur})
		}
	}

	if err := e.Next(); err != nil {
		return err
	}
	if len(ops) == 0 {
		return nil
	}
	ik, err := indexKey(dek, col.Id)
	if err != nil {
		return err
	}
	db := e.App.NonconcurrentDB()
	for _, op := range ops {
		if op.drop {
			err = indexDel(db, col.Id, op.field, e.Record.Id)
		} else {
			err = indexPut(db, col.Id, op.field, e.Record.Id, blindHMAC(ik, op.field, op.plain), ver)
		}
		if err != nil {
			m.app.Logger().Error("crypto: blind index update failed", "collection", col.Name, "field", op.field, "record", e.Record.Id, "error", err)
			return fmt.Errorf("crypto: blind index update failed: %w", err)
		}
	}
	return nil
}

// isSentinel reports whether a stored column value is the [Undecryptable] marker.
func isSentinel(stored string, isJSON bool) bool {
	if isJSON {
		return stored == storedFromCT(Undecryptable, true)
	}
	return stored == Undecryptable
}

func (m *Module) onDelete(e *core.RecordEvent) error {
	col := e.Record.Collection()
	if col == nil || isCryptoSystem(col.Name) {
		return e.Next()
	}
	cfg, _ := m.fieldsFor(col.Id)
	if err := e.Next(); err != nil {
		return err
	}
	if len(cfg) > 0 {
		if _, err := e.App.NonconcurrentDB().NewQuery("DELETE FROM {{" + IndexTable + "}} WHERE collection={:c} AND record={:r}").
			Bind(dbx.Params{"c": col.Id, "r": e.Record.Id}).Execute(); err != nil {
			m.app.Logger().Warn("crypto: failed to remove index rows", "record", e.Record.Id, "error", err)
		}
	}
	return nil
}

// ----- index table -----

type execer interface {
	NewQuery(string) *dbx.Query
}

func indexPut(db execer, collId, field, rec, h string, ver int) error {
	_, err := db.NewQuery("INSERT OR REPLACE INTO {{" + IndexTable + "}} (collection, field, record, hmac, ver) VALUES ({:c},{:f},{:r},{:h},{:v})").
		Bind(dbx.Params{"c": collId, "f": field, "r": rec, "h": h, "v": ver}).Execute()
	return err
}

func indexDel(db execer, collId, field, rec string) error {
	_, err := db.NewQuery("DELETE FROM {{" + IndexTable + "}} WHERE collection={:c} AND field={:f} AND record={:r}").
		Bind(dbx.Params{"c": collId, "f": field, "r": rec}).Execute()
	return err
}

var _ = strings.TrimSpace

// syncCTKey identifies a remembered ciphertext: the record being saved and the field.
type syncCTKey struct {
	rec   *core.Record
	field string
}

// syncField handles an encrypted field of a record written by a sync apply
// (docs/SYNC_DESIGN.md §7.6). The ciphertext that came over the wire is stored
// UNCHANGED so that the stored bytes, and with them the record hash, are the
// same on every node whatever its master key. It reports handled=false when
// the normal write path must run (a plaintext value, for example from a local
// edit).
//
//   - onValidate decrypted the value (so validators saw plaintext) and
//     remembered the ciphertext: it is put back when the plaintext is still the
//     decryption of it.
//   - The value is still a ciphertext (SaveNoValidate: pull, snapshot): it is
//     kept as is. One that cannot be decrypted is refused for a push (a node
//     must not be able to plant garbage), and stored verbatim for a pull or
//     snapshot (a historic change can carry a retired key version; the next
//     change of the record replaces it).
//
// The blind index is recomputed from the plaintext with the local copy of the
// same DEK, so it is identical on every node.
func (m *Module) syncField(origin *kernel.SyncOrigin, col *core.Collection, rec *core.Record, field, mode, cur, origStored string, isNew, isJSON bool) (*indexOp, bool, error) {
	key := syncCTKey{rec, field}
	if v, ok := origin.Recall(key); ok {
		origin.Forget(key)
		ct, _ := v.(string)
		stored := storedFromCT(ct, isJSON)
		if p, err := m.openStored(col, field, rec.Id, stored, isJSON); err == nil && p == cur {
			setStored(rec, field, stored, isJSON)
			if !isNew && stored == origStored {
				return nil, true, nil // unchanged: the index row is already right
			}
			if mode == ModeBlindIndex {
				return &indexOp{field: field, plain: p}, true, nil
			}
			return nil, true, nil
		}
		return nil, false, nil // a hook changed the plaintext: encrypt it normally
	}
	if _, isCT := ctOf(cur, isJSON); !isCT || (!isNew && cur == origStored) {
		return nil, false, nil
	}
	p, err := m.openStored(col, field, rec.Id, cur, isJSON)
	if err != nil {
		if ct, _ := ctOf(cur, isJSON); m.keyUnknown(col.Id, ct) && origin.Mode != kernel.SyncModePush {
			// a new key version (rotation) that the next handshake brings: retry then, with the index
			return nil, false, fmt.Errorf("%w (%s.%s)", kernel.ErrSyncKeyMissing, col.Name, field)
		}
		if origin.Mode == kernel.SyncModePush {
			return nil, false, fmt.Errorf("crypto: the ciphertext sent for %s.%s cannot be decrypted by the hub (unknown or retired key version, or it belongs to another record): refused", col.Name, field)
		}
		var op *indexOp
		if mode == ModeBlindIndex {
			op = &indexOp{field: field, drop: true}
		}
		m.app.Logger().Warn("crypto: stored a synced ciphertext that this node cannot decrypt", "collection", col.Name, "field", field, "record", rec.Id, "error", err)
		return op, true, nil
	}
	if mode == ModeBlindIndex {
		return &indexOp{field: field, plain: p}, true, nil
	}
	return nil, true, nil
}

// keyUnknown reports whether the ciphertext names a key version that this node
// has no row for at all (neither usable nor retired): the hub has not shipped it
// yet.
func (m *Module) keyUnknown(collId, ct string) bool {
	ver, _, ok := parseCT(ct)
	if !ok {
		return false
	}
	k, err := m.keysFor(collId)
	if err != nil {
		return false
	}
	if _, have := k.deks[ver]; have || k.retired[ver] {
		return false
	}
	return true
}
