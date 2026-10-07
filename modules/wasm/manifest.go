//go:build !no_wasm

package wasm

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tokibase/tokibase/tools/cron"
)

// Defaults and hard caps for sidecar limits.
const (
	DefaultTimeoutMS   = 2000
	MaxTimeoutMS       = 120000
	DefaultMemoryPages = 256 // 16 MiB; a Go (wasip1) guest needs roughly 40-60 pages to start
	MaxMemoryPages     = 16384
	MaxStdoutBytes     = 1 << 20
	MaxHostReqBytes    = 4 << 20
	// MaxRouteBodyBytes and MaxRouteHeaderBytes cap what a route guest receives.
	MaxRouteBodyBytes   = 1 << 20
	MaxRouteHeaderBytes = 32 << 10
)

// Capabilities a sidecar may grant through `needs`.
var knownNeeds = map[string]bool{"http": true, "records": true, "mail": true, "kv": true, "jobs": true}

// Manifest is the parsed sidecar `<name>.toml` (all fields optional).
type Manifest struct {
	Name        string   `json:"name"`
	File        string   `json:"file"`
	Events      []string `json:"events"`
	TimeoutMS   int      `json:"timeout_ms"`
	MemoryPages int      `json:"memory_pages"`
	Needs       []string `json:"needs"`
	// HTTPAllow narrows TOKI_WASM_HTTP_ALLOW for this module (host patterns);
	// empty = only the global list applies. It never widens it.
	HTTPAllow []string          `json:"http_allow,omitempty"`
	Env       map[string]string `json:"env"`
}

// Has reports whether the capability was granted.
func (m *Manifest) Has(need string) bool {
	for _, n := range m.Needs {
		if n == need {
			return true
		}
	}
	return false
}

// EventKind classifies a declared event.
type EventKind int

const (
	KindRecord EventKind = iota + 1
	KindCron
	KindRoute
	KindJob
	KindBatch
)

// ParsedEvent is a validated entry of `events`.
type ParsedEvent struct {
	Raw        string
	Kind       EventKind
	After      bool   // record.after.*
	Action     string // create|update|delete|*
	Collection string // name or *
	Cron       string
	Method     string
	Path       string
	Job        string
	Phase      string // batch: before | after | *
}

var recordActions = map[string]bool{"create": true, "update": true, "delete": true, "*": true}

// ParseEvent validates one declared event string.
func ParseEvent(s string) (ParsedEvent, error) {
	s = strings.TrimSpace(s)
	ev := ParsedEvent{Raw: s}
	switch {
	case strings.HasPrefix(s, "cron:"):
		expr := strings.TrimSpace(strings.TrimPrefix(s, "cron:"))
		if _, err := cron.NewSchedule(expr); err != nil {
			return ev, fmt.Errorf("event %q: invalid cron expression: %v", s, err)
		}
		ev.Kind, ev.Cron = KindCron, expr
	case strings.HasPrefix(s, "route:"):
		rest := strings.TrimSpace(strings.TrimPrefix(s, "route:"))
		method, path, ok := strings.Cut(rest, " ")
		path = strings.TrimSpace(path)
		if !ok || method == "" || !strings.HasPrefix(path, "/") || method != strings.ToUpper(method) {
			return ev, fmt.Errorf("event %q: want \"route:METHOD /path\"", s)
		}
		ev.Kind, ev.Method, ev.Path = KindRoute, method, path
	case strings.HasPrefix(s, "job:"):
		name := strings.TrimSpace(strings.TrimPrefix(s, "job:"))
		if name == "" || strings.ContainsAny(name, " \t") {
			return ev, fmt.Errorf("event %q: want \"job:<name>\"", s)
		}
		ev.Kind, ev.Job = KindJob, name
	case strings.HasPrefix(s, "batch."):
		phase := strings.TrimPrefix(s, "batch.")
		if phase != "before" && phase != "after" && phase != "*" {
			return ev, fmt.Errorf("event %q: want batch.before, batch.after or batch.*", s)
		}
		ev.Kind, ev.Phase = KindBatch, phase
	case strings.HasPrefix(s, "record."):
		parts := strings.Split(s, ".")
		if len(parts) > 1 && parts[1] == "after" {
			ev.After = true
			parts = append(parts[:1], parts[2:]...)
		}
		if len(parts) != 3 || !recordActions[parts[1]] || parts[2] == "" {
			return ev, fmt.Errorf("event %q: want record[.after].<create|update|delete|*>.<collection|*>", s)
		}
		ev.Kind, ev.Action, ev.Collection = KindRecord, parts[1], parts[2]
	default:
		return ev, fmt.Errorf("event %q: unknown event type", s)
	}
	return ev, nil
}

// Matches reports whether a record event (action, collection, after) is
// selected by e. A wildcard collection never matches names starting with "_".
func (e ParsedEvent) matchRecord(after bool, action, collection string) bool {
	if e.Kind != KindRecord || e.After != after {
		return false
	}
	if e.Action != "*" && e.Action != action {
		return false
	}
	if e.Collection == "*" {
		return !strings.HasPrefix(collection, "_")
	}
	return e.Collection == collection
}

func (e ParsedEvent) matchBatch(phase string) bool {
	return e.Kind == KindBatch && (e.Phase == "*" || e.Phase == phase)
}

// ParseManifest builds a Manifest from sidecar TOML ("" = defaults).
func ParseManifest(name, file, src string) (*Manifest, error) {
	m := &Manifest{Name: name, File: file, TimeoutMS: DefaultTimeoutMS, MemoryPages: DefaultMemoryPages, Env: map[string]string{}}
	if strings.TrimSpace(src) == "" {
		return m, nil
	}
	doc, err := parseTOML(src)
	if err != nil {
		return nil, err
	}
	strs := func(key string) ([]string, error) {
		v, ok := doc[key]
		if !ok {
			return nil, nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of strings", key)
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	}
	for k := range doc {
		switch k {
		case "events", "timeout_ms", "memory_pages", "needs", "env", "http_allow":
		default:
			return nil, fmt.Errorf("unknown key %q", k)
		}
	}
	if m.Events, err = strs("events"); err != nil {
		return nil, err
	}
	if m.Needs, err = strs("needs"); err != nil {
		return nil, err
	}
	if m.HTTPAllow, err = strs("http_allow"); err != nil {
		return nil, err
	}
	for _, p := range m.HTTPAllow {
		if strings.TrimSpace(p) == "" || strings.ContainsAny(p, " /:") || (strings.Contains(p, "*") && !strings.HasPrefix(p, "*.")) {
			return nil, fmt.Errorf("http_allow entry %q: want a host (api.example.com) or *.example.com", p)
		}
	}
	for _, e := range m.Events {
		if _, err := ParseEvent(e); err != nil {
			return nil, err
		}
	}
	for _, n := range m.Needs {
		if !knownNeeds[n] {
			return nil, fmt.Errorf("unknown capability %q in needs (known: http, records, mail, kv, jobs)", n)
		}
	}
	sort.Strings(m.Needs)
	if v, ok := doc["timeout_ms"]; ok {
		n, ok := v.(int64)
		if !ok || n < 1 || n > MaxTimeoutMS {
			return nil, fmt.Errorf("timeout_ms must be an integer between 1 and %d", MaxTimeoutMS)
		}
		m.TimeoutMS = int(n)
	}
	if v, ok := doc["memory_pages"]; ok {
		n, ok := v.(int64)
		if !ok || n < 1 || n > MaxMemoryPages {
			return nil, fmt.Errorf("memory_pages must be an integer between 1 and %d (64 KiB each)", MaxMemoryPages)
		}
		m.MemoryPages = int(n)
	}
	if v, ok := doc["env"]; ok {
		t, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("env must be a table of strings")
		}
		for k, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("env.%s must be a string", k)
			}
			m.Env[k] = s
		}
	}
	return m, nil
}

// ParsedEvents returns the validated events (Manifest.Events already passed validation).
func (m *Manifest) ParsedEvents() []ParsedEvent {
	out := make([]ParsedEvent, 0, len(m.Events))
	for _, s := range m.Events {
		if e, err := ParseEvent(s); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// discover lists `*.wasm` files of dir with their manifests; broken sidecars
// are reported in errs (keyed by module name) and the module is skipped.
func discover(dir string) (mans []*Manifest, errs map[string]error) {
	errs = map[string]error{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errs
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".wasm") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".wasm")
		var src string
		if b, err := os.ReadFile(filepath.Join(dir, name+".toml")); err == nil {
			src = string(b)
		}
		m, err := ParseManifest(name, filepath.Join(dir, e.Name()), src)
		if err != nil {
			errs[name] = fmt.Errorf("%s.toml: %w", name, err)
			continue
		}
		mans = append(mans, m)
	}
	sort.Slice(mans, func(i, j int) bool { return mans[i].Name < mans[j].Name })
	return mans, errs
}
