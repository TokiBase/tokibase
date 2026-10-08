# Module `roles`

Named roles and scoped, expiring memberships for any auth collection and for MCP agents (`_agents`), tested from collection rules with `@role()` and `@member()`. Package `modules/roles`.

- Enabled by default: `tokibase.go` calls `roles.Register(app)` and adds the `roles` command. Tag `no_roles` compiles it out (profiles edge and nano).
- Creates the system collections at boot. Rules of both are `null` (superusers only); writes go through the normal record path, so the audit log and hooks see them.

## Schema

`_roles`: `name` (unique, required), `description`, `created`, `updated`.

`_memberships`:

| Field | Meaning |
| --- | --- |
| `user_collection` | auth collection of the holder. The name or id is accepted on write and stored as the id (survives renames). `_agents` for MCP agents |
| `user` | record id of the holder |
| `role` | relation to `_roles` (cascade delete: deleting a role deletes its memberships) |
| `scope` | optional record id the grant is limited to (team, tenant, clan); empty = global |
| `scope_collection` | collection of the scope record (name or id, stored as id). **Required when `scope` is set** (record ids are only unique per collection); cleared when `scope` is empty |
| `expires` | optional date; the membership is ignored from that moment |

`user` must exist in `user_collection` and role names must not have surrounding whitespace or contain quotes/backslashes/control characters (checked on write). Unique index on (`user_collection`, `user`, `role`, `scope`). Deleting an auth record (or agent), or a record used as `scope`, deletes its memberships inside the same transaction as the delete (through `app.Delete`, so audit and hooks run; if any step fails everything rolls back). Deleting a whole collection deletes the memberships it holds or is scope of. Because `user_collection`/`scope_collection` are plain text, not relations, they are never validated on the database level; only writes through the record path are checked.

## Rule functions

A function cannot stand alone in a rule expression, compare it with `true`:

| Expression | True when the requesting auth record has |
| --- | --- |
| `@role("admin") = true` | an unexpired membership of role `admin` with empty scope |
| `@role("editor", team, "teams") = true` | an unexpired `editor` membership whose scope equals the field `team` (or a string literal) AND whose `scope_collection` is the collection `teams` (name or id); an empty scope operand is false |
| `@member(team, "teams") = true` | any unexpired membership with scope `team` in collection `teams` |

The scope collection is a mandatory string literal (name or id): `@role("x", team)` and `@member(team)` are refused. A relation field cannot tell the rule compiler which collection it points to, so say it explicitly; this stops a record of another collection that reuses the id (ids can be chosen by clients) from matching someone else's grant. `= false` / `!= true` negate. Guests (no auth) are always false. A global grant does not imply every scope: scopes are matched exactly. The role name must be a string literal; the scope may be a field identifier (single-value; a multi-value field is rejected) or a literal. Works in all rules, filters (including a `filter=` sent by a client; it can only observe the caller's own memberships) and in `fieldperm` rules, with both compilers (legacy and `TOKI_RULE_AST=1`); SQLite only (other dialects return `rule.ErrUnsupported`). MCP agents (`_agents`) can hold roles and are checked through the Go API; they are not `@request.auth` records, so rules do not see them.

Example: `listRule = "@member(team, \"teams\") = true || @role(\"admin\") = true"`.

### SQL shape

```
EXISTS (SELECT 1 FROM `_memberships` AS m INNER JOIN `_roles` AS r ON r.id = m.role
  WHERE m.user = {:authId} AND {:authId} != '' AND m.user_collection = {:authCol}
    AND r.name = {:name} AND m.scope = ''            -- or: m.scope = <operand> AND <operand> != ''
    AND m.scope_collection IN (SELECT id FROM _collections WHERE id = {:c} OR name = {:c})   -- scoped form only
    AND (m.expires = '' OR m.expires > {:now}))
```

All values are bound parameters; aliases are random per use so several calls compose. Performance: the unique index covers (`user_collection`, `user`, `role`, `scope`) so each check is an index seek per evaluated row; The result does not depend on the record when the scope is a literal or absent, but SQLite re-evaluates the subquery per row anyway (in `fieldperm` rules it is evaluated per record too). The subquery is not cached across rows by SQLite; for very large lists prefer a rule that narrows by an indexed field first.

## Go API

`roles.Has(app, authRecord, "admin", scope, scopeCollection)` and `roles.IsMember(app, authRecord, scope, scopeCollection)` (scope collection by name or id; an unknown one never matches; `Has` with an empty scope asks for the global grant, unlike the SQL function where an empty scope operand is always false). They use a per-auth-record cache with a 5 s TTL, invalidated by writes through this process (a load that overlaps an invalidation is not cached; the same pattern as `fieldperm`); a failed load answers false. The SQL functions always query the database.

## CLI

```
toki roles list
toki roles create <name> [--description ...]
toki roles rm <name>                 # also deletes all its memberships
toki roles grant <collection>/<userId> <role> [--scope <id> --scope-collection <c> --expires <t>]   # --scope needs --scope-collection; holder may be _agents
toki roles revoke <collection>/<userId> <role> [--scope <id>]
toki roles who <role> [--scope <id>] [--json]
toki roles lint                      # rules using @role("x") for a role that does not exist (also hints at case differences)
```

`--expires` takes RFC3339, `YYYY-MM-DD` or a duration (`720h`). A running server picks up CLI changes within 5 seconds.

## Limits

`toki roles lint` scans the five collection rules (and auth/manage rules) for `@role("x")` with a literal first argument; it does not scan `fieldperm` rules, does not check `@member`, and does not warn about roles without memberships. The unique index is (`user_collection`, `user`, `role`, `scope`) and does not include `scope_collection`, so one holder cannot have the same role for the same id in two collections.
