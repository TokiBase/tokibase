//go:build !no_mcp

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

const (
	// maxPayload caps the size of a tool input and of a tool result.
	maxPayload = 1 << 20
	// EnvUnaudited allows write tools while no audit sink is configured.
	EnvUnaudited = "TOKI_MCP_UNAUDITED"
	// deniedAuditEvery is the minimum gap between two audit rows of denied
	// calls of the same agent and tool.
	deniedAuditEvery = time.Minute
)

// queryTimeout bounds records.query (a var so tests can shrink it).
var queryTimeout = 30 * time.Second

var errInternal = errors.New("internal error")

// internal logs the details of an unexpected failure and returns the generic
// error that is safe to hand to the agent (no SQL, table or column names).
func (s *Server) internal(c *call, op string, err error) error {
	tool := ""
	if c != nil {
		tool = c.tool
	}
	s.app.Logger().Error("mcp: "+op, "tool", tool, "error", err)
	return errInternal
}

// userErr keeps field validation messages (safe and useful to the agent) and
// turns everything else into the generic error.
func (s *Server) userErr(c *call, op string, err error) error {
	var ve validation.Errors
	if errors.As(err, &ve) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return s.internal(c, op, err)
}

func auditEnabled() bool {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return auditSink != nil
}

func unauditedAllowed() bool { return os.Getenv(EnvUnaudited) == "1" }

func tooBig(v any) bool {
	raw, err := json.Marshal(v)
	return err == nil && len(raw) > maxPayload
}

// ---- collection access ----------------------------------------------------

// reserved reports collections that are internal: System ones and any name
// starting with an underscore (module config collections).
func reserved(c *kernel.Collection) bool {
	return c.System || strings.HasPrefix(c.Name, "_")
}

// canTouch is the single access predicate: `_agents` never; the allowlist for
// every role; reserved collections are readable by operators only and writable
// by nobody.
func canTouch(a *Agent, c *kernel.Collection, write bool) bool {
	if c.Name == CollectionName || !a.allows(c.Name) {
		return false
	}
	if reserved(c) {
		return !write && a.Role == RoleOperator
	}
	return true
}

// ---- relation / filter traversal (M1, M2) ---------------------------------

var (
	quotedRe = regexp.MustCompile(`'(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*"`)
	identRe  = regexp.MustCompile(`@?[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z]+)?(?:\.[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z]+)?)*`)
)

func stripModifier(seg string) string {
	if i := strings.Index(seg, ":"); i >= 0 {
		return seg[:i]
	}
	return seg
}

type traversal struct {
	s   *Server
	a   *Agent
	bad map[string]bool
}

func (t *traversal) col(name string) *kernel.Collection {
	c, err := t.s.app.FindCollectionByNameOrId(name)
	if err != nil {
		return nil
	}
	return c
}

func (t *traversal) check(c *kernel.Collection) {
	if !canTouch(t.a, c, false) {
		t.bad[c.Name] = true
	}
}

// walk follows relation segments from start. includeLast also resolves the
// target of the last segment (expand); a filter on the relation field itself
// only compares ids.
func (t *traversal) walk(start *kernel.Collection, segs []string, includeLast bool) {
	cur := start
	for i, raw := range segs {
		if cur == nil {
			return
		}
		seg := stripModifier(raw)
		if idx := strings.Index(seg, "_via_"); idx > 0 {
			target := t.col(seg[:idx])
			if target == nil {
				return
			}
			t.check(target)
			cur = target
			continue
		}
		rf, ok := cur.Fields.GetByName(seg).(*kernel.RelationField)
		if !ok || (i == len(segs)-1 && !includeLast) {
			return
		}
		target := t.col(rf.CollectionId)
		if target == nil {
			return
		}
		t.check(target)
		cur = target
	}
}

func (t *traversal) expr(col *kernel.Collection, expr string) {
	expr = quotedRe.ReplaceAllString(expr, " ")
	for _, id := range identRe.FindAllString(expr, -1) {
		segs := strings.Split(id, ".")
		switch {
		case strings.HasPrefix(id, "@collection."):
			if len(segs) < 2 {
				continue
			}
			target := t.col(stripModifier(segs[1]))
			if target == nil {
				continue
			}
			t.check(target)
			t.walk(target, segs[2:], false)
		case strings.HasPrefix(id, "@"):
		default:
			t.walk(col, segs, false)
		}
	}
}

// checkTraversal rejects a request whose expand, filter or sort reaches
// collections outside the agent allowlist, naming them.
func (s *Server) checkTraversal(a *Agent, col *kernel.Collection, expand, filter, sort_ string) error {
	t := &traversal{s: s, a: a, bad: map[string]bool{}}
	for _, p := range strings.Split(expand, ",") {
		if p = strings.TrimSpace(p); p != "" {
			t.walk(col, strings.Split(p, "."), true)
		}
	}
	t.expr(col, filter)
	for _, f := range strings.Split(sort_, ",") {
		f = strings.TrimLeft(strings.TrimSpace(f), "+-")
		if f != "" && !strings.HasPrefix(f, "@") {
			t.walk(col, strings.Split(f, "."), false)
		}
	}
	if len(t.bad) == 0 {
		return nil
	}
	names := make([]string, 0, len(t.bad))
	for n := range t.bad {
		names = append(names, n)
	}
	sort.Strings(names)
	return fmt.Errorf("request rejected: expand, filter or sort reaches collections outside the agent allowlist: %s", strings.Join(names, ", "))
}

// ---- cascades (M4) --------------------------------------------------------

// cascadeTargets lists the other collections a delete of a record of col can
// touch: relation fields pointing at it (set null or cascade), following
// cascadeDelete chains transitively.
func (s *Server) cascadeTargets(col *kernel.Collection) ([]*kernel.Collection, error) {
	all, err := s.app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	touched := map[string]*kernel.Collection{}
	seen := map[string]bool{col.Id: true}
	queue := []*kernel.Collection{col}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range all {
			if c.IsView() {
				continue
			}
			for _, f := range c.Fields {
				rf, ok := f.(*kernel.RelationField)
				if !ok || rf.CollectionId != cur.Id {
					continue
				}
				if c.Id != col.Id {
					touched[c.Id] = c
				}
				if rf.CascadeDelete && !seen[c.Id] {
					seen[c.Id] = true
					queue = append(queue, c)
				}
			}
		}
	}
	out := make([]*kernel.Collection, 0, len(touched))
	for _, c := range touched {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// cascadeCheck returns the plan warnings for a delete in col and fails (for
// non-operators) when a cascade target is outside what the agent may write.
// Warnings never name collections the agent may not touch.
func (s *Server) cascadeCheck(a *Agent, col *kernel.Collection) ([]string, error) {
	targets, err := s.cascadeTargets(col)
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	if len(targets) == 0 {
		return warnings, nil
	}
	var visible []string
	for _, c := range targets {
		if a.Role != RoleOperator && !canTouch(a, c, true) {
			return nil, errCascadeDenied
		}
		visible = append(visible, c.Name)
	}
	warnings = append(warnings, "deleting this record may also delete or modify records in other collections through relation fields (cascade delete, set null): "+strings.Join(visible, ", "))
	return warnings, nil
}

// ---- field permissions and enrichment (M8) --------------------------------

// syntheticEvent builds a request event for the agent (evaluated as guest) so
// the core request pipeline, including OnRecordEnrich and the record create
// and update request hooks of modules such as fieldperm, applies to MCP.
func (s *Server) syntheticEvent(method string, body map[string]any) (*core.RequestEvent, error) {
	raw := []byte("{}")
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://localhost/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	e := &core.RequestEvent{App: s.app}
	e.Request = req
	return e, nil
}

// writeGuard runs the record create/update request hook chain (with a no-op
// terminal handler) as a guest request carrying data as the body. Operators
// are superuser-like and skip it. rec must already hold the new values.
func (s *Server) writeGuard(a *Agent, col *kernel.Collection, rec *kernel.Record, data map[string]any, create bool) error {
	if a == nil || a.Role == RoleOperator {
		return nil
	}
	method := http.MethodPatch
	if create {
		method = http.MethodPost
	}
	e, err := s.syntheticEvent(method, data)
	if err != nil {
		return err
	}
	ev := new(core.RecordRequestEvent)
	ev.RequestEvent = e
	ev.Collection = col
	ev.Record = rec
	h := s.app.OnRecordUpdateRequest()
	if create {
		h = s.app.OnRecordCreateRequest()
	}
	err = h.Trigger(ev, func(*core.RecordRequestEvent) error { return nil })
	if err != nil {
		return fmt.Errorf("write denied: %v", err)
	}
	return nil
}

// enrich applies the OnRecordEnrich hooks (field level read permissions) as a
// guest to the records and, recursively, to their expanded relations.
func (s *Server) enrich(a *Agent, recs []*kernel.Record) {
	if a == nil || a.Role == RoleOperator || len(recs) == 0 {
		return
	}
	groups := map[string][]*kernel.Record{}
	order := []string{}
	var nested []*kernel.Record
	for _, r := range recs {
		id := r.Collection().Id
		if _, ok := groups[id]; !ok {
			order = append(order, id)
		}
		groups[id] = append(groups[id], r)
		for _, v := range r.Expand() {
			switch t := v.(type) {
			case *kernel.Record:
				nested = append(nested, t)
			case []*kernel.Record:
				nested = append(nested, t...)
			}
		}
	}
	for _, id := range order {
		e, err := s.syntheticEvent(http.MethodGet, nil)
		if err == nil {
			err = apis.EnrichRecords(e, groups[id])
		}
		if err != nil {
			// fail closed: never return unenriched data
			s.app.Logger().Error("mcp: record enrichment failed", "error", err)
			for _, r := range groups[id] {
				r.Hide(r.Collection().Fields.FieldNames()...)
			}
		}
	}
	s.enrich(a, nested)
}

// exportVisible exports a record the agent just wrote: operators see it all,
// others only when it passes the view rule as guest, otherwise just the id.
func (s *Server) exportVisible(a *Agent, col *kernel.Collection, rec *kernel.Record) map[string]any {
	if a.Role != RoleOperator {
		ok, err := s.canRead(a, rec, col.ViewRule)
		if err != nil || !ok {
			return map[string]any{"id": rec.Id, "collection": col.Name}
		}
		s.enrich(a, []*kernel.Record{rec})
	}
	return exportRecord(rec, "")
}
