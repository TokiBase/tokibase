# Rule engine (phase 2, PR 1)

The filter/rule language (the PocketBase `fexpr` grammar used by list/view/create/update/delete
rules, `?filter=`, `FindRecordsByFilter`, ...) is now split into a parser that produces a
dialect-neutral AST and an emitter that turns the AST into SQL.

```
string --fexpr.Parse--> []fexpr.ExprGroup --rule.Parse--> *rule.AST --Emit--> dbx.Expression
                                                                  \--(later) PostgreSQL emitter
                                                                   \--(later) in-memory evaluator
```

Status: the AST path is **opt-in** (`TOKI_RULE_AST=1`, default off). With the variable unset
production behavior is the legacy fused compiler. COMPAT: no change (SQL and parameters are identical).

## Packages

| Package | Role |
| --- | --- |
| `kernel/rule` | AST types and `Parse(expr) (*AST, error)`. No SQL, no dbx, no store. Depends only on fexpr. |
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

## What is still fused

- Identifier resolution (`FieldResolver.Resolve`, `kernel/record_field_resolver*.go`) emits SQL, registers
  JOINs/aliases on the resolver and builds the multi-match subqueries. The emitter delegates to it unchanged.
- `?`-less multi-match handling (`manyVsOneExpr`, `manyVsManyExpr`), the null-fallback (`COALESCE`) rules and
  `LIKE ... ESCAPE` param wrapping are SQLite SQL text in `tools/search/filter.go`.
- Filter functions (`search.TokenFunctions`) still take `fexpr.Token` arguments; the emitter converts AST
  operands back with `rule.ToToken`.
- Random placeholder/alias names (`security.PseudorandomString`) are generated while resolving.

A PostgreSQL emitter needs the resolver split first (resolution into a typed reference, emission separate).

## How to add an emitter

1. Consume `*rule.AST` only (`rule.Parse` once, cache by source string).
2. Walk `Group`/`Item`/`Comparison`; honor `Item.Join` (left to right) and keep nesting for parentheses.
3. For every `Ident`, delegate to a resolver for your dialect; apply the same null-fallback semantics for `=`/`!=`
   and the `LIKE` wrapping/escaping for `~`/`!~` (see `resolveEqualExpr`, `wrapLikeParams`).
4. Enforce the max comparisons limit (one per `Comparison`, error `search.ErrFilterExprLimit`).
5. Add the emitter to the differential harness (`tools/search/rule_ast_diff_test.go`) against the SQLite output
   where semantics are shared.

## Testing

- `kernel/rule/parse_test.go`: errors identical to fexpr, structure, identifier classification, token round trip.
- `tools/search/rule_ast_diff_test.go`: builds every expression with the legacy path and the AST path and compares
  the full query SQL and parameters byte-for-byte (the pseudorandom source and the clock are pinned), or the error text
  when building fails. Corpus: hand written expressions, an operator x operand matrix, every string literal in
  `*_test.go` files that parses as a filter, and every rule of the `tests/data` collections; run against every
  collection with and without request info and with hidden fields allowed. Also covers placeholder params and the limit.
- CI job `test-rule-ast` runs `./kernel/rule/... ./tools/search/... ./kernel/... ./apis/...` with `TOKI_RULE_AST=1`.

The test seam `security.SeedPseudorandomForTest` makes `PseudorandomString` deterministic; it is for tests only.
