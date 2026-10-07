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
// the plaintext, in place. A value that cannot be decrypted becomes "" and is
// logged (and sampled into the audit sink); it never panics.
func (m *Module) decryptRecord(rec *core.Record) error {
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
			setStored(rec, field, "", isJSON)
			errs = append(errs, fmt.Errorf("%s.%s: %w", col.Name, field, err))
			continue
		}
		setStored(rec, field, plain, isJSON)
	}
	return errors.Join(errs...)
}

func (m *Module) decryptFailed(col *core.Collection, field, recId string, err error) {
	m.app.Logger().Error("crypto: decryption failed, value replaced by empty string",
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
// this is called. Failed values become "" and the joined error is returned.
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
	if col == nil || isCryptoSystem(col.Name) || e.Record.IsNew() || !m.Active() {
		return e.Next()
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil || len(cfg) == 0 {
		return e.Next()
	}
	for field := range cfg {
		isJSON := isJSONField(col, field)
		stored := storedOf(e.Record, field)
		if _, ok := ctOf(stored, isJSON); !ok {
			continue
		}
		if p, err := m.openStored(col, field, e.Record.Id, stored, isJSON); err == nil {
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

	for field, mode := range cfg {
		f := col.Fields.GetByName(field)
		if f == nil {
			continue
		}
		isJSON := f.Type() == core.FieldTypeJSON
		cur := storedOf(e.Record, field)
		origStored := ""
		if !isNew {
			origStored = storedOf(orig, field)
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
