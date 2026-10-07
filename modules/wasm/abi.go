//go:build !no_wasm

package wasm

import "encoding/json"

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

// EventIn is the JSON document written to the guest's stdin.
type EventIn struct {
	ABI         string         `json:"abi"`
	Module      string         `json:"module"`
	Event       string         `json:"event"`
	Kind        string         `json:"kind"` // record | cron | route | job
	Phase       string         `json:"phase,omitempty"`
	Action      string         `json:"action,omitempty"`
	Collection  string         `json:"collection,omitempty"`
	Record      map[string]any `json:"record,omitempty"`
	Original    map[string]any `json:"original,omitempty"`
	Actor       Actor          `json:"actor"`
	RequestInfo *RequestInfoIn `json:"request_info,omitempty"`
	Route       *RouteIn       `json:"route,omitempty"`
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
