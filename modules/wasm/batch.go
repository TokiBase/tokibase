//go:build !no_wasm

package wasm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
)

// MaxBatchPayloadBytes caps the JSON of the sub-requests handed to a guest
// (same cap as a host request). A bigger batch is rejected (fail closed)
// instead of being validated on a truncated view.
const MaxBatchPayloadBytes = MaxHostReqBytes

// syncBatch binds the batch handler only while a loaded module declares a
// batch event, so a server without such modules adds no work to /api/batch
// (batchguard buffers the response only when kernel.OnBatchFor has handlers).
// Called from Reload with h.mu held.
func (h *Host) syncBatch(r *registry) {
	want := false
	for _, m := range r.mods {
		for _, e := range m.Parsed {
			if e.Kind == KindBatch {
				want = true
			}
		}
	}
	hooks := kernel.OnBatchFor(h.app)
	switch {
	case want && !h.batchBound:
		hooks.Bind(&hook.Handler[*kernel.BatchEvent]{Id: hookID, Func: h.onBatch})
		h.batchBound = true
	case !want && h.batchBound:
		hooks.Unbind(hookID)
		h.batchBound = false
	}
}

func (h *Host) matchBatch(phase string) []*Module {
	var out []*Module
	for _, m := range h.reg.Load().mods {
		for _, e := range m.Parsed {
			if e.matchBatch(phase) {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

func (h *Host) onBatch(e *kernel.BatchEvent) error {
	phase := "before"
	if e.Name == kernel.BatchAfter {
		phase = "after"
	}
	mods := h.matchBatch(phase)
	if len(mods) == 0 {
		return e.Next()
	}
	batch := buildBatchIn(e)
	if b, err := json.Marshal(batch); err != nil || len(b) > MaxBatchPayloadBytes {
		h.app.Logger().Error("wasm: batch payload too large or not encodable, batch refused", "size", len(b), "error", err)
		return router.NewApiError(http.StatusRequestEntityTooLarge, "Batch too large for validation hooks.", nil)
	}
	for _, m := range mods {
		ev := &EventIn{Event: e.Name, Kind: "batch", Phase: phase, Actor: actorFromAuth(e.Auth), Batch: batch}
		// The batch runs inside a transaction: e.App is the transaction app, so
		// host calls of the guest read and write inside it. A guest failure
		// (trap, timeout, bad output) fails CLOSED: the batch rolls back.
		res, err := h.Invoke(context.Background(), m, ev, CallOpts{App: e.App})
		if err != nil {
			return router.NewApiError(http.StatusInternalServerError, "Hook failed.", nil)
		}
		if !res.OK {
			status := res.Status
			if status < 400 || status > 599 {
				status = http.StatusBadRequest
			}
			msg := res.Message
			if msg == "" {
				msg = "Rejected by hook."
			}
			return router.NewApiError(status, msg, safeData(res.Data))
		}
	}
	return e.Next()
}

// buildBatchIn converts a kernel batch event into the guest payload: bodies
// are copied and redacted, the auth record is reduced to id/collection.
func buildBatchIn(e *kernel.BatchEvent) *BatchIn {
	out := &BatchIn{Requests: make([]BatchRequestIn, len(e.Requests))}
	for i, r := range e.Requests {
		q := BatchRequestIn{Index: r.Index, Method: r.Method, Collection: r.Collection, ID: r.ID, Deleted: r.Deleted}
		if r.Collection != "" {
			q.Path = "/api/collections/" + r.Collection + "/records"
			if r.ID != "" {
				q.Path += "/" + r.ID
			}
		}
		if r.Body != nil {
			q.Body = redactBatchBody(e.App, r.Collection, r.Body)
		}
		out.Requests[i] = q
	}
	if a := e.Auth; a != nil {
		out.Auth = &BatchAuthIn{ID: a.Id, Collection: a.Collection().Name, Superuser: a.IsSuperuser()}
	}
	return out
}

// redactBatchBody returns a copy of body without credential keys (scrub) and
// with the value of every field registered through
// kernel.RegisterSensitiveField replaced by kernel.SensitiveMarker. Modifier
// forms of a field name (`f+`, `+f`, `f-`, `f:suffix`) are covered too.
func redactBatchBody(app kernel.App, collection string, body map[string]any) map[string]any {
	out, _ := scrub(body).(map[string]any)
	if out == nil || collection == "" || app == nil {
		return out
	}
	col, err := core.AsApp(app).FindCachedCollectionByNameOrId(collection)
	if err != nil || col == nil {
		return out
	}
	sensitive := kernel.SensitiveFieldsOf(col.Id)
	if len(sensitive) == 0 {
		return out
	}
	for k, v := range out {
		base := strings.TrimPrefix(k, "+")
		base = strings.TrimRight(base, "+-")
		if i := strings.Index(base, ":"); i > 0 {
			base = base[:i]
		}
		for _, f := range sensitive {
			if base == f && !emptyValue(v) {
				out[k] = kernel.SensitiveMarker
			}
		}
	}
	return out
}

func emptyValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	}
	return false
}
