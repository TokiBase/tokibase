package fieldperm

import (
	"bytes"
	"encoding/json"
	"net/http"
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
			// the cache entry is created before Next so the first record of a
			// batch owns it and every following record of the request reuses it
			var entry *reqEntry
			if e.RequestInfo != nil {
				var owner bool
				if entry, owner = m.requestEntry(e.RequestInfo); owner {
					defer m.releaseEntry(e.RequestInfo)
				}
			}
			// after Next: the upstream finalizer un-hides every field for superusers
			if err := e.Next(); err != nil {
				return err
			}
			m.enforceRead(e, entry)
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

func (m *Module) enforceRead(e *core.RecordEnrichEvent, entry *reqEntry) {
	if e.Record == nil {
		return
	}
	col := e.Record.Collection()
	rules, closed := m.rulesFor(col)

	if e.RequestInfo == nil {
		// unknown request context: fail closed for every protected field
		for field, r := range rules {
			if r.Read != nil && *r.Read != "" && col.Fields.GetByName(field) != nil {
				e.Record.Hide(field)
			}
		}
		hideRelationExpands(e.Record, rules)
		return
	}
	if e.RequestInfo.HasSuperuserAuth() && !EnforceSuperuser() {
		return
	}
	if closed {
		// rules never loaded: we cannot know which fields are protected
		for _, f := range col.Fields.FieldNames() {
			if f != "id" {
				e.Record.Hide(f)
			}
		}
		e.Record.SetExpand(nil)
		return
	}

	if len(rules) > 0 {
		for field, r := range rules {
			if r.Read == nil || *r.Read == "" || col.Fields.GetByName(field) == nil {
				continue
			}
			ok, err := m.evalRead(e, entry, col, field, *r.Read)
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
	// Expanded relations are attached by the finalizer (nested ones even after
	// the child's own enrich), so the whole tree is pruned from the root.
	m.pruneExpand(e.Record, 0)
}

// bodyField strips the modifier markers (field+, +field, field-) from a body key.
func bodyField(k string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(k, "+"), "+"), "-")
}

func (m *Module) enforceWrite(e *core.RecordRequestEvent, create bool) error {
	rules, closed := m.rulesFor(e.Collection)
	if len(rules) == 0 && !closed {
		return nil
	}
	info, err := e.RequestInfo()
	if err != nil || info == nil {
		return nil // the handler reports its own error
	}
	if info.HasSuperuserAuth() && !EnforceSuperuser() {
		return nil
	}

	if closed {
		// fail closed: rules never loaded, refuse writes instead of skipping the checks
		return e.Error(http.StatusServiceUnavailable, "Field rules are temporarily unavailable.", nil)
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
		if !create && unchanged(e.Record, name) {
			continue // not a change: re-sending the stored value is allowed
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

// unchanged reports whether the (already loaded) record value of a field
// equals the stored one. File fields are never considered unchanged.
func unchanged(rec *core.Record, name string) bool {
	orig := rec.Original()
	f := rec.Collection().Fields.GetByName(name)
	if orig == nil || f == nil {
		return false
	}
	if _, isFile := f.(*core.FileField); isFile {
		return false
	}
	a, err1 := json.Marshal(rec.Get(name))
	b, err2 := json.Marshal(orig.Get(name))
	return err1 == nil && err2 == nil && bytes.Equal(a, b)
}

// hiddenField reports whether name is not exported for rec (hidden by a rule or the schema).
func hiddenField(rec *core.Record, export *map[string]any, name string) bool {
	if *export == nil {
		exp := rec.Expand()
		rec.SetExpand(nil)
		*export = rec.PublicExport()
		rec.SetExpand(exp)
	}
	_, shown := (*export)[name]
	return !shown
}

// hideRelationExpands drops every expand entry whose relation is protected by a read rule (used when no request info is available).
func hideRelationExpands(rec *core.Record, rules map[string]*Rule) {
	exp := rec.Expand()
	for field, r := range rules {
		if r.Read != nil && *r.Read != "" {
			delete(exp, field)
		}
	}
	rec.SetExpand(exp)
}

// pruneExpand removes from the expand tree of rec every relation whose field
// is hidden on its owner record, and (for back-relations "coll_via_field")
// every related record whose "field" is hidden.
func (m *Module) pruneExpand(rec *core.Record, depth int) {
	exp := rec.Expand()
	if len(exp) == 0 || depth > 8 {
		return
	}
	var export map[string]any
	changed := false
	for key, v := range exp {
		if idx := strings.Index(key, "_via_"); idx > 0 {
			field := key[idx+len("_via_"):]
			var kept []*core.Record
			var all []*core.Record
			switch t := v.(type) {
			case *core.Record:
				all = []*core.Record{t}
			case []*core.Record:
				all = t
			}
			for _, r := range all {
				var ex map[string]any
				if !hiddenField(r, &ex, field) {
					kept = append(kept, r)
				}
			}
			if len(kept) != len(all) {
				changed = true
				if len(kept) == 0 {
					delete(exp, key)
				} else {
					exp[key] = kept
				}
			}
			continue
		}
		if rec.Collection().Fields.GetByName(key) == nil {
			continue
		}
		if hiddenField(rec, &export, key) {
			delete(exp, key)
			changed = true
		}
	}
	if changed {
		rec.SetExpand(exp)
	}
	for _, v := range exp {
		switch t := v.(type) {
		case *core.Record:
			m.pruneExpand(t, depth+1)
		case []*core.Record:
			for _, r := range t {
				m.pruneExpand(r, depth+1)
			}
		}
	}
}
