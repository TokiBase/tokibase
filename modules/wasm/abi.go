//go:build !no_wasm

package wasm

import (
	"encoding/json"
	"strings"

	"github.com/tokibase/tokibase/core"
)

// ABI is the guest ABI version carried in every event.
const ABI = "toki/1"

// Actor identifies who triggered a call.
type Actor struct {
	Kind       string `json:"kind"` // superuser | auth | guest | system
	ID         string `json:"id,omitempty"`
	Collection string `json:"collection,omitempty"`
}

// RequestInfoIn is the request context passed to record hooks.
type RequestInfoIn struct {
	Method  string            `json:"method"`
	Context string            `json:"context"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    map[string]any    `json:"body"`
}

// RouteIn is the request passed to route handlers.
type RouteIn struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	PathParams map[string]string `json:"path_params"`
	Query      map[string]string `json:"query"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// BatchRequestIn is one sub-request of an /api/batch call as a guest sees it.
// Body is the submitted body in batch.before and the stored values (read back
// inside the transaction, nil for deleted records) in batch.after. Credential
// keys and sensitive fields are redacted.
type BatchRequestIn struct {
	Index      int            `json:"index"`
	Method     string         `json:"method"` // POST | PATCH | DELETE
	Path       string         `json:"path,omitempty"`
	Collection string         `json:"collection,omitempty"`
	ID         string         `json:"id,omitempty"`
	Body       map[string]any `json:"body,omitempty"`
	Deleted    bool           `json:"deleted,omitempty"`
}

// BatchAuthIn summarizes who sent the batch (never the token).
type BatchAuthIn struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Superuser  bool   `json:"superuser"`
}

// BatchIn is delivered for batch events.
type BatchIn struct {
	Requests []BatchRequestIn `json:"requests"`
	Auth     *BatchAuthIn     `json:"auth,omitempty"` // nil = anonymous
}

// EventIn is the JSON document written to the guest's stdin.
type EventIn struct {
	ABI         string         `json:"abi"`
	Module      string         `json:"module"`
	Event       string         `json:"event"`
	Kind        string         `json:"kind"` // record | cron | route | job | batch
	Phase       string         `json:"phase,omitempty"`
	Action      string         `json:"action,omitempty"`
	Collection  string         `json:"collection,omitempty"`
	Record      map[string]any `json:"record,omitempty"`
	Original    map[string]any `json:"original,omitempty"`
	Actor       Actor          `json:"actor"`
	RequestInfo *RequestInfoIn `json:"request_info,omitempty"`
	Route       *RouteIn       `json:"route,omitempty"`
	Batch       *BatchIn       `json:"batch,omitempty"`
	Cron        *struct {
		Expr string `json:"expr"`
	} `json:"cron,omitempty"`
	Job *struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	} `json:"job,omitempty"`
	Time string `json:"time"`
}

// Result is the JSON document read from the guest's stdout.
type Result struct {
	OK      bool              `json:"ok"`
	Record  map[string]any    `json:"record,omitempty"`
	Status  int               `json:"status,omitempty"`
	Message string            `json:"message,omitempty"`
	Data    map[string]any    `json:"data,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Headers never handed to guests in request_info (credentials of the actor).
func sensitiveHeader(k string) bool {
	k = strings.ReplaceAll(strings.ToLower(k), "_", "-")
	switch k {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
	return strings.HasPrefix(k, "x-toki-")
}

func sensitiveKey(k string) bool {
	switch strings.ToLower(k) {
	case "password", "passwordconfirm", "oldpassword", "token", "secret":
		return true
	}
	return false
}

func scrub(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !sensitiveKey(k) {
				out[k] = scrub(e)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = scrub(e)
		}
		return out
	}
	return v
}

// sanitizeRequestInfo copies the request context for a guest without
// credentials: the authorization/cookie/x-toki-* headers and the
// password/passwordConfirm/oldPassword/token/secret keys of the body (at any
// depth) and of the query are dropped. The original is never modified.
func sanitizeRequestInfo(info *core.RequestInfo) *RequestInfoIn {
	out := &RequestInfoIn{Method: info.Method, Context: info.Context,
		Query: map[string]string{}, Headers: map[string]string{}}
	for k, v := range info.Query {
		if !sensitiveKey(k) {
			out.Query[k] = v
		}
	}
	for k, v := range info.Headers {
		if !sensitiveHeader(k) {
			out.Headers[k] = v
		}
	}
	if info.Body != nil {
		out.Body = scrub(info.Body).(map[string]any)
	}
	return out
}
