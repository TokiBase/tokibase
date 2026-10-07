# Module `mcp`

A Model Context Protocol server so AI agents (Claude Code, Cursor, remote agents) can inspect and manage a TokiBase instance through typed tools, with their own identity, rule enforcement and audit. Package `modules/mcp`. No REST contract change.

PR 1 (this document): stdio transport and the core tools. Built with the official Go SDK `github.com/modelcontextprotocol/go-sdk` v1.8.0.

- `tokibase.go` calls `mcp.Register(app)` (creates `_agents`), wires the audit sink and the providers of other modules (`tokibase_mcp.go`), and adds the `mcp`, `agent` and `gen` commands.
- Build tag `no_mcp` excludes the module (a stub keeps `tokibase.go` compiling; the commands and the `_agents` collection are not created).
- Env `TOKI_MCP=off` is RESERVED for the HTTP transport of PR 2 (`mcp.HTTPEnabled()`); PR 1 has no HTTP part, so it changes nothing yet.

## Setup

1. Create an agent (prints the API key once, only a sha256 hash is stored):

   ```
   toki agent create reviewer --role writer --collections posts,comments --dir pb_data
   ```

2. Claude Code:

   ```
   claude mcp add tokibase -e TOKI_AGENT_KEY=tka_... -- toki mcp serve --dir pb_data
   ```

3. Cursor, `.cursor/mcp.json`:

   ```json
   { "mcpServers": { "tokibase": {
       "command": "toki",
       "args": ["mcp", "serve", "--dir", "pb_data"],
       "env": { "TOKI_AGENT_KEY": "tka_..." } } } }
   ```

`toki mcp serve` bootstraps the app from `--dir`, authenticates `TOKI_AGENT_KEY`, and speaks MCP on stdin/stdout (stdout carries the protocol only; messages go to stderr). Every call re-reads the agent, so `toki agent revoke` and role changes apply to running sessions on their next call. The process is a second process on the same `pb_data` as `toki serve`: SQLite WAL allows it, and the audit chain re-reads its head on insert conflict; walreplica does not start in it.

## Agent identity: `_agents`

System collection in the main DB (`data.db`, because rules and exports may reference it), created with `EnsureCollection` at bootstrap when missing. All API rules are null (superusers only); `_agents` can never be read or written through MCP, not even by operators.

| Field | Type | Notes |
| --- | --- | --- |
| `name` | text | unique, `a-z 0-9 _ . -`, 1-63 chars |
| `key_hash` | text, hidden | sha256 of the API key (`tka_` + 40 random chars), unique |
| `role` | select | `reader` \| `writer` \| `operator` |
| `collections` | json | allowlist of collection names, empty = all |
| `rate_per_min` | number | default 120 |
| `enabled` | bool | `agent revoke` sets false (the row stays so the audit trail resolves the name) |
| `created`, `updated` | autodate | |

## Roles

| Role | Can |
| --- | --- |
| `reader` | `schema.list`, `schema.describe` (no samples), `records.query/get`, `rule.lint`, `rule.explain` (rule text) |
| `writer` | reader + `records.create/update/delete/batch` |
| `operator` | writer + sample records, `audit.tail/verify`, `backup.verify`, `replica.status`, `deny.tail`, `lockout.list`, `rule.explain` with `record_id`/`as_user_id`; bypasses collection rules (superuser-like) |

The `collections` allowlist applies to every role. Collections outside it answer "not found or not accessible" (no existence leak). System collections (`_superusers`, `_otps`, ...) are readable by operators only and writable by nobody; `_agents` by nobody.

## Limitations of PR 1 (read this)

- **Rules are evaluated as GUEST for reader and writer.** An agent is not a request auth record yet, so `@request.auth.*` conditions fail for it: a reader/writer can only read what a guest could read (list/view rule `""` or an expression that does not need auth). A `null` (locked) rule means no access unless the agent is an operator.
- **Writes are authorized by role + allowlist, not by the create/update/delete rules.** The allowlist is the explicit grant. Writes go through `app.Save`/`app.Delete` (model validation and model hooks run; request hooks and the REST audit hooks do not, so the MCP audit sink records them instead). Writers can therefore write a collection whose create rule would deny guests. Grant narrowly with `--collections`.
- **Operator bypasses rules** and is audited on every call (reads included).
- Schema and rules can not be changed through MCP (no collection/settings tools).
- stdio only, one agent per process; no HTTP transport, no sandbox mode.
- `rule.explain` evaluates "agent" as pass for operators and as guest otherwise; it cannot evaluate create rules (no record yet).

## Tools

| Tool | Role | Arguments |
| --- | --- | --- |
| `schema.list` | reader | none; collections visible to the agent with rule state (locked/public/expression) and counts |
| `schema.describe` | reader | `collection`; fields (types/options), all rules, indexes; operator: 2 sample records (strings cut to 40 chars, fields containing password/token/secret/key redacted) |
| `records.query` | reader | `collection`, `filter?`, `sort?`, `page?`, `perPage?` (max 200), `expand?`, `fields?` |
| `records.get` | reader | `collection`, `id`, `expand?`, `fields?` |
| `records.create` | writer | `collection`, `data`, `reason` |
| `records.update` | writer | `collection`, `id`, `data`, `reason` |
| `records.delete` | writer | `collection`, `id`, `reason`, `confirm_token?` |
| `records.batch` | writer | `ops` (max 500, atomic), `reason`, `confirm_token?` |
| `rule.lint` | reader | none; public rules not allowlisted in `ruleguard.json` |
| `rule.explain` | reader | `collection`, `operation` (list/view/create/update/delete, auth/manage on auth collections), `as_user_id?`, `record_id?` (operator only for the last two) |
| `audit.tail` | operator | `since?` (`1h`, `7d`, `2026-10-01`), `limit?` |
| `audit.verify` | operator | none |
| `backup.verify` | operator | `name?` (default latest) |
| `replica.status` | operator | none |
| `deny.tail` | operator | `since?`, `limit?` |
| `lockout.list` | operator | none |

Operator tools answer "module not enabled" when the module is off (for example `TOKI_AUDIT=off`).

### Reasons, plans and confirmation

- `reason` (at least 3 characters) is required for every write and is stored in the audit entry.
- `records.delete` is always two-step. Without `confirm_token` it deletes nothing and returns a plan (preview of the record, cascade warnings) plus a single-use token valid 5 minutes. Calling again with IDENTICAL arguments plus the token executes. The token is bound to the agent, the tool and the arguments; a mismatch or reuse burns it.
- `records.batch` runs directly when it has no delete and at most 100 operations; otherwise the same plan/token flow applies. Execution is one transaction: any failing operation rolls back everything.
- Field names in `data` are validated against the schema (modifiers like `tags+` are accepted). Hidden fields are never returned.

## Resources and prompts

| URI / name | Content |
| --- | --- |
| `toki://schema` | JSON of the collections visible to the agent (fields, rules, indexes), no sample data |
| `toki://docs/rules` | concise markdown about the rule language |
| `toki://instance` | version, profile, enabled modules, the calling agent, session id |
| prompt `write-rule` | `collection`, `operation`, `intent` |
| prompt `debug-403` | `collection`, `operation`, `details?` |
| prompt `design-schema` | `goal`, `constraints?` |

## Audit

Every write tool call (plans, denied attempts and failures included) and every operator call is passed to the sink set with `mcp.SetAuditSink(func(action, collection, record string, details map[string]any))`, wired in `tokibase.go` to the `_audit` chain with `actor_kind = agent`, `actor_id` = agent record id, `actor_collection = _agents`. Action is `agent.<tool>` (for example `agent.records.create`). `details` (stored in `after`) has `agent`, `agent_id`, `role`, `session` (random per server run), `reason`, and for writes the redacted `data`, `dry_run`, `denied`, `error`. `toki agent create|revoke` emit `agent.created` / `agent.revoked` with actor `system`. With `TOKI_AUDIT=off` nothing is recorded.

## Rate limit

Token bucket per agent: capacity and refill follow `rate_per_min` (default 120). An exceeded call returns an MCP tool error (`rate limit exceeded`). The bucket lives in the server process.

## CLI

```
toki agent create <name> [--role reader|writer|operator] [--collections a,b] [--rate 120]
toki agent list [--json]
toki agent revoke <name>
toki mcp serve                       # needs TOKI_AGENT_KEY; stdio
toki gen agents-md [--out AGENTS.md] [--force]   # AGENTS.md + llms.txt next to it
```

`gen agents-md` writes the collections (fields and a rules summary, system collections left out), the enabled modules, how to connect and the rules of engagement; it refuses to overwrite existing files without `--force`.

## Roadmap (PR 2)

- Streamable HTTP transport (behind `TOKI_MCP=off|on`), agent keys as bearer tokens.
- `@request.auth.kind = "agent"` available in collection rules, so agents get real per-collection rules instead of guest evaluation.
- Sandbox mode (writes into a throw-away copy of `pb_data`, diff before apply).
