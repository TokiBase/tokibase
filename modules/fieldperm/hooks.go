package fieldperm

import (
	"strings"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	// readPriority: the field is hidden after the rest of the enrich chain (including the superuser un-hide finalizer) ran.
	readPriority = 1 << 20
	// writePriority runs before every other record request handler.
	writePriority = -1 << 20

	// ErrCode / ErrMessage are the validation error returned for a denied write.
	ErrCode    = "validation_field_not_allowed"
	ErrMessage = "You are not allowed to change this field."
)

func (m *Module) bindHooks() {
	// Read: OnRecordEnrich runs for every record that leaves through the
	// record API (list, view, create/update responses, realtime and expanded
	// relations), each with the request info of the current client.
	m.app.OnRecordEnrich().Bind(&hook.Handler[*core.RecordEnrichEvent]{
		Id: hookId, Priority: readPriority,
		Func: func(e *core.RecordEnrichEvent) error {
			// after Next: the upstream finalizer un-hides every field for superusers
			if err := e.Next(); err != nil {
				return err
			}
			m.enforceRead(e)
			return nil
		},
	})

	m.app.OnRecordCreateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: writePriority,
		Func: func(e *core.RecordRequestEvent) error {
			if err := m.enforceWrite(e, true); err != nil {
				return err
			}
			return e.Next()
		},
	})
	m.app.OnRecordUpdateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: writePriority,
		Func: func(e *core.RecordRequestEvent) error {
			if err := m.enforceWrite(e, false); err != nil {
				return err
			}
			return e.Next()
		},
	})
}

func (m *Module) enforceRead(e *core.RecordEnrichEvent) {
	if e.Record == nil || e.RequestInfo == nil {
		return
	}
	col := e.Record.Collection()
	rules := m.rulesFor(col)
	if len(rules) == 0 {
		return
	}
	if e.RequestInfo.HasSuperuserAuth() && !EnforceSuperuser() {
		return
	}
	for field, r := range rules {
		if r.Read == nil || *r.Read == "" || col.Fields.GetByName(field) == nil {
			continue
		}
		ok, err := evalExisting(e.App, e.Record, e.RequestInfo, *r.Read)
		if err != nil {
			// fail closed: a broken rule must not leak the field
			e.App.Logger().Warn("fieldperm: read rule failed, hiding field",
				"collection", col.Name, "field", field, "error", err)
			ok = false
		}
		if !ok {
			e.Record.Hide(field)
		}
	}
}

// bodyField strips the modifier markers (field+, +field, field-) from a body key.
func bodyField(k string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, "+"), "+"), "-")
}

func (m *Module) enforceWrite(e *core.RecordRequestEvent, create bool) error {
	rules := m.rulesFor(e.Collection)
	if len(rules) == 0 {
		return nil
	}
	info, err := e.RequestInfo()
	if err != nil || info == nil {
		return nil // the handler reports its own error
	}
	if info.HasSuperuserAuth() && !EnforceSuperuser() {
		return nil
	}

	denied := validation.Errors{}
	deniedFields := []string{}
	for k := range info.Body {
		name := bodyField(k)
		r := rules[name]
		if r == nil || r.Write == nil {
			continue
		}
		if _, dup := denied[name]; dup {
			continue
		}
		var ok bool
		if *r.Write == "" {
			ok = false // locked: only superusers (handled above unless enforcing)
		} else {
			var evalErr error
			if create {
				ok, evalErr = evalSubmitted(e.App, e.Record, info, *r.Write)
			} else {
				ok, evalErr = evalExisting(e.App, e.Record, info, *r.Write)
			}
			if evalErr != nil {
				e.App.Logger().Warn("fieldperm: write rule failed, rejecting",
					"collection", e.Collection.Name, "field", name, "error", evalErr)
				ok = false
			}
		}
		if !ok {
			denied[name] = validation.NewError(ErrCode, ErrMessage)
			deniedFields = append(deniedFields, name)
		}
	}
	if len(denied) == 0 {
		return nil
	}
	for _, f := range deniedFields {
		m.audit(e, info, f, create)
	}
	msg := "Failed to update record."
	if create {
		msg = "Failed to create record."
	}
	return e.BadRequestError(msg, denied)
}

func (m *Module) audit(e *core.RecordRequestEvent, info *core.RequestInfo, field string, create bool) {
	user, userColl := "", ""
	if info.Auth != nil {
		user, userColl = info.Auth.Id, info.Auth.Collection().Name
	}
	key := e.Collection.Name + "\x00" + field + "\x00" + userColl + ":" + user
	now := m.now()
	m.auditMu.Lock()
	if last, ok := m.auditLast[key]; ok && now.Sub(last) < auditEvery {
		m.auditMu.Unlock()
		return
	}
	m.auditLast[key] = now
	m.auditMu.Unlock()

	op := "update"
	if create {
		op = "create"
	}
	e.App.Logger().Warn("fieldperm: write denied",
		"collection", e.Collection.Name, "field", field, "op", op, "user", user)

	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(ActionDenied, e.Collection.Name, e.Record.Id, map[string]any{
			"field": field, "op": op, "user": user, "user_collection": userColl,
			"ip": e.RealIP(),
		})
	}
}
