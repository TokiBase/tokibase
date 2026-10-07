// Package toki is the Go guest SDK for TokiBase WASM hooks (ABI "toki/1").
//
// Build a guest with the standard toolchain:
//
//	GOOS=wasip1 GOARCH=wasm go build -o pb_hooks_wasm/hello.wasm .
//
// The package imports only the standard library, so it also works with TinyGo.
// See docs/modules/wasm.md for the wire format other languages must implement.
package toki

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// ABI is the version string carried in every event.
const ABI = "toki/1"

// Log levels.
const (
	LevelDebug = 0
	LevelInfo  = 1
	LevelWarn  = 2
	LevelError = 3
)

// Actor is who triggered the call: "superuser", "auth", "guest" or "system".
type Actor struct {
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	Collection string `json:"collection,omitempty"`
}

// RequestInfo mirrors the request context of the originating HTTP request.
type RequestInfo struct {
	Method  string            `json:"method"`
	Context string            `json:"context"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    map[string]any    `json:"body"`
}

// Route is delivered for `route:` events.
type Route struct {
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	PathParams map[string]string `json:"path_params"`
	Query      map[string]string `json:"query"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

// BatchRequest is one sub-request of an /api/batch call (batch events). Body is
// the submitted body in batch.before and the stored values in batch.after (nil
// for deleted records); credential keys and sensitive fields are redacted.
type BatchRequest struct {
	Index      int            `json:"index"`
	Method     string         `json:"method"` // POST | PATCH | DELETE
	Path       string         `json:"path,omitempty"`
	Collection string         `json:"collection,omitempty"`
	ID         string         `json:"id,omitempty"`
	Body       map[string]any `json:"body,omitempty"`
	Deleted    bool           `json:"deleted,omitempty"`
}

// BatchAuth summarizes who sent the batch (never the token).
type BatchAuth struct {
	ID         string `json:"id"`
	Collection string `json:"collection"`
	Superuser  bool   `json:"superuser"`
}

// Batch is delivered for `batch.before`, `batch.after` and `batch.*` events.
// Return Reject(...) from the handler to refuse the whole batch; the batch
// transaction rolls back.
type Batch struct {
	Requests []BatchRequest `json:"requests"`
	Auth     *BatchAuth     `json:"auth,omitempty"` // nil = anonymous
}

// For returns the requests addressed to collection (DELETEs included).
func (b *Batch) For(collection string) []BatchRequest {
	var out []BatchRequest
	if b == nil {
		return nil
	}
	for _, r := range b.Requests {
		if r.Collection == collection {
			out = append(out, r)
		}
	}
	return out
}

// Sum adds the numeric (or numeric string) field over the non-DELETE requests
// of collection; missing or non numeric values count as 0.
func (b *Batch) Sum(collection, field string) float64 {
	var total float64
	for _, r := range b.For(collection) {
		if r.Deleted || r.Method == "DELETE" {
			continue
		}
		switch v := r.Body[field].(type) {
		case float64:
			total += v
		case string:
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
				total += f
			}
		}
	}
	return total
}

// Event is the JSON document the host writes to stdin.
type Event struct {
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
	RequestInfo *RequestInfo   `json:"request_info,omitempty"`
	Route       *Route         `json:"route,omitempty"`
	Batch       *Batch         `json:"batch,omitempty"`
	Cron        *struct {
		Expr string `json:"expr"`
	} `json:"cron,omitempty"`
	Job *struct {
		Name    string          `json:"name"`
		Payload json.RawMessage `json:"payload"`
	} `json:"job,omitempty"`
	Time string `json:"time"`
}

// Result is the JSON document the guest writes to stdout.
type Result struct {
	OK      bool              `json:"ok"`
	Record  map[string]any    `json:"record,omitempty"`  // record hooks: fields to change
	Status  int               `json:"status,omitempty"`  // routes: HTTP status; rejections: 4xx
	Message string            `json:"message,omitempty"` // rejections
	Data    map[string]any    `json:"data,omitempty"`    // rejections: field errors
	Headers map[string]string `json:"headers,omitempty"` // routes
	Body    any               `json:"body,omitempty"`    // routes: string = raw, anything else = JSON
}

// Ok returns an empty successful result.
func Ok() *Result { return &Result{OK: true} }

// Set records a field change (record hooks).
func (r *Result) Set(field string, v any) *Result {
	if r.Record == nil {
		r.Record = map[string]any{}
	}
	r.Record[field] = v
	return r
}

// Reject builds a rejection (before-hooks) or an error response (routes).
func Reject(status int, message string, data map[string]any) *Result {
	return &Result{OK: false, Status: status, Message: message, Data: data}
}

// JSON builds a route response with a JSON body.
func JSON(status int, body any) *Result {
	return &Result{OK: true, Status: status, Body: body, Headers: map[string]string{"Content-Type": "application/json"}}
}

// Handler processes one event. Returning an error (or panicking) makes the
// host treat the call as a failed hook.
type Handler func(*Event) (*Result, error)

// Run reads the event from stdin, calls h and writes the result to stdout.
// Call it from main; it exits non-zero when h fails.
func Run(h Handler) {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal(err)
	}
	var ev Event
	if err := json.Unmarshal(in, &ev); err != nil {
		fatal(err)
	}
	res, err := h(&ev)
	if err != nil {
		fatal(err)
	}
	if res == nil {
		res = Ok()
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "toki guest error:", err)
	os.Exit(1)
}

// ---- host calls ----

type envelope struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Raw   json.RawMessage `json:"-"`
}

func call(fn func(ptr, n uint32) uint64, req any, out any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := callRaw(fn, b)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(resp, &env); err != nil {
		return err
	}
	if !env.OK {
		return errors.New(env.Error)
	}
	if out != nil {
		return json.Unmarshal(resp, out)
	}
	return nil
}

// Log writes to the host log (needs no capability).
func Log(level int, msg string) { hostLog(level, msg) }

// Logf is Log with formatting.
func Logf(level int, format string, a ...any) { hostLog(level, fmt.Sprintf(format, a...)) }

// FindRequest selects records. Set ID for a single record, or Filter (with
// {:name} placeholders bound from Params) for a list.
type FindRequest struct {
	Collection string         `json:"collection"`
	ID         string         `json:"id,omitempty"`
	Filter     string         `json:"filter,omitempty"`
	Params     map[string]any `json:"params,omitempty"`
	Sort       string         `json:"sort,omitempty"`
	Limit      int            `json:"limit,omitempty"`
	Offset     int            `json:"offset,omitempty"`
}

// RecordsFind needs the "records" capability.
func RecordsFind(req FindRequest) ([]map[string]any, error) {
	var out struct {
		Records []map[string]any `json:"records"`
	}
	if err := call(hostRecordsFind, req, &out); err != nil {
		return nil, err
	}
	return out.Records, nil
}

// RecordsSave creates (empty id) or updates a record; "records" capability.
// Writes made by a guest do not re-trigger WASM hooks.
func RecordsSave(collection, id string, data map[string]any) (map[string]any, error) {
	var out struct {
		Record map[string]any `json:"record"`
	}
	err := call(hostRecordsSave, map[string]any{"collection": collection, "id": id, "data": data}, &out)
	return out.Record, err
}

// RecordsDelete deletes a record; "records" capability.
func RecordsDelete(collection, id string) error {
	return call(hostRecordsDelete, map[string]any{"collection": collection, "id": id}, nil)
}

// HTTPRequest is an outbound request; "http" capability + TOKI_WASM_HTTP_ALLOW.
type HTTPRequest struct {
	Method    string            `json:"method,omitempty"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

// HTTPResponse is the reply to HTTPFetch.
type HTTPResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

// HTTPFetch performs an HTTP request through the host.
func HTTPFetch(req HTTPRequest) (*HTTPResponse, error) {
	var out HTTPResponse
	if err := call(hostHTTPFetch, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Mail is an outgoing message; "mail" capability.
type Mail struct {
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	HTML    string   `json:"html,omitempty"`
	Text    string   `json:"text,omitempty"`
}

// MailSend sends through the configured SMTP settings.
func MailSend(m Mail) error { return call(hostMailSend, m, nil) }

// KVGet reads a per-module key; "kv" capability. found is false when absent.
func KVGet(key string) (value string, found bool, err error) {
	var out struct {
		Found bool   `json:"found"`
		Value string `json:"value"`
	}
	err = call(hostKVGet, map[string]any{"key": key}, &out)
	return out.Value, out.Found, err
}

// KVSet writes a per-module key; ttlSeconds 0 means no expiry.
func KVSet(key, value string, ttlSeconds int) error {
	return call(hostKVSet, map[string]any{"key": key, "value": value, "ttl_s": ttlSeconds}, nil)
}

// JobsEnqueue queues a durable job delivered back to this module as event
// `job:<name>` (declare it in the sidecar); "jobs" capability.
func JobsEnqueue(name string, payload any, delaySeconds int, unique string) (id string, err error) {
	var out struct {
		ID string `json:"id"`
	}
	err = call(hostJobsEnqueue, map[string]any{"name": name, "payload": payload, "delay_s": delaySeconds, "unique": unique}, &out)
	return out.ID, err
}
