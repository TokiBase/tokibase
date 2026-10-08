//go:build !no_webhooks

package webhooks

import (
	"encoding/json"
	"reflect"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
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
	Seq        int64          `json:"seq,omitempty"` // monotonic per webhook (not a delivery order guarantee)
	Truncated  bool           `json:"truncated,omitempty"`
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

// encodePayload marshals p for one webhook with its sequence number. A payload
// above the size cap is replaced by a marker carrying only the record id.
func encodePayload(p *Payload, seq int64, recordID string) ([]byte, error) {
	cp := *p
	cp.Seq = seq
	body, err := json.Marshal(&cp)
	if err != nil {
		return nil, err
	}
	if len(body) > maxPayloadBytes() {
		cp.Data = map[string]any{"id": recordID}
		cp.Old = nil
		cp.Truncated = true
		return json.Marshal(&cp)
	}
	return body, nil
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
			// a sync replica apply (pull/snapshot/bundle) replays a change that
			// already fired its webhook once, on the hub
			if kernel.IsSyncReplica(e.Context) {
				return nil
			}
			name := e.Record.Collection().Name
			if skipCollection(name) {
				return nil
			}
			p := newPayload(event, name, e.Record.Id, safeExport(e.Record))
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
			if skipCollection(name) { // _superusers and other system auth collections
				return nil
			}
			data := map[string]any{"id": e.Record.Id, "method": e.AuthMethod, "ip": e.RealIP()}
			m.enqueue(EventAuthLogin, name, e.Record.Id, newPayload(EventAuthLogin, name, e.Record.Id, data))
			return nil
		},
	})
}

// safeExport is the public export without credential fields.
func safeExport(r *core.Record) map[string]any {
	m := kernel.RedactExport(r, r.PublicExport(), "") // encrypted fields never leave as plaintext or ciphertext
	delete(m, core.FieldNamePassword)
	delete(m, core.FieldNameTokenKey)
	return m
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
	// a field is reported only when it is visible in the public export of BOTH
	// the new and the previous state (this hides `email` unless emailVisibility)
	visNew, visOld := r.PublicExport(), orig.PublicExport()
	for _, f := range r.Collection().Fields {
		n := f.GetName()
		if f.GetHidden() || n == core.FieldNamePassword || n == core.FieldNameTokenKey {
			continue
		}
		if _, ok := visNew[n]; !ok {
			continue
		}
		if _, ok := visOld[n]; !ok {
			continue
		}
		a, b := r.Get(n), orig.Get(n)
		if kernel.IsSensitive(r.Collection().Id, n) {
			// the new and old values differ by nonce even when nothing changed
			// and the old value is ciphertext: report only the field name
			if jsonEqual(a, b) {
				continue
			}
			changed = append(changed, n)
			old[n] = kernel.SensitiveMarker
			continue
		}
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
