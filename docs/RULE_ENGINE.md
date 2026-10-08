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
| Functions | `geoDistance(lonA, latA, lonB, latB)`, `strftime(fmt, [time, mods...])`, `@role(name[, scope, "collection"])`, `@member(scope, "collection")` (SQLite only, from `modules/roles`, compare with `= true`), `entitled(key)` (module `payments`, SQLite only, compare with `= true`) | `Call` |
| Comments | `// ...`, `/* ... */` | dropped by the scanner |

Keywords stay identifiers (not literal nodes) because the resolver gets the first chance to
resolve a column named `null`, `true` or `false`; only if resolution fails does the emitter fall back
to `NULL`, `1`, `0`. The `Kind`/`Path`/`Modifier` classification is informational: resolvers always
receive the full identifier string.

Placeholders (`{:name}`) are still substituted textually before parsing, exactly as before.

## `entitled(key)` (payments)

Registered by `modules/payments` in `search.TokenFunctions` (`entitled` and `@entitled`), so both the legacy compiler and the AST path produce the same SQL; PostgreSQL gets the usual "custom function is sqlite only" error. `key` must be a string literal. It resolves `@request.auth.id` and `@request.auth.collectionName` through the normal resolver and emits a scalar `CASE WHEN EXISTS (SELECT 1 FROM _entitlements ...) THEN 1 ELSE 0 END` (status `active|trial|grace`, `until` empty or in the future, subject and collection equal to the request auth), so use it as a comparison: `entitled("pro") = true`. Guests never match. See `docs/modules/payments.md`.

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
- `@role()` and `@member()` (from `modules/roles`) are registered in that map, so both compilers use the same code. They
  expand to an `EXISTS` subquery on `_memberships` (bound parameters only); see `docs/modules/roles.md`. A function cannot
  stand alone in a rule (fexpr needs a comparison), so write `@role("admin") = true`.
- Random placeholder/alias names (`security.PseudorandomString`) are generated while resolving; the order of the
  calls is unchanged (the differential tests compare names too).

## PostgreSQL emitter

```go
r := kernel.NewRecordFieldResolver(app, collection, requestInfo, false)
if err := r.SetDialect(pg.Dialect); err != nil { /* only before the first Resolve */ }
ast, _ := rule.Parse(`@request.auth.id != "" && title ~ "a"`)
expr, err := pg.Emit(ast, r) // then query.AndWhere(expr); r.UpdateQuery(query)
```

SQL examples (rendered with double quotes, `testdata/rule_pg_golden.txt` has about 90):

```
text ~ 'abc'        CAST([[demo1.text]] AS TEXT) ILIKE {:p1} ESCAPE E'\\'         (p1 = "%abc%")
text != 'abc'       [[demo1.text]] IS DISTINCT FROM {:p1}
text = null         (CAST([[demo1.text]] AS TEXT) = '' OR [[demo1.text]] IS NULL)
json.a.b = 'x'      (to_jsonb([[demo1.json]]) #>> '{"a","b"}') IS NOT DISTINCT FROM {:p1}
json.n > 5          (to_jsonb([[demo1.json]]) #> '{"n"}') > to_jsonb(CAST({:p1} AS DOUBLE PRECISION))
json.f = true       (to_jsonb([[demo1.json]]) #> '{"f"}') IS NOT DISTINCT FROM to_jsonb(CAST(TRUE AS BOOLEAN))
@request.body.b = true   (body field b not sent)   CAST(NULL AS BOOLEAN) IS NOT DISTINCT FROM TRUE
number = number     [[demo1.number]] IS NOT DISTINCT FROM [[demo1.number]]
```

Type assumptions (documented in `kernel/rule/pg`):

- json, geoPoint and multi-value fields are `jsonb` (the `json` type is not supported by the row deduplication, see
  "Known limitations"; values go through `to_jsonb`); a multi-value
  field is a JSON array, a single-value field a plain scalar. A `geoPoint` is `{"lon":..,"lat":..}`.
- date fields are text in the PocketBase format or `timestamptz`; time macros and literals are bound as text
  (`2026-10-07 12:34:56.789Z`), which PostgreSQL parses for `timestamptz` and compares lexicographically for text.
- bool is `boolean`, number is `double precision`/`numeric`.
- JSON members are compared as text (`#>>`) with strings and, when the other operand is a number or boolean (literal,
  parameter, or a number/bool column), as `jsonb` (`#>` against `to_jsonb(CAST(.. AS DOUBLE PRECISION|BOOLEAN))`), so
  `json.n > 5` is numeric and `json.f = true` works. Such a comparison on a multi-match path (`@collection.x.json.a = 1`)
  returns `rule.ErrUnsupported`. Path segments are always quoted (`'{"a","null"}'`), a key literally named `null` is a
  string; `'`, `"`, `\`, `{`, `}`, `,`, spaces and control characters are rejected.
- Number, bool and date columns carry their type in the resolver result (`ResolverResult.Type`): two columns of the same
  such type are compared natively (`IS [NOT] DISTINCT FROM`), not through `CAST(.. AS TEXT)`; `NULL` still equals `NULL`
  and differs from every value, like the SQLite `COALESCE(x, '')` semantics.
- A missing boolean/number operand (e.g. an unsent `@request.body.flag`) is compared as a typed `NULL`
  (`CAST(NULL AS BOOLEAN) IS NOT DISTINCT FROM TRUE`) instead of `'' = TRUE`, which PostgreSQL rejects at parse time.
- `:lower` casts its operand to text first (`LOWER(CAST(x AS TEXT))`).
- `geoDistance` evaluates every argument once (derived tables) and casts it with a guarded regex, so a non-numeric JSON
  member yields `NULL` like in SQLite instead of aborting the query.
- `~` escapes `%`, `_` and `\` in the pattern exactly like SQLite and uses `ESCAPE E'\\'` (independent of
  `standard_conforming_strings`); a user pattern that ends with a dangling `\` gets the backslash doubled (PostgreSQL
  would reject it).
- `~` uses `ILIKE`. SQLite's LIKE is case-insensitive for ASCII only, `ILIKE` follows the database collation: non-ASCII
  letters can match differently.
- Non-text operands of `=`/`!=`/`~` are compared through `CAST(.. AS TEXT)` where the SQLite code relies on implicit
  type mixing (the cast defeats an index on that column; plain `col = {:param}` comparisons are not cast).

Unsupported constructs (the emitter returns an error wrapping `rule.ErrUnsupported`, never different SQL):

| Construct | Why |
| --- | --- |
| `field:each` on a multi-value field, multi-value relation hops (`rel_many.title`, `@collection.x.rel_many.y`), `@request.body.field:each` | need a table valued join (`json_each`); PostgreSQL needs `LATERAL jsonb_array_elements_text(..) AS alias(value)`, which the join builder (`registerJoin` / `UpdateQuery` / multi-match `Join`) cannot express yet |
| `strftime(...)` | SQLite format specifiers and modifiers (`start of month`, `+1 day`) have no PostgreSQL equivalent; would need a translation layer |
| custom `search.TokenFunctions` entries | SQLite SQL by definition; the error wraps `rule.ErrUnsupported` (an unknown name stays a plain "unknown function" error) |

Supported but different from SQLite: back-relations through a multi-value relation field use
`id IN (SELECT jsonb_array_elements_text(..))` (`TestRulePostgresMultiBackRelation` builds the join and the multi-match
variant end to end). `geoDistance` propagates NULL
explicitly (`LEAST`/`GREATEST` ignore NULLs, SQLite's `min`/`max` do not).

### Known limitations (documented, not fixed)

| Topic | Behavior |
| --- | --- |
| Row deduplication (`SELECT DISTINCT *`) | `UpdateQuery` still emits `SELECT DISTINCT *` for every join. On PostgreSQL that fails for `json` columns (use `jsonb`) and for `ORDER BY` on a joined field ("ORDER BY expressions must appear in select list"). `DISTINCT ON (id)` cannot be used with an arbitrary sort, so a dialect aware dedup (`id IN (subquery)`) is deferred to the store module. |
| `timestamptz` and the session time zone | A date literal without zone (`created > "2026-01-01 10:00:00"`) is parsed in the session `TimeZone` on `timestamptz` columns, SQLite compares it as UTC text. The store module must `SET TIME ZONE 'UTC'` on every connection. Macros and `@now` carry a `Z`. |
| JSON text rendering | `jsonb::text` / `#>>` of objects and arrays has a space after `:` and `,` (`{"a": 1}`), SQLite renders minified JSON. `json = '{"a":1}'` and `~` on the text of a multi-value field can differ. |
| Numeric parameters | Go numbers/bools are bound to placeholders PostgreSQL may infer as `text` (`CAST({:p} AS TEXT)` in `~`/`geoDistance`/`:lower`). Whether the driver (pgx) encodes them is unverified without an integration run; the store module should bind with `QueryExecModeSimpleProtocol` or cast. |
| JSON numeric segments | `json.a.0` applies index 0 to arrays and key `"0"` to objects on PostgreSQL (negative values index from the end), SQLite `$.a[0]` only matches arrays. |
| NUL byte | PostgreSQL rejects `\u0000` in text parameters, SQLite accepts it. |
| `bool` as text | `CAST(true AS TEXT)` is `true`, SQLite stores `1`: `~` or text equality on a bool column differ. |

There is no PostgreSQL in CI: the emitter is tested by golden SQL and by comparison with the SQLite output (see Testing).
Nothing here selects PostgreSQL at runtime; the store is still SQLite and `pg.Emit` is a building block for a store module.

## Plugging a dialect in (store modules)

1. Implement `rule.Dialect` (all methods, including `JSONExtractTyped`, `JSONScalar`, `NativeCompare`, `EmptyFor`, `NormalizeLikePattern`; a SQLite-like dialect returns the untyped value / `''` / the pattern unchanged; return `rule.ErrUnsupported`-wrapped errors for what you cannot express).
   Start from `kernel/rule/pg` for a server database.
2. Give the record resolver your dialect: `resolver.SetDialect(d)` (before the first `Resolve`; it returns an error afterwards; one resolver per request, it is not goroutine-safe). A resolver whose `Dialect()` returns `nil` is rejected. Resolvers that do not implement `search.DialectResolver` cannot be checked against the emitter dialect.
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
- `tools/search/rule_ast_pg_test.go`: PostgreSQL. `TestRulePostgresGolden` compares ~90 expressions with
  `testdata/rule_pg_golden.txt` (regenerate with `TOKI_UPDATE_GOLDEN=1 go test ./tools/search -run TestRulePostgresGolden`
  and review the diff). `TestRulePostgresStructuralParity` builds the whole hand corpus + collection rules for 3
  collections with and without hidden fields on both dialects and requires: identical parameters and placeholder order,
  no SQLite-only constructs in the PostgreSQL SQL (`IS`/`IS NOT` on values, case sensitive `LIKE`, `json_*`, `strftime`,
  `min(`/`max(`), and every PostgreSQL failure to be `rule.ErrUnsupported`. `kernel/rule/pg/dialect_test.go` pins every
  dialect method. `TestRulePostgresTypedCasts` asserts that the dialect specific typed casts (jsonb comparison, typed
  NULL, native column comparison, `LOWER(CAST(..))`, doubled trailing backslash, guarded geo cast) appear where expected
  and that SQLite output is unchanged.
- CI job `test-rule-ast` runs `./kernel/rule/... ./tools/search/... ./kernel/... ./apis/... ./modules/...` with `TOKI_RULE_AST=1`.

The test seam `security.SeedPseudorandomForTest` makes `PseudorandomString` deterministic; it is for tests only (it panics outside a test binary).

The switch is read once at process start (changing the variable later has no effect). Both paths reject expressions longer than 65536 bytes or nested deeper than 64 groups (`rule.CheckLimits`).
