# Module `roles`

Named roles and scoped, expiring memberships for any auth collection (and MCP agents), tested from collection rules with `@role()` and `@member()`. Package `modules/roles`.

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
| `scope_collection` | optional collection of the scope record (name or id, stored as id) |
| `expires` | optional date; the membership is ignored from that moment |

Unique index on (`user_collection`, `user`, `role`, `scope`). Deleting an auth record, or a record used as `scope` (with `scope_collection` set), deletes its memberships (through `app.Delete`, so audit and hooks run).

## Rule functions

A function cannot stand alone in a rule expression, compare it with `true`:

| Expression | True when the requesting auth record has |
| --- | --- |
| `@role("admin") = true` | an unexpired membership of role `admin` with empty scope |
| `@role("editor", team) = true` | an unexpired `editor` membership whose scope equals the field `team` (or a string literal); an empty scope operand is false |
| `@member(team) = true` | any unexpired membership with scope `team` |

`= false` / `!= true` negate. Guests (no auth) are always false. A global grant does not imply every scope: scopes are matched exactly. The role name must be a string literal; the scope may be a field identifier (single-value; a multi-value field is rejected) or a literal. Works in all rules, filters and in `fieldperm` rules, with both compilers (legacy and `TOKI_RULE_AST=1`); SQLite only (other dialects return `rule.ErrUnsupported`).

Example: `listRule = "@member(team) = true || @role(\"admin\") = true"`.

### SQL shape

```
EXISTS (SELECT 1 FROM `_memberships` AS m INNER JOIN `_roles` AS r ON r.id = m.role
  WHERE m.user = {:authId} AND {:authId} != '' AND m.user_collection = {:authCol}
    AND r.name = {:name} AND m.scope = ''            -- or: m.scope = <operand> AND <operand> != ''
    AND (m.expires = '' OR m.expires > {:now}))
```

All values are bound parameters; aliases are random per use so several calls compose. Performance: the unique index covers (`user_collection`, `user`, `role`, `scope`) so each check is an index seek per evaluated row; The result does not depend on the record when the scope is a literal or absent, but SQLite re-evaluates the subquery per row anyway (in `fieldperm` rules it is evaluated per record too). The subquery is not cached across rows by SQLite; for very large lists prefer a rule that narrows by an indexed field first.

## Go API

`roles.Has(app, authRecord, "admin", scope)` and `roles.IsMember(app, authRecord, scope)`. They use a per-auth-record cache with a 5 s TTL, invalidated at once by writes through this process (the same pattern as `fieldperm`); a failed load answers false. The SQL functions always query the database.

## CLI

```
toki roles list
toki roles create <name> [--description ...]
toki roles rm <name>                 # also deletes all its memberships
toki roles grant <collection>/<userId> <role> [--scope <id> --scope-collection <c> --expires <t>]
toki roles revoke <collection>/<userId> <role> [--scope <id>]
toki roles who <role> [--scope <id>] [--json]
toki roles lint                      # rules using @role("x") for a role that does not exist
```

`--expires` takes RFC3339, `YYYY-MM-DD` or a duration (`720h`). A running server picks up CLI changes within 5 seconds.
