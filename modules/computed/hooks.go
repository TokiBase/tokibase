//go:build !no_computed

package computed

import (
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	// writePriority runs before every other record request handler.
	writePriority = -1 << 20

	// ErrCode / ErrMessage are the validation error returned for a client write.
	ErrCode    = "validation_computed_field"
	ErrMessage = "This field is maintained by the server and cannot be changed."
)

func bodyField(k string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, "+"), "+"), "-")
}

func (m *Module) bindHooks() {
	// child writes (fired after the transaction committed)
	child := func(op string) func(e *core.RecordEvent) error {
		return func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			// consume the captured previous value first: a marked (module) write
			// returns below, and its entry must not stay behind
			var prev map[string]string
			if op == "update" {
				prev = m.popPrev(e.Record)
			}
			if isMarked(e.Context) {
				return nil
			}
			m.onChild(e.Record, op, prev)
			return nil
		}
	}
	app := m.app
	app.OnRecordAfterCreateSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "child", Func: child("create")})
	app.OnRecordAfterUpdateSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "child", Func: child("update")})
	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId + "child", Func: child("delete")})

	// the stored record is reset after a save, so the previous relation value
	// is captured before the update and consumed by the after-hook
	app.OnRecordUpdate().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "pre",
		Func: func(e *core.RecordEvent) error {
			pushed := false
			if orig := e.Record.Original(); orig != nil && !isMarked(e.Context) {
				old := map[string]string{}
				for _, d := range m.defsForSource(e.Record.Collection().Name) {
					old[d.SourceRelation] = orig.GetString(d.SourceRelation)
				}
				if len(old) > 0 {
					m.pushPrev(e.Record, old)
					pushed = true
				}
			}
			err := e.Next()
			if err != nil && pushed {
				m.dropLastPrev(e.Record)
			}
			return err
		},
	})
	app.OnRecordAfterUpdateError().Bind(&hook.Handler[*core.RecordErrorEvent]{
		Id: hookId + "preerr",
		Func: func(e *core.RecordErrorEvent) error {
			// the update (and so its after-success hook) never happened, e.g. the
			// transaction rolled back: drop what the pre hook captured for it
			m.dropLastPrev(e.Record)
			return e.Next()
		},
	})

	// definitions are stored by collection id: renames and deletes change what
	// the names resolve to
	invalidate := func(e *core.CollectionEvent) error {
		err := e.Next()
		m.Invalidate()
		return err
	}
	app.OnCollectionAfterCreateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "coll", Func: invalidate})
	app.OnCollectionAfterUpdateSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "coll", Func: invalidate})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId + "coll", Func: invalidate})

	// definitions: validate on every write path, invalidate the cache, audit
	app.OnRecordValidate(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "validate",
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			d := toDef(m.app, e.Record)
			if err := Validate(m.app, &d); err != nil {
				return validation.Errors{"field": validation.NewError("validation_computed_definition", err.Error())}
			}
			// normalize to ids so a rename of either collection keeps working
			e.Record.Set("collection", d.CollectionId)
			e.Record.Set("source_collection", d.SourceCollId)
			if w := MissingIndex(m.app, d); w != "" {
				m.app.Logger().Warn("computed: " + w)
			}
			return nil
		},
	})
	def := func(action string) func(e *core.RecordEvent) error {
		return func(e *core.RecordEvent) error {
			err := e.Next()
			m.Invalidate()
			if err == nil {
				d := toDef(m.app, e.Record)
				audit(action, d.Collection, d.Field, map[string]any{
					"kind": d.Kind, "source_collection": d.SourceCollection,
					"source_relation": d.SourceRelation, "source_field": d.SourceField, "filter": d.Filter,
				})
			}
			return err
		}
	}
	app.OnRecordAfterCreateSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: def(ActionDefCreate)})
	app.OnRecordAfterUpdateSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: def(ActionDefUpdate)})
	app.OnRecordAfterDeleteSuccess(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: def(ActionDefDelete)})

	// client writes to computed fields are rejected
	app.OnRecordCreateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: writePriority,
		Func: func(e *core.RecordRequestEvent) error {
			if err := m.guard(e, true); err != nil {
				return err
			}
			return e.Next()
		},
	})
	app.OnRecordUpdateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: writePriority,
		Func: func(e *core.RecordRequestEvent) error {
			if err := m.guard(e, false); err != nil {
				return err
			}
			if len(m.defsForParent(e.Collection.Name)) > 0 && !m.manualAllowed(e) {
				m.clientSaves.Store(e.Record, struct{}{})
				defer m.clientSaves.Delete(e.Record)
			}
			return e.Next()
		},
	})
	// Inside the write transaction: a client save never carries a stale value
	// of a computed column. The record was loaded at request start; the column
	// may have been recomputed since, and the save writes every column. Reset
	// the computed fields to the value stored right now.
	app.OnRecordUpdateExecute().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "reset", Priority: -1 << 20,
		Func: func(e *core.RecordEvent) error {
			if _, ok := m.clientSaves.Load(e.Record); ok && !isMarked(e.Context) {
				m.resetComputed(e)
			}
			return e.Next()
		},
	})
}

// manualAllowed reports whether this request may write computed fields by hand.
func (m *Module) manualAllowed(e *core.RecordRequestEvent) bool {
	if !AllowManual() {
		return false
	}
	info, err := e.RequestInfo()
	return err == nil && info != nil && info.HasSuperuserAuth()
}

// resetComputed sets the computed fields of e.Record to their stored values
// (read through the transaction that is about to write).
func (m *Module) resetComputed(e *core.RecordEvent) {
	col := e.Record.Collection()
	for _, d := range m.defsForParent(col.Name) {
		var v float64
		err := e.App.DB().Select("COALESCE([[" + d.Field + "]], 0)").From(col.Name).Where(dbx.HashExp{"id": e.Record.Id}).Row(&v)
		if err != nil {
			m.app.Logger().Warn("computed: cannot read the stored value, keeping the submitted one",
				"field", d.key(), "record", e.Record.Id, "error", err)
			continue
		}
		e.Record.Set(d.Field, v)
	}
}

// prevQueue is the FIFO of relation values captured before each update of one
// record object.
type prevQueue struct {
	mu    sync.Mutex
	items []map[string]string
}

func (m *Module) pushPrev(r *core.Record, old map[string]string) {
	v, _ := m.prev.LoadOrStore(r, &prevQueue{})
	q := v.(*prevQueue)
	q.mu.Lock()
	q.items = append(q.items, old)
	q.mu.Unlock()
}

// popPrev returns the oldest captured entry (the one of the update whose
// after-hook is running) and forgets the record when its queue is empty.
func (m *Module) popPrev(r *core.Record) map[string]string {
	v, ok := m.prev.Load(r)
	if !ok {
		return nil
	}
	q := v.(*prevQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		m.prev.Delete(r)
		return nil
	}
	out := q.items[0]
	q.items = q.items[1:]
	if len(q.items) == 0 {
		m.prev.Delete(r)
	}
	return out
}

// dropLastPrev forgets the newest entry (the update it belonged to failed).
func (m *Module) dropLastPrev(r *core.Record) {
	v, ok := m.prev.Load(r)
	if !ok {
		return
	}
	q := v.(*prevQueue)
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.items); n > 0 {
		q.items = q.items[:n-1]
	}
	if len(q.items) == 0 {
		m.prev.Delete(r)
	}
}

// onChild recomputes the parents touched by a committed child write. Errors
// are logged, not returned: the child write is already committed.
func (m *Module) onChild(rec *core.Record, op string, prev map[string]string) {
	defs := m.defsForSource(rec.Collection().Name)
	for _, d := range defs {
		ids := map[string]struct{}{}
		if id := rec.GetString(d.SourceRelation); id != "" {
			ids[id] = struct{}{}
		}
		if old, ok := prev[d.SourceRelation]; ok && old != "" {
			ids[old] = struct{}{}
		}
		for id := range ids {
			_, err := m.Recompute(d, id)
			if err != nil { // one retry: another process may have held the write lock
				time.Sleep(50 * time.Millisecond)
				_, err = m.Recompute(d, id)
			}
			if err != nil {
				m.app.Logger().Error("computed: recompute failed",
					"field", d.key(), "parent", id, "error", err)
			}
		}
	}
}

func (m *Module) guard(e *core.RecordRequestEvent, create bool) error {
	defs := m.defsForParent(e.Collection.Name)
	if len(defs) == 0 {
		return nil
	}
	info, err := e.RequestInfo()
	if err != nil || info == nil {
		return nil
	}
	if info.HasSuperuserAuth() && AllowManual() {
		return nil
	}
	denied := validation.Errors{}
	for k := range info.Body {
		name := bodyField(k)
		for _, d := range defs {
			if d.Field != name {
				continue
			}
			if create {
				if e.Record.GetFloat(name) == 0 {
					continue
				}
			} else if orig := e.Record.Original(); orig != nil && !differs(e.Record.GetFloat(name), orig.GetFloat(name)) {
				continue // not a change: re-sending the stored value is allowed
			}
			denied[name] = validation.NewError(ErrCode, ErrMessage)
		}
	}
	if len(denied) == 0 {
		return nil
	}
	msg := "Failed to update record."
	if create {
		msg = "Failed to create record."
	}
	return e.BadRequestError(msg, denied)
}
