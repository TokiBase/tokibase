package computed

import (
	"strings"

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
			if isMarked(e.Context) {
				return nil
			}
			m.onChild(e.Record, op)
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
			if orig := e.Record.Original(); orig != nil {
				old := map[string]string{}
				for _, d := range m.defsForSource(e.Record.Collection().Name) {
					old[d.SourceRelation] = orig.GetString(d.SourceRelation)
				}
				if len(old) > 0 {
					m.prev.Store(e.Record, old)
				}
			}
			err := e.Next()
			if err != nil {
				m.prev.Delete(e.Record)
			}
			return err
		},
	})
	app.OnRecordAfterUpdateError().Bind(&hook.Handler[*core.RecordErrorEvent]{
		Id: hookId + "preerr",
		Func: func(e *core.RecordErrorEvent) error {
			m.prev.Delete(e.Record)
			return e.Next()
		},
	})

	// definitions: validate on every write path, invalidate the cache, audit
	app.OnRecordValidate(CollectionName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "validate",
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			d := toDef(e.Record)
			if err := Validate(m.app, &d); err != nil {
				return validation.Errors{"field": validation.NewError("validation_computed_definition", err.Error())}
			}
			return nil
		},
	})
	def := func(action string) func(e *core.RecordEvent) error {
		return func(e *core.RecordEvent) error {
			err := e.Next()
			m.Invalidate()
			if err == nil {
				d := toDef(e.Record)
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
			return e.Next()
		},
	})
}

// onChild recomputes the parents touched by a committed child write. Errors
// are logged, not returned: the child write is already committed.
func (m *Module) onChild(rec *core.Record, op string) {
	defs := m.defsForSource(rec.Collection().Name)
	var prev map[string]string
	if op == "update" {
		if v, ok := m.prev.LoadAndDelete(rec); ok {
			prev = v.(map[string]string)
		}
	}
	for _, d := range defs {
		ids := map[string]struct{}{}
		if id := rec.GetString(d.SourceRelation); id != "" {
			ids[id] = struct{}{}
		}
		if old, ok := prev[d.SourceRelation]; ok && old != "" {
			ids[old] = struct{}{}
		}
		for id := range ids {
			if _, err := m.Recompute(d, id); err != nil {
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
