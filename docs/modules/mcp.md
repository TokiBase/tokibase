# Module `mcp`

A Model Context Protocol server so AI agents (Claude Code, Cursor, remote agents) can inspect and manage a TokiBase instance through typed tools, with their own identity, rule enforcement and audit. Package `modules/mcp`. No REST contract change.

PR 1: stdio transport and the core tools. PR 2: streamable HTTP transport, agent auth kind in rules, sandbox mode. Built with the official Go SDK `github.com/modelcontextprotocol/go-sdk` v1.8.0.

- `tokibase.go` calls `mcp.Register(app)` (creates `_agents`), wires the audit sink and the providers of other modules (`tokibase_mcp.go`), and adds the `mcp`, `agent` and `gen` commands.
- Build tag `no_mcp` excludes the module (a stub keeps `tokibase.go` compiling; the commands and the `_agents` collection are not created).
- Env `TOKI_MCP=on` mounts the streamable HTTP transport at `/api/mcp` (default off; stdio is always available). Without the `no_mcp` tag only.

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
| `collections` | json | allowlist of collection names, empty = all. A value that is not a JSON array of strings makes the agent invalid: every call is denied (fail closed) and the problem is logged |
| `rate_per_min` | number | default 120 |
| `enabled` | bool | `agent revoke` sets false (the row stays so the audit trail resolves the name) |
| `sandbox` | bool | writes are dry runs (see Sandbox mode) |
| `expires` | date | optional; after it the key is refused (HTTP 403, stdio start fails, running sessions stop on the next call) |
| `created`, `updated` | autodate | |

## Roles

| Role | Can |
| --- | --- |
| `reader` | `schema.list`, `schema.describe` (no samples), `records.query/get`, `rule.lint`, `rule.explain` (rule text) |
| `writer` | reader + `records.create/update/delete/batch` |
| `operator` | writer + sample records, `audit.tail/verify`, `backup.verify`, `replica.status`, `deny.tail`, `lockout.list`, `rule.explain` with `record_id`/`as_user_id`; bypasses collection rules (superuser-like) |

The `collections` allowlist applies to every role, also inside `expand`, filters, sorts and cascades. Collections outside it answer "not found or not accessible" (no existence leak). **Reserved collections** (`System` OR a name starting with `_`, for example `_webhooks`, `_field_rules`, `_superusers`, `_otps`) are readable by operators only and writable by nobody; `_agents` by nobody. Module config collections (`_webhooks`) are created with `System = true`; the `_` prefix rule also covers existing ones.

Requests that reach other collections through `expand`, `@collection.<name>` or relation paths in `filter`/`sort` are rejected with an error listing the collections outside the allowlist (for every role; text inside quotes is ignored).

## Rules, limitations (read this)

- **Rules are evaluated with the agent as request auth** (PR 2). For reader and writer, `@request.auth.id` is the agent id, `@request.auth.collectionName` is `_agents`, `@request.auth.role` its role, `@request.auth.kind` is `"agent"`, and the other `_agents` fields resolve like fields of any auth record. A rule `@request.auth.kind = "agent"` therefore opens a collection to agents only; `@request.auth.kind = "user"` denies them. **Careful:** the common rule `@request.auth.id != ""` ("any logged in client") now also admits every agent (it used to deny agents, which were guests). Existing rules that must stay user-only need `@request.auth.kind = "user"` (or `@request.auth.collectionName = "users"`). A `null` (locked) rule still means no access unless the agent is an operator. Operators keep superuser-like behaviour (rules bypassed). See `docs/RULE_ENGINE.md`, "Request auth kind".
- **Writes are authorized by role + allowlist, not by the create/update/delete rules.** The allowlist is the explicit grant. Writes go through `app.Save`/`app.Delete` (model validation and model hooks run; the MCP audit sink records them). Writers can therefore write a collection whose create rule would deny guests. Grant narrowly with `--collections`.
- **Field level permissions apply (fieldperm).** For reader/writer, record reads go through `apis.EnrichRecords` with a synthetic guest request, so every `OnRecordEnrich` handler (fieldperm read rules) hides fields, also in expanded relations. Writes (create, update, batch) run the `OnRecordCreateRequest`/`OnRecordUpdateRequest` hook chain with a synthetic guest request whose body is `data` and a no-op terminal handler, so fieldperm write rules (and other request hooks, for example timelint) reject the write before `app.Save`. Operators are superuser-like and skip both. Modules still do not import each other. Side effect: any user hook bound to those request hooks also sees a guest request without HTTP details.
- **What a write returns.** `records.create`/`update` return the record only when it passes the view rule as an agent (and after enrichment); otherwise just `{id, collection}`. The `records.delete` plan preview follows the same rule. `update` with empty `data` therefore no longer reads locked records.
- **Delete cascades.** A delete can cascade or set null in other collections. The plan only says it "may also delete or modify records in other collections" and names the allowed ones. For non-operators the delete is denied when any target (transitively through cascadeDelete) is outside the allowlist or reserved.
- **The confirm token is a speed bump, not a control**, because it is handed to the agent that asked. For a human gate set `TOKI_MCP_REQUIRE_HUMAN_CONFIRM=1` on the MCP server: tokens are then only valid after an operator runs `toki agent confirm <token>` (state in `<dir>/.mcp_confirm/`, mode 0600, 5 minutes). An agent with shell access to the instance can run that command too; keep it away from the shell. The plan is bound to collection and id, not to the record content.
- **Audit required for writes.** If no audit sink is wired (`TOKI_AUDIT=off`), write tools answer an error unless the operator exports `TOKI_MCP_UNAUDITED=1`; `toki mcp serve` prints a warning on stderr either way. Reads by reader/writer are not audited; a failed audit append is only logged.
- **Operator bypasses rules** and is audited on every call (reads included).
- Schema and rules can not be changed through MCP (no collection/settings tools).
- stdio: one agent per process. HTTP: one server (and rate limit bucket) per agent key, many sessions.
- `rule.explain` evaluates "agent" as pass for operators and as an agent otherwise; it cannot evaluate create rules (no record yet).

## Tools

| Tool | Role | Arguments |
| --- | --- | --- |
| `schema.list` | reader | none; collections visible to the agent with rule state (locked/public/expression); record counts for operators only |
| `schema.describe` | reader | `collection`; fields (types/options, relation targets outside the allowlist masked), all rules, indexes; operator: also `viewQuery` and 2 sample records (strings cut to 40 chars, fields matching password/secret/token/key/authorization/cookie/headers redacted) |
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

Denied calls are audited at most once per minute per agent and tool; the row has `denied_suppressed_since_last` with the number of denials folded into it. The redaction key regex is `(?i)(password|secret|token|key|authorization|cookie|headers)`.

## Rate limit

Token bucket per agent: capacity and refill follow `rate_per_min` (default 120). The token is taken BEFORE the role check, so denied calls count. An exceeded call returns an MCP tool error (`rate limit exceeded`). The bucket lives in the server process (several `toki mcp serve` processes each have their own).

## Robustness

- Tool errors are generic for unexpected failures (`internal error`; details go to the server log), field validation messages are kept. Filter/sort errors answer `query failed: invalid filter, sort or parameters`.
- A panic in a tool handler is recovered and answered as `internal error`; the process keeps serving.
- Tool input and tool result are capped at 1 MB; `records.query` has a 30 s context timeout.
- Rule text cut to 80 characters in AGENTS.md is cut on rune boundaries.

## CLI

```
toki agent create <name> [--role reader|writer|operator] [--collections a,b] [--rate 120] [--sandbox] [--expires 90d|2026-12-31]
toki agent list [--json]
toki agent revoke <name>
toki agent confirm <token>           # approve a pending plan (TOKI_MCP_REQUIRE_HUMAN_CONFIRM=1)
toki mcp serve                       # needs TOKI_AGENT_KEY; stdio
toki gen agents-md [--out AGENTS.md] [--force]   # AGENTS.md + llms.txt next to it
```

`gen agents-md` writes the collections (fields and a rules summary, system collections left out), the enabled modules, how to connect and the rules of engagement; it refuses to overwrite existing files without `--force`.

## HTTP transport (PR 2)

`TOKI_MCP=on` mounts the MCP streamable HTTP transport (official go-sdk `NewStreamableHTTPHandler`) at `/api/mcp` on the app router. Default off; with `TOKI_MCP` unset or `off` the route does not exist (404), and neither does it with the `no_mcp` build tag.

```
claude mcp add --transport http tokibase https://example.com/api/mcp --header "Authorization: Bearer tka_..."
```

- **Auth:** `Authorization: Bearer tka_...`, the same keys as stdio. The sha256 of the key is looked up in `_agents` and compared in constant time. Missing, malformed or unknown key: **401** (`WWW-Authenticate: Bearer`). Agent revoked, expired (`expires`) or with a malformed allowlist: **403**. After 30 failed attempts per client address and minute: **429**.
- **Sessions:** the SDK session (`Mcp-Session-Id`) is bound to the agent id; another key can not use it (403). Idle sessions are closed after 30 minutes (`TOKI_MCP_SESSION_TIMEOUT`, Go duration). Every call re-reads the agent, so revoking, disabling or changing the role applies to open sessions on their next call.
- **Rate limit:** the global rate limiter of the app applies (settings rate-limit rules match the path prefix `/api/mcp`, label `/api/mcp`), in addition to the per-agent `rate_per_min` bucket shared by all sessions of one key.
- **deny log:** 401/403/429 answers appear in `deny.tail` like any other request (denylog middleware). The failed attempt throttle above is separate from `lockout`, which keys on user identities.
- **Audit:** same as stdio; the details carry `transport: "http"`.
- Put TLS in front (the key is a bearer secret). Behind a proxy configure the trusted proxy headers of the app so the failed-attempt throttle sees client addresses.

## Sandbox mode (PR 2)

```
toki agent create tryout --role writer --collections posts --sandbox
```

For a sandbox agent (`_agents.sandbox = true`) `records.create`, `records.update`, `records.delete` (after the confirm step) and `records.batch` run inside a database transaction that is **always rolled back**. Validation, model hooks, field permissions and the view rule run as for a real write; the answer is what the write would have returned (with a generated id) plus `"sandbox": true, "dry_run": true, "rolled_back": true`. Nothing persists, and the `After*Success` model hooks do not fire (they run after commit). Hooks that act on the outside world in a `Before*` or inside-transaction handler still run. The call is audited with `sandbox` and `dry_run` set. The change applies on the next call, also for open sessions. It is not a copy of `pb_data`: reads see the real data.

## Deferred

- Sandbox as a throw-away copy of `pb_data` with a diff before apply (the PR 2 sandbox is the rollback form).
- OAuth for MCP clients (protected resource metadata), per-IP lockout integration with the `lockout` module.

## Encrypted fields

Record exports, delete previews and operator samples replace encrypted fields (`kernel.IsSensitive`) by `"[encrypted]"`, also inside expanded relations: an agent never receives their ciphertext or plaintext, whatever its role.
