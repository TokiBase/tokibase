//go:build !no_mcp

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const rulesDoc = `# TokiBase / PocketBase rule language

Every collection has API rules per operation: ` + "`listRule`, `viewRule`, `createRule`, `updateRule`, `deleteRule`" + ` (auth collections also ` + "`authRule`, `manageRule`" + `).

## The three states

| Rule value | Meaning |
| --- | --- |
| ` + "`null`" + ` (locked) | only superusers pass |
| ` + "`\"\"`" + ` (empty string) | everyone passes, including guests (public) |
| expression | passes when the expression is true |

## Expressions

- Fields of the record: ` + "`status = \"public\"`, `owner = @request.auth.id`" + `.
- Operators: ` + "`=  !=  >  >=  <  <=  ~ (like/contains)  !~  ?=  ?!=  ?>  ?~`" + ` (the ` + "`?`" + ` prefix makes a multi-value field match when ANY element matches). Combine with ` + "`&&`, `||`" + ` and parentheses. Strings use single or double quotes; ` + "`true false null`" + ` and numbers are literals.
- Relations are traversed with dots: ` + "`author.verified = true`" + `, back relations ` + "`posts_via_author.id ?= \"x\"`" + `.
- Request data: ` + "`@request.auth.id`, `@request.auth.<field>`, `@request.body.<field>`, `@request.query.<k>`, `@request.headers.<k>`, `@request.method`, `@request.context`" + `. A guest has ` + "`@request.auth.id = \"\"`" + `. ` + "`@request.auth.kind`" + ` is guest, user, superuser or agent; an MCP agent has kind agent, its id and ` + "`@request.auth.role`" + ` (reader/writer/operator).
- Other collections: ` + "`@collection.memberships.user ?= @request.auth.id`" + `.
- Dates: ` + "`@now`, `@todayStart`, `@yearEnd`, ...`created > @now`" + `. Functions: ` + "`geoDistance(lonA, latA, lonB, latB)`" + `, and the ` + "`:lower` `:length` `:each` `:isset`" + ` modifiers (` + "`name:lower = \"x\"`, `@request.body.role:isset = false`" + `).

## Patterns

- Owner only: ` + "`@request.auth.id != \"\" && owner = @request.auth.id`" + `
- Signed in users: ` + "`@request.auth.id != \"\"`" + `
- Public if published: ` + "`published = true`" + `
- Block changing a field: ` + "`@request.body.role:isset = false`" + ` in the update rule.

## Common mistakes

- An empty string rule is PUBLIC and null is locked. Never use an empty rule for private data.
- A list rule acts as a filter: unmatched records are silently omitted (empty list, not 403). View/update/delete answer 404 or 403.
- Rules do not apply to superusers.
- Hidden fields can not be used in filters by non-superusers.
- Create rules can not reference the record being created by id; use ` + "`@request.body.*`" + `.

## In this MCP server (PR 1)

reader/writer agents are evaluated as GUEST. Role operator bypasses rules (superuser-like) and is audited. Use ` + "`rule.explain`" + ` to see a rule and (operator) test records.
`

func (s *Server) agentNow(ctx context.Context) (*Agent, error) {
	return reload(s.app, s.agentID)
}

func textResource(uri, mime, text string) *sdk.ReadResourceResult {
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: mime, Text: text}}}
}

func (s *Server) registerResources() {
	s.sdk.AddResource(&sdk.Resource{
		URI: "toki://schema", Name: "schema", MIMEType: "application/json",
		Description: "All collections visible to this agent: fields, rules, indexes (no sample data).",
	}, func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		a, err := s.agentNow(ctx)
		if err != nil {
			return nil, err
		}
		txt, err := s.schemaJSON(a)
		if err != nil {
			return nil, err
		}
		return textResource("toki://schema", "application/json", txt), nil
	})
	s.sdk.AddResource(&sdk.Resource{
		URI: "toki://docs/rules", Name: "rules-docs", MIMEType: "text/markdown",
		Description: "Concise guide to the PocketBase rule language.",
	}, func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return textResource("toki://docs/rules", "text/markdown", rulesDoc), nil
	})
	s.sdk.AddResource(&sdk.Resource{
		URI: "toki://instance", Name: "instance", MIMEType: "application/json",
		Description: "Version, enabled modules, profile and the calling agent.",
	}, func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		a, err := s.agentNow(ctx)
		if err != nil {
			return nil, err
		}
		p := getProviders()
		raw, _ := json.MarshalIndent(map[string]any{
			"version": p.Version, "profile": p.Profile, "modules": p.Modules,
			"agent":   map[string]any{"name": a.Name, "role": a.Role, "collections": a.Collections, "rate_per_min": a.RatePerMin},
			"session": s.session,
		}, "", "  ")
		return textResource("toki://instance", "application/json", string(raw)), nil
	})
}

func promptResult(desc, text string) (*sdk.GetPromptResult, error) {
	return &sdk.GetPromptResult{
		Description: desc,
		Messages:    []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: text}}},
	}, nil
}

func (s *Server) registerPrompts() {
	s.sdk.AddPrompt(&sdk.Prompt{
		Name: "write-rule", Description: "Write or tighten an API rule for a collection operation.",
		Arguments: []*sdk.PromptArgument{
			{Name: "collection", Description: "collection name", Required: true},
			{Name: "operation", Description: "list, view, create, update, delete", Required: true},
			{Name: "intent", Description: "who should be allowed to do what", Required: true},
		},
	}, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		a := req.Params.Arguments
		return promptResult("write-rule", fmt.Sprintf(`Write the %s rule for collection %q.
Intent: %s

Steps:
1. Read toki://docs/rules.
2. Call schema.describe for %q to see fields, relations and the current rules.
3. Propose the rule as a single expression. Remember: null = locked, "" = public. Prefer the narrowest rule that satisfies the intent.
4. Call rule.explain (collection, operation) to confirm the current state, and rule.lint to check for accidental public rules.
5. Explain the rule line by line and list the cases it denies. Do not apply it: collection rules are changed by a human via migration or the Admin UI.`,
			a["operation"], a["collection"], a["intent"], a["collection"]))
	})

	s.sdk.AddPrompt(&sdk.Prompt{
		Name: "debug-403", Description: "Find why a request to a collection is denied (401/403/404/empty list).",
		Arguments: []*sdk.PromptArgument{
			{Name: "collection", Description: "collection name", Required: true},
			{Name: "operation", Description: "list, view, create, update, delete", Required: true},
			{Name: "details", Description: "who calls it, the error, the record id if any"},
		},
	}, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		a := req.Params.Arguments
		return promptResult("debug-403", fmt.Sprintf(`Debug a denied %s request on collection %q.
Details: %s

Steps:
1. rule.explain for (%s, %s): is the rule locked (null), public ("") or an expression?
2. If an expression: check which conditions need @request.auth, @request.body or relations the caller may lack. With operator role, pass record_id and as_user_id to see who passes.
3. deny.tail (operator) shows the recorded reason and rule_kind of recent 401/403/429 responses; lockout.list shows locked identities.
4. Remember a list rule filters silently: an empty list is not a 403. View/update/delete outside the rule answer 404/403.
5. Report the root cause and the smallest rule or data change that fixes it. Do not change rules yourself.`,
			a["operation"], a["collection"], a["details"], a["collection"], a["operation"]))
	})

	s.sdk.AddPrompt(&sdk.Prompt{
		Name: "design-schema", Description: "Design collections, fields, relations and rules for a feature.",
		Arguments: []*sdk.PromptArgument{
			{Name: "goal", Description: "what the application must store and who uses it", Required: true},
			{Name: "constraints", Description: "existing collections to reuse, scale, privacy needs"},
		},
	}, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		a := req.Params.Arguments
		return promptResult("design-schema", strings.TrimSpace(fmt.Sprintf(`Design the schema for: %s
Constraints: %s

Steps:
1. Read toki://schema and reuse existing collections where possible.
2. Propose collections with field names, types, required/unique flags, relations (and cascadeDelete choices) and indexes.
3. For every collection give the five rules. Default to locked (null) and open only what the use case needs; never use an empty rule for private data.
4. List the migration or Admin UI steps a human must run. You can not change the schema with these tools.
5. List risks: rule gaps, missing indexes for expected filters, fields that must be hidden.`,
			a["goal"], a["constraints"])))
	})
}
