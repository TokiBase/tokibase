//go:build !no_mcp

package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/security"
)

const planTTL = 5 * time.Minute

// Server is an MCP server bound to one agent identity.
type Server struct {
	app     core.App
	agentID string
	session string
	sdk     *sdk.Server
	now     func() time.Time
	bucket  bucket

	mu    sync.Mutex
	plans map[string]*plan

	dmu    sync.Mutex
	denied map[string]*deniedState
}

// deniedState throttles the audit rows of denied calls per agent and tool.
type deniedState struct {
	last       time.Time
	suppressed int
}

type plan struct {
	agent   string
	tool    string
	hash    string
	expires time.Time
}

// call is the per-invocation context handed to tool handlers.
type call struct {
	ctx   context.Context
	agent *Agent
	tool  string

	// audit fields filled in by the handler
	collection string
	record     string
	details    map[string]any
}

func (c *call) set(k string, v any) {
	if c.details == nil {
		c.details = map[string]any{}
	}
	c.details[k] = v
}

// NewServer builds the MCP server (tools, resources, prompts) for an agent.
func NewServer(app core.App, agent *Agent, version string) *Server {
	s := &Server{
		app: app, agentID: agent.ID, now: time.Now,
		session: security.RandomString(16),
		plans:   map[string]*plan{},
		denied:  map[string]*deniedState{},
	}
	if version == "" {
		version = getProviders().Version
	}
	if version == "" {
		version = "dev"
	}
	s.sdk = sdk.NewServer(&sdk.Implementation{Name: "tokibase", Version: version}, &sdk.ServerOptions{
		Instructions: "TokiBase instance. Use schema.list and schema.describe first. Writes need a `reason`; deletes and large batches are two-step (plan, then confirm_token). Rules are enforced as the agent role allows; see toki://docs/rules.",
	})
	s.registerTools()
	s.registerResources()
	s.registerPrompts()
	return s
}

// Session returns the id generated for this server run.
func (s *Server) Session() string { return s.session }

// SDK exposes the underlying SDK server (tests, custom transports).
func (s *Server) SDK() *sdk.Server { return s.sdk }

// Run serves the transport until it closes or ctx is canceled.
func (s *Server) Run(ctx context.Context, t sdk.Transport) error { return s.sdk.Run(ctx, t) }

// authorize reloads the agent, takes a rate limit token (denied calls count
// too) and then enforces the role. c.agent is set whenever the agent resolved.
func (s *Server) authorize(c *call, min Role) (*Agent, error) {
	a, err := reload(s.app, s.agentID)
	if err != nil {
		return nil, err
	}
	c.agent = a
	if !s.bucket.allow(s.now(), a.RatePerMin) {
		return a, fmt.Errorf("rate limit exceeded: %d calls/min for agent %q, retry shortly", a.RatePerMin, a.Name)
	}
	if a.Role.rank() < min.rank() {
		return a, fmt.Errorf("permission denied: tool needs role %s, agent %q is %s", min, a.Name, a.Role)
	}
	return a, nil
}

var errUnaudited = errors.New("write tools are refused while the audit module is disabled (enable audit, or the operator sets " + EnvUnaudited + "=1 to accept unaudited agent writes)")

// addTool registers a typed tool. write tools are always audited, operator
// calls (read or write) as well. Panics in a handler become "internal error".
func addTool[In any](s *Server, name, desc string, min Role, write bool, h func(c *call, in In) (map[string]any, error)) {
	sdk.AddTool(s.sdk, &sdk.Tool{Name: name, Description: desc},
		func(ctx context.Context, _ *sdk.CallToolRequest, in In) (*sdk.CallToolResult, map[string]any, error) {
			c := &call{ctx: ctx, tool: name}
			run := func() (out map[string]any, err error, denied bool) {
				defer func() {
					if r := recover(); r != nil {
						s.app.Logger().Error("mcp: tool handler panic", "tool", name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
						out, err, denied = nil, errInternal, false
					}
				}()
				if _, err := s.authorize(c, min); err != nil {
					return nil, err, true
				}
				if write && !auditEnabled() && !unauditedAllowed() {
					return nil, errUnaudited, true
				}
				if tooBig(in) {
					return nil, fmt.Errorf("payload too large (limit %d bytes)", maxPayload), false
				}
				out, err = h(c, in)
				if err == nil && tooBig(out) {
					return nil, fmt.Errorf("result too large (limit %d bytes): narrow the request (perPage, fields)", maxPayload), false
				}
				return out, err, false
			}
			out, err, denied := run()
			if c.agent != nil && (write || c.agent.Role == RoleOperator) {
				if denied {
					s.auditDenied(c, err)
				} else {
					s.audit(c, err, false)
				}
			}
			if err != nil {
				return nil, nil, err
			}
			return nil, out, nil
		})
}

// auditDenied records a denied call at most once per minute per agent and
// tool; the row carries how many denials were folded into it.
func (s *Server) auditDenied(c *call, err error) {
	key := c.agent.ID + "|" + c.tool
	now := s.now()
	s.dmu.Lock()
	st := s.denied[key]
	if st == nil {
		st = &deniedState{}
		s.denied[key] = st
	}
	if !st.last.IsZero() && now.Sub(st.last) < deniedAuditEvery {
		st.suppressed++
		s.dmu.Unlock()
		return
	}
	n := st.suppressed
	st.suppressed, st.last = 0, now
	s.dmu.Unlock()
	c.set("denied_suppressed_since_last", n)
	s.audit(c, err, true)
}

func (s *Server) audit(c *call, err error, denied bool) {
	d := map[string]any{
		"agent": c.agent.Name, "agent_id": c.agent.ID, "role": string(c.agent.Role),
		"session": s.session,
	}
	for k, v := range c.details {
		d[k] = v
	}
	if err != nil {
		d["error"] = err.Error()
	}
	if denied {
		d["denied"] = true
	}
	emit("agent."+c.tool, c.collection, c.record, d)
}

// ---- confirmation plans ---------------------------------------------------

func canonHash(v any) string {
	raw, _ := json.Marshal(v)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// issuePlan returns a single use token bound to (agent, tool, args).
func (s *Server) issuePlan(a *Agent, tool string, args any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, p := range s.plans {
		if now.After(p.expires) {
			delete(s.plans, k)
		}
	}
	tok := security.RandomString(24)
	s.plans[tok] = &plan{agent: a.ID, tool: tool, hash: canonHash(args), expires: now.Add(planTTL)}
	if requireHumanConfirm() {
		if err := writePending(s.app.DataDir(), tok, &pendingConfirm{
			Agent: a.Name, AgentID: a.ID, Tool: tool, Summary: planSummary(args), Expires: now.Add(planTTL),
		}); err != nil {
			s.app.Logger().Error("mcp: failed to store the pending confirmation", "error", err)
		}
	}
	return tok
}

// consumePlan validates and burns a token; the args must equal the planned ones.
func (s *Server) consumePlan(a *Agent, tool, token string, args any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[token]
	if !ok {
		return errors.New("confirm_token is unknown or already used: request a new plan (omit confirm_token)")
	}
	if requireHumanConfirm() {
		pc, err := readPending(s.app.DataDir(), token)
		if err != nil || !pc.Approved {
			return approvalErr(token)
		}
		_ = os.Remove(confirmPath(s.app.DataDir(), token))
	}
	delete(s.plans, token)
	if s.now().After(p.expires) {
		return errors.New("confirm_token expired (valid 5 minutes): request a new plan")
	}
	if p.agent != a.ID || p.tool != tool || p.hash != canonHash(args) {
		return errors.New("confirm_token does not match these arguments: the plan must be confirmed unchanged")
	}
	return nil
}

// ---- access helpers -------------------------------------------------------

var (
	errNotFound = func(name string) error {
		return fmt.Errorf("collection %q not found or not accessible to this agent", name)
	}
	secretKeyRe = regexp.MustCompile(`(?i)(password|secret|token|key|authorization|cookie|headers)`)
)

// guestInfo is the request info used to evaluate rules for non-operator agents.
func guestInfo() *kernel.RequestInfo {
	return &kernel.RequestInfo{
		Context: kernel.RequestInfoContextDefault, Method: "GET",
		Query: map[string]string{}, Headers: map[string]string{}, Body: map[string]any{},
	}
}

// collection resolves a collection for the agent: allowlist, `_agents` never,
// other system collections only for operators (read) and never for writes.
func (s *Server) collection(a *Agent, name string, write bool) (*kernel.Collection, error) {
	c, err := s.app.FindCollectionByNameOrId(strings.TrimSpace(name))
	if err != nil || !canTouch(a, c, write) {
		return nil, errNotFound(name)
	}
	if write && c.IsView() {
		return nil, fmt.Errorf("collection %q is a view and can not be written", c.Name)
	}
	return c, nil
}

// visibleCollections lists the collections the agent may see.
func (s *Server) visibleCollections(a *Agent) ([]*kernel.Collection, error) {
	all, err := s.app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	out := all[:0:0]
	for _, c := range all {
		if !canTouch(a, c, false) {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// canRead evaluates a collection rule for the agent: operators bypass,
// everyone else is evaluated as a guest.
func (s *Server) canRead(a *Agent, rec *kernel.Record, rule *string) (bool, error) {
	if a.Role == RoleOperator {
		return true, nil
	}
	return s.app.CanAccessRecord(rec, guestInfo(), rule)
}

func ruleState(rule *string) string {
	switch {
	case rule == nil:
		return "locked"
	case *rule == "":
		return "public"
	}
	return "expression"
}

// ---- value helpers --------------------------------------------------------

// sanitize truncates long strings and redacts secret looking keys, recursively.
func sanitize(v any, max int) any {
	switch t := v.(type) {
	case string:
		if max > 0 && len([]rune(t)) > max {
			return string([]rune(t)[:max]) + "…"
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if secretKeyRe.MatchString(k) {
				out[k] = "[redacted]"
				continue
			}
			out[k] = sanitize(x, max)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = sanitize(x, max)
		}
		return out
	}
	return v
}

// jsonValue converts any value into plain JSON types.
func jsonValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func exportRecord(r *kernel.Record, fields string) map[string]any {
	m := exportRedacted(r)
	if fields = strings.TrimSpace(fields); fields != "" {
		keep := map[string]bool{}
		for _, f := range strings.Split(fields, ",") {
			keep[strings.TrimSpace(f)] = true
		}
		for k := range m {
			if !keep[k] {
				delete(m, k)
			}
		}
	}
	return m
}

// exportRedacted is the JSON form of the record's public export with the
// fields registered as sensitive (encrypted at rest) replaced by a marker, so
// an agent never receives ciphertext or plaintext of them.
func exportRedacted(r *kernel.Record) map[string]any {
	return jsonValue(kernel.RedactExport(r, r.PublicExport(), "")).(map[string]any)
}
