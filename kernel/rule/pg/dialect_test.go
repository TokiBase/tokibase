package pg_test

import (
	"errors"
	"testing"

	"github.com/tokibase/tokibase/kernel/rule"
	"github.com/tokibase/tokibase/kernel/rule/pg"
)

func TestDialectSQL(t *testing.T) {
	d := pg.Dialect

	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Fatalf("%s:\n got:  %s\n want: %s", name, got, want)
		}
	}

	if d.Name() != "postgres" {
		t.Fatal(d.Name())
	}

	got, err := d.JSONExtract("t.j", nil)
	if err != nil {
		t.Fatal(err)
	}
	check("json top", got, `(to_jsonb([[t.j]]) #>> '{}')`)

	got, _ = d.JSONExtract("t.j", []string{"a", "0", "b"})
	check("json path", got, `(to_jsonb([[t.j]]) #>> '{a,0,b}')`)

	if _, err = d.JSONExtract("t.j", []string{`a"b`}); !errors.Is(err, rule.ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported for a bad path segment, got %v", err)
	}

	got, _ = d.JSONArrayLength("t.c")
	check("array length", got, `(CASE WHEN [[t.c]] IS NULL THEN 0 WHEN jsonb_typeof(to_jsonb([[t.c]])) = 'array' THEN jsonb_array_length(to_jsonb([[t.c]])) WHEN CAST([[t.c]] AS TEXT) = '' THEN 0 ELSE 1 END)`)

	got, _ = d.JSONArrayMember("a.id", "je", "b.c")
	check("array member", got, `[[a.id]] IN (SELECT jsonb_array_elements_text(CASE WHEN jsonb_typeof(to_jsonb([[b.c]])) = 'array' THEN to_jsonb([[b.c]]) ELSE jsonb_build_array([[b.c]]) END))`)

	for k, want := range map[string]string{"null": "NULL", "true": "TRUE", "false": "FALSE"} {
		got, ok := d.Keyword(k)
		if !ok || got != want {
			t.Fatalf("keyword %s: %q %v", k, got, ok)
		}
	}
	if _, ok := d.Keyword("other"); ok {
		t.Fatal("unexpected keyword")
	}

	check("text", d.TextOf("x"), `CAST(x AS TEXT)`)
	check("coalesce", d.CoalesceEmpty("x"), `COALESCE(CAST(x AS TEXT), '')`)
	check("eq", d.NullSafeEq("a", "b", true), `a IS NOT DISTINCT FROM b`)
	check("neq", d.NullSafeEq("a", "b", false), `a IS DISTINCT FROM b`)
	check("like col", d.Like("a", "b", false, true), `CAST(a AS TEXT) ILIKE ('%' || CAST(b AS TEXT) || '%') ESCAPE '\'`)
	check("nlike col", d.Like("a", "b", true, true), `CAST(a AS TEXT) NOT ILIKE ('%' || CAST(b AS TEXT) || '%') ESCAPE '\'`)
	check("like param", d.Like("a", "{:p}", false, false), `CAST(a AS TEXT) ILIKE {:p} ESCAPE '\'`)
	check("optional on", d.OptionalOn(), ` ON TRUE`)
	check("exists", d.ExistsNone("S", "x", "W"), `NOT EXISTS (SELECT 1 FROM (S) {{x}} WHERE W)`)
	check("exists many", d.ExistsNoneMany("L", "l", "R", "r", "W"), `NOT EXISTS (SELECT 1 FROM (L) {{l}} LEFT JOIN (R) {{r}} ON TRUE WHERE W)`)

	got, _ = d.GeoDistance("lonA", "latA", "lonB", "latB")
	check("geo", got, `(CASE WHEN (cos(radians(CAST(latA AS DOUBLE PRECISION))) * cos(radians(CAST(latB AS DOUBLE PRECISION))) * cos(radians(CAST(lonB AS DOUBLE PRECISION)) - radians(CAST(lonA AS DOUBLE PRECISION))) + sin(radians(CAST(latA AS DOUBLE PRECISION))) * sin(radians(CAST(latB AS DOUBLE PRECISION)))) IS NULL THEN NULL ELSE `+
		`6371 * acos(LEAST(1, GREATEST(-1, cos(radians(CAST(latA AS DOUBLE PRECISION))) * cos(radians(CAST(latB AS DOUBLE PRECISION))) * cos(radians(CAST(lonB AS DOUBLE PRECISION)) - radians(CAST(lonA AS DOUBLE PRECISION))) + sin(radians(CAST(latA AS DOUBLE PRECISION))) * sin(radians(CAST(latB AS DOUBLE PRECISION)))))) END)`)
}

func TestDialectUnsupported(t *testing.T) {
	d := pg.Dialect

	if _, err := d.JSONEach("a.b"); !errors.Is(err, rule.ErrUnsupported) {
		t.Fatalf("JSONEach: %v", err)
	}
	if _, err := d.JSONEachParam("p"); !errors.Is(err, rule.ErrUnsupported) {
		t.Fatalf("JSONEachParam: %v", err)
	}
	if _, err := d.Strftime([]string{"'%Y'"}); !errors.Is(err, rule.ErrUnsupported) {
		t.Fatalf("Strftime: %v", err)
	}
}
