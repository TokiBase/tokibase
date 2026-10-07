package webhooks

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

// Event names.
const (
	EventRecordCreate     = "record.create"
	EventRecordUpdate     = "record.update"
	EventRecordDelete     = "record.delete"
	EventCollectionCreate = "collection.create"
	EventCollectionUpdate = "collection.update"
	EventCollectionDelete = "collection.delete"
	EventAuthLogin        = "auth.login"
	EventPing             = "ping"
)

// Payload is the JSON body sent to the receiver.
type Payload struct {
	ID         string         `json:"id"`
	Event      string         `json:"event"`
	Created    string         `json:"created"`
	Collection string         `json:"collection"`
	RecordID   string         `json:"record_id"`
	Data       any            `json:"data"`
	Old        map[string]any `json:"old,omitempty"`
	Changed    []string       `json:"changed,omitempty"`
}

func newPayload(event, collection, recordID string, data any) *Payload {
	return &Payload{
		ID:    security.RandomStringWithAlphabet(15, "abcdefghijklmnopqrstuvwxyz0123456789"),
		Event: event, Created: fmtTime(nowFn()), Collection: collection, RecordID: recordID, Data: data,
	}
}

// skipCollection reports whether events of the collection are never captured
// (system collections, which include the _webhooks config itself).
func skipCollection(name string) bool { return strings.HasPrefix(name, "_") }

func (m *Module) bindCapture() {
	app := m.app
	rec := func(event string) func(e *core.RecordEvent) error {
		return func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			name := e.Record.Collection().Name
			if skipCollection(name) {
				return nil
			}
			p := newPayload(event, name, e.Record.Id, e.Record.PublicExport())
			if event == EventRecordUpdate {
				p.Old, p.Changed = changedFields(e.Record)
			}
			m.enqueue(event, name, e.Record.Id, p)
			return nil
		}
	}
	bindRec := func(tagged func(...string) *hook.TaggedHook[*core.RecordEvent], event string) {
		tagged().Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Priority: hookPriority, Func: rec(event)})
	}
	bindRec(app.OnRecordAfterCreateSuccess, EventRecordCreate)
	bindRec(app.OnRecordAfterUpdateSuccess, EventRecordUpdate)
	bindRec(app.OnRecordAfterDeleteSuccess, EventRecordDelete)

	col := func(event string) func(e *core.CollectionEvent) error {
		return func(e *core.CollectionEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if skipCollection(e.Collection.Name) {
				return nil
			}
			m.enqueue(event, e.Collection.Name, e.Collection.Id, newPayload(event, e.Collection.Name, e.Collection.Id, collectionData(e.Collection)))
			return nil
		}
	}
	bindCol := func(tagged func(...string) *hook.TaggedHook[*core.CollectionEvent], event string) {
		tagged().Bind(&hook.Handler[*core.CollectionEvent]{Id: hookId, Priority: hookPriority, Func: col(event)})
	}
	bindCol(app.OnCollectionAfterCreateSuccess, EventCollectionCreate)
	bindCol(app.OnCollectionAfterUpdateSuccess, EventCollectionUpdate)
	bindCol(app.OnCollectionAfterDeleteSuccess, EventCollectionDelete)

	app.OnRecordAuthRequest().Bind(&hook.Handler[*core.RecordAuthRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordAuthRequestEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			// empty AuthMethod = token refresh / impersonation, not a login
			if e.AuthMethod == "" {
				return nil
			}
			name := e.Collection.Name
			data := map[string]any{"id": e.Record.Id, "method": e.AuthMethod, "ip": e.RealIP()}
			m.enqueue(EventAuthLogin, name, e.Record.Id, newPayload(EventAuthLogin, name, e.Record.Id, data))
			return nil
		},
	})
}

// changedFields compares the record with its pristine copy and returns the
// old values of the changed, non-hidden fields plus their names.
func changedFields(r *core.Record) (map[string]any, []string) {
	orig := r.Original()
	if orig == nil || orig.Id == "" { // no pristine copy (record created in the same object)
		return nil, nil
	}
	old := map[string]any{}
	var changed []string
	for _, f := range r.Collection().Fields {
		n := f.GetName()
		if f.GetHidden() || n == core.FieldNamePassword || n == core.FieldNameTokenKey {
			continue
		}
		a, b := r.Get(n), orig.Get(n)
		if jsonEqual(a, b) {
			continue
		}
		changed = append(changed, n)
		old[n] = b
	}
	if len(changed) == 0 {
		return nil, nil
	}
	return old, changed
}

func jsonEqual(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	ja, e1 := json.Marshal(a)
	jb, e2 := json.Marshal(b)
	return e1 == nil && e2 == nil && string(ja) == string(jb)
}

// collectionData is the collection JSON with secret-looking keys removed.
func collectionData(c *core.Collection) any {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return stripSecrets(v)
}

func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "tokenkey", "token", "privatekey", "apikey", "otp":
		return true
	}
	return strings.Contains(k, "password") || strings.Contains(k, "secret")
}

func stripSecrets(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if isSecretKey(k) {
				delete(t, k)
				continue
			}
			t[k] = stripSecrets(val)
		}
	case []any:
		for i := range t {
			t[i] = stripSecrets(t[i])
		}
	}
	return v
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
