# Rule engine (phase 2, PR 1 + PR 2)

The filter/rule language (the PocketBase `fexpr` grammar used by list/view/create/update/delete
rules, `?filter=`, `FindRecordsByFilter`, ...) is now split into a parser that produces a
dialect-neutral AST and an emitter that turns the AST into SQL.

```
string --fexpr.Parse--> []fexpr.ExprGroup --rule.Parse--> *rule.AST --EmitAST(dialect)--> dbx.Expression
                                                                 \--(later) in-memory evaluator

identifier --FieldResolver.Resolve--> rule.Ref (alias, column, shape, modifier, path)
                                         + joins / multi-match subquery description
                                         \--Ref.Emit(Dialect)--> SQL fragment

rule.Dialect  <-- kernel/rule/sqlite (byte-identical to the legacy compiler)
              <-- kernel/rule/pg     (PostgreSQL, PR 2)
              <-- a store module's own dialect (see "Plugging a dialect in")
```

Status: the AST path is **opt-in** (`TOKI_RULE_AST=1`, default off). With the variable unset
production behavior is the legacy fused compiler. COMPAT: no change (SQL and parameters are identical).

## Packages

| Package | Role |
| --- | --- |
| `kernel/rule` | AST types and `Parse(expr) (*AST, error)`. No SQL, no dbx, no store. Depends only on fexpr. |
| `kernel/rule` (`dialect.go`) | `Dialect` interface, `Ref` (typed result of resolving an identifier), `ErrUnsupported`. Still no SQL text beyond what the dialects return. |
| `kernel/rule/sqlite` | The SQLite `Dialect`. Reproduces the previous SQL byte for byte; the legacy path always uses it. |
| `kernel/rule/pg` | The PostgreSQL `Dialect` and `Emit` / `EmitWithLimit`. |
| `kernel/rule/sql` | `Emit(ast, resolver)` / `EmitWithLimit`: SQLite emitter entry point. |
| `tools/search` (`ast_emit.go`) | Emitter implementation `EmitAST` and the `TOKI_RULE_AST` switch in `FilterData.BuildExprWithLimit`. It lives here because it shares the unexported operator helpers (`buildResolversExpr`, `resolveEqualExpr`, `wrapLikeParams`, multi-match expressions) with the legacy path: one implementation of the operators. `kernel/rule/sql` imports `tools/search`, so the reverse import is impossible. |

## AST shape

```
AST{Source, Root *Group}
Group{Items []Item}            // parenthesized / root sequence
Item{Join (&& ||), Node}       // Node = *Group | *Comparison; Join links to the previous item
Comparison{Pos, Left, Op, Right}
Operand = *Ident | *Literal | *Call
Ident{Name, Kind, Path, Modifier, CollectionAlias}
Literal{Kind string|number, Value}
Call{Name, Args []Operand}
```

The tree mirrors fexpr's output on purpose (including how nested groups are represented) because
group nesting decides the parenthesization of the emitted SQL (`concatExpr`).

`Pos.Expr` is the 1-based ordinal of the comparison in source order. fexpr's scanner does not expose
byte offsets and the tokenizer was deliberately not rewritten, so offsets are not available yet.

## Grammar table

| Construct | Syntax | AST |
| --- | --- | --- |
| Groups, joins | `( ... )`, `&&`, `\|\|` (no precedence between joins: left to right as upstream) | `Group`, `Item.Join` |
| Comparison | `=` `!=` `>` `>=` `<` `<=` `~` `!~` | `Comparison.Op` |
| Any-match | `?=` `?!=` `?>` `?>=` `?<` `?<=` `?~` `?!~` | `Op.IsAny()` |
| Field path | `a`, `a.b.c`, `rel.title`, `demo_via_rel.col` | `Ident{Kind: KindField, Path}` |
| Modifiers | `:isset` `:length` `:each` `:lower` (and `:changed`) | `Ident.Modifier` |
| Request | `@request.context\|method\|query.*\|headers.*\|body.*\|auth.*` | `Ident{Kind: KindRequest}` |
| Cross collection | `@collection.name[:alias].field` | `Ident{Kind: KindCollection, CollectionAlias}` |
| Time macros | `@now @yesterday @tomorrow @second @minute @hour @day @month @weekday @year @todayStart @todayEnd @monthStart @monthEnd @yearStart @yearEnd` | `Ident{Kind: KindMacro}` |
| Strings | `'a'`, `"a"` with escapes | `Literal{String}` |
| Numbers | `1`, `-2.5`, `1e3` | `Literal{Number}` |
| null/true/false | identifiers, case-insensitive | `Ident{Kind: KindKeyword}` |
| Functions | `geoDistance(lonA, latA, lonB, latB)`, `strftime(fmt, [time, mods...])` | `Call` |
| Comments | `// ...`, `/* ... */` | dropped by the scanner |

Keywords stay identifiers (not literal nodes) because the resolver gets the first chance to
resolve a column named `null`, `true` or `false`; only if resolution fails does the emitter fall back
to `NULL`, `1`, `0`. The `Kind`/`Path`/`Modifier` classification is informational: resolvers always
receive the full identifier string.

Placeholders (`{:name}`) are still substituted textually before parsing, exactly as before.

## Request auth kind

`@request.auth.kind` is resolved by the field resolver (`kernel.RecordFieldResolver`), not by the parser or an emitter, so the legacy compiler and the AST path produce the same SQL. Values (`kernel.AuthKindOf`): `guest` (no auth), `user` (any auth collection but `_superusers`), `superuser`, `agent` (an MCP agent: `RequestInfo.Auth` is a record of the system collection `_agents`). It is a bound parameter like other static `@request.*` values. If the auth collection has a real field `kind`, that field wins (backward compatibility). For agents only `@request.auth.id`, `.collectionId`, `.collectionName` and `.kind` resolve at the top level; the agent record is exposed under `@request.auth.agent.*` (`agent.role` = `reader|writer|operator`, `agent.name`, ...; never the key hash) and every other `@request.auth.<name>` is empty, so a user rule on `@request.auth.role` does not match an agent. Implemented in the field resolver (`kernel.AuthAgentNamespace`); operators bypass rules. Note `@request.auth.id != ""` is true for agents too: use `@request.auth.kind = "user"` for user-only rules. See `docs/modules/mcp.md`.
## Resolver / emitter split (PR 2)

`kernel/record_field_resolver_runner.go` still walks the identifier and registers joins, but it no longer writes
dialect specific SQL. The final segment of an identifier is described as a `rule.Ref` (`Kind`: column, JSON member,
array length; `Alias`, `Column`, `Path` segments, `Lower`) and rendered by `Ref.Emit(dialect)`, once for the main query
alias and once for the multi-match alias. Everything that is not plain SQL goes through `rule.Dialect`:

| Method | Used for |
| --- | --- |
| `JSONExtract`, `JSONArrayLength` | json/geoPoint members, `:length` |
| `JSONEach`, `JSONEachParam` | `:each`, multi-value relation hops, `@request.body.*:each` (table valued joins) |
| `JSONArrayMember` | back-relation through a multi-value relation field (`id IN each(field)`) |
| `Keyword` | `null`/`true`/`false` when no such field exists |
| `TextOf`, `CoalesceEmpty` | `''`/NULL normalization of `=` and `!=` |
| `NullSafeEq` | `IS` / `IS NOT` (no coalesce fallback and `!=`) |
| `Like` | `~`, `!~` (the `%` wrapping of a column operand, escape character) |
| `OptionalOn` | joins registered without an ON clause |
| `ExistsNone`, `ExistsNoneMany` | many<->one and many<->many multi-match subqueries |
| `GeoDistance`, `Strftime` | the built-in filter functions |

Identifier quoting stays with dbx: fragments carry `[[alias.column]]` / `{{table}}` markers that the dbx driver
quotes (backticks for SQLite, double quotes for the pgx driver). `{:name}` placeholders are dbx's too.

Operator helpers (`buildResolversExpr`, `resolveEqualExpr`, the multi-match expressions) are still shared by the legacy
and the AST path and take the dialect as a parameter. The legacy path is SQLite only. A `FieldResolver` that implements
`search.DialectResolver` (`RecordFieldResolver` does) makes `FilterData.BuildExpr` use the AST path with its dialect, so
nested expressions the resolver builds itself (`:changed`, joined collection list rules) stay in the same dialect.
`EmitASTWithDialect` rejects an emitter/resolver dialect mismatch.

## What is still fused

- Join registration and the multi-match join list are built by the resolver (aliases, `registerJoin`) as before;
  only their dialect specific parts moved behind `Dialect`.
- Filter functions (`search.TokenFunctions`) still take `fexpr.Token` arguments; the emitter converts AST
  operands back with `rule.ToToken`. For SQLite the exported map is used (custom functions can be registered); other
  dialects get only `geoDistance` and `strftime`, a custom function is an error there.
- Random placeholder/alias names (`security.PseudorandomString`) are generated while resolving; the order of the
  calls is unchanged (the differential tests compare names too).

## PostgreSQL emitter

```go
r := kernel.NewRecordFieldResolver(app, collection, requestInfo, false)
r.SetDialect(pg.Dialect)
ast, _ := rule.Parse(`@request.auth.id != "" && title ~ "a"`)
expr, err := pg.Emit(ast, r) // then query.AndWhere(expr); r.UpdateQuery(query)
```

SQL examples (rendered with double quotes, `testdata/rule_pg_golden.txt` has about 55):

```
text ~ 'abc'        CAST([[demo1.text]] AS TEXT) ILIKE {:p1} ESCAPE '\'          (p1 = "%abc%")
text != 'abc'       [[demo1.text]] IS DISTINCT FROM {:p1}
text = null         (CAST([[demo1.text]] AS TEXT) = '' OR [[demo1.text]] IS NULL)
json.a.b = 'x'      (to_jsonb([[demo1.json]]) #>> '{a,b}') IS NOT DISTINCT FROM {:p1}
```

Type assumptions (documented in `kernel/rule/pg`):

- json, geoPoint and multi-value fields are `jsonb` (`json` also works, values go through `to_jsonb`); a multi-value
  field is a JSON array, a single-value field a plain scalar. A `geoPoint` is `{"lon":..,"lat":..}`.
- date fields are text in the PocketBase format or `timestamptz`; time macros and literals are bound as text
  (`2026-10-07 12:34:56.789Z`), which PostgreSQL parses for `timestamptz` and compares lexicographically for text.
- bool is `boolean`, number is `double precision`/`numeric`.
- JSON members are text (`#>>`). Comparing one with a number needs an explicit cast in the stored data model; the
  emitter does not guess. `geoDistance` casts its arguments to `double precision` itself.
- `~` uses `ILIKE`. SQLite's LIKE is case-insensitive for ASCII only, `ILIKE` follows the database collation: non-ASCII
  letters can match differently.
- Non-text operands of `=`/`!=`/`~` are compared through `CAST(.. AS TEXT)` where the SQLite code relies on implicit
  type mixing (the cast defeats an index on that column; plain `col = {:param}` comparisons are not cast).

Unsupported constructs (the emitter returns an error wrapping `rule.ErrUnsupported`, never different SQL):

| Construct | Why |
| --- | --- |
| `field:each` on a multi-value field, multi-value relation hops (`rel_many.title`, `@collection.x.rel_many.y`), `@request.body.field:each` | need a table valued join (`json_each`); PostgreSQL needs `LATERAL jsonb_array_elements_text(..) AS alias(value)`, which the join builder (`registerJoin` / `UpdateQuery` / multi-match `Join`) cannot express yet |
| `strftime(...)` | SQLite format specifiers and modifiers (`start of month`, `+1 day`) have no PostgreSQL equivalent; would need a translation layer |
| custom `search.TokenFunctions` entries | SQLite SQL by definition |

Supported but different from SQLite: back-relations through a multi-value relation field use
`id IN (SELECT jsonb_array_elements_text(..))` (unit tested, not in the golden corpus). `geoDistance` propagates NULL
explicitly (`LEAST`/`GREATEST` ignore NULLs, SQLite's `min`/`max` do not).

There is no PostgreSQL in CI: the emitter is tested by golden SQL and by comparison with the SQLite output (see Testing).
Nothing here selects PostgreSQL at runtime; the store is still SQLite and `pg.Emit` is a building block for a store module.

## Plugging a dialect in (store modules)

1. Implement `rule.Dialect` (all methods; return `rule.ErrUnsupported`-wrapped errors for what you cannot express).
   Start from `kernel/rule/pg` for a server database.
2. Give the record resolver your dialect: `resolver.SetDialect(d)`.
3. Emit with `search.EmitASTWithDialect(ast, resolver, limit, d)` (wrap it like `pg.Emit`), or call
   `FilterData.BuildExpr(resolver)`, which picks up the resolver's dialect.
4. Use the same resolver for `UpdateQuery(query)`: joins without a condition get `TRUE` when `OptionalOn()` is non-empty.
5. Add the dialect to `tools/search/rule_ast_pg_test.go` (structural parity + golden file).

## How to add an emitter

1. Consume `*rule.AST` only (`rule.Parse` once, cache by source string).
2. Walk `Group`/`Item`/`Comparison`; honor `Item.Join` (left to right) and keep nesting for parentheses.
3. For every `Ident`, delegate to a resolver for your dialect; apply the same null-fallback semantics for `=`/`!=`
   and the `LIKE` wrapping/escaping for `~`/`!~` (see `resolveEqualExpr`, `wrapLikeParams`).
4. Enforce the max comparisons limit (one per `Comparison`, error `search.ErrFilterExprLimit`).
5. Add the emitter to the differential harness (`tools/search/rule_ast_diff_test.go`) against the SQLite output
   where semantics are shared (for another dialect: `tools/search/rule_ast_pg_test.go`).

## Testing

- `kernel/rule/parse_test.go`: errors identical to fexpr, structure, identifier classification, token round trip.
- `tools/search/rule_ast_diff_test.go`: builds every expression with the legacy path and the AST path and compares
  the full query SQL and parameters byte-for-byte (the pseudorandom source and the clock are pinned), or the error text
  when building fails. Corpus: hand written expressions, an operator x operand matrix, every string literal in
  `*_test.go` files that parses as a filter, and every rule of the `tests/data` collections; run against every
  collection with and without request info and with hidden fields allowed. Also covers placeholder params and the limit.
- `tools/search/rule_ast_pg_test.go`: PostgreSQL. `TestRulePostgresGolden` compares ~55 expressions with
  `testdata/rule_pg_golden.txt` (regenerate with `TOKI_UPDATE_GOLDEN=1 go test ./tools/search -run TestRulePostgresGolden`
  and review the diff). `TestRulePostgresStructuralParity` builds the whole hand corpus + collection rules for 3
  collections with and without hidden fields on both dialects and requires: identical parameters and placeholder order,
  no SQLite-only constructs in the PostgreSQL SQL (`IS`/`IS NOT` on values, case sensitive `LIKE`, `json_*`, `strftime`,
  `min(`/`max(`), and every PostgreSQL failure to be `rule.ErrUnsupported`. `kernel/rule/pg/dialect_test.go` pins every
  dialect method.
- CI job `test-rule-ast` runs `./kernel/rule/... ./tools/search/... ./kernel/... ./apis/... ./modules/...` with `TOKI_RULE_AST=1`.

The test seam `security.SeedPseudorandomForTest` makes `PseudorandomString` deterministic; it is for tests only (it panics outside a test binary).

The switch is read once at process start (changing the variable later has no effect). Both paths reject expressions longer than 65536 bytes or nested deeper than 64 groups (`rule.CheckLimits`).
