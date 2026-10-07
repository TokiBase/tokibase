//go:build !no_crypto

package crypto

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
)

func (e *env) list(t *testing.T, who *core.Record, coll, query string) (int, []string) {
	t.Helper()
	code, b := e.do(t, who, "GET", "/api/collections/"+coll+"/records?"+query, "")
	if code != 200 {
		return code, nil
	}
	var ids []string
	for _, it := range b["items"].([]any) {
		ids = append(ids, it.(map[string]any)["id"].(string))
	}
	sort.Strings(ids)
	return code, ids
}

func fq(filter string) string { return "filter=" + url.QueryEscape(filter) }

func (e *env) createSSN(t *testing.T, ssn string) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":"N","diagnosis":"flu","ssn":%q}`, ssn)
	code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", body)
	if code != 200 {
		t.Fatal(code, b)
	}
	return b["id"].(string)
}

func (e *env) wantFilter(t *testing.T, who *core.Record, coll, filter string, want ...string) {
	t.Helper()
	code, got := e.list(t, who, coll, fq(filter))
	sort.Strings(want)
	if code != 200 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s: status %d got %v want %v", filter, code, got, want)
	}
}

func TestFilterEqualityOnBlindIndex(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	a := e.createSSN(t, "123-45")
	b := e.createSSN(t, "999-99")
	for _, who := range []*core.Record{e.su, nil} {
		e.wantFilter(t, who, "patients", `ssn = "123-45"`, a)
		e.wantFilter(t, who, "patients", `"123-45" = ssn`, a)
		e.wantFilter(t, who, "patients", `ssn ?= "999-99"`, b)
		e.wantFilter(t, who, "patients", `ssn != "123-45"`, b)
		e.wantFilter(t, who, "patients", `ssn ?!= "123-45"`, b)
		e.wantFilter(t, who, "patients", `"123-45" != ssn`, b)
		e.wantFilter(t, who, "patients", `ssn = "nope"`)
		e.wantFilter(t, who, "patients", `ssn != "nope"`, a, b)
		e.wantFilter(t, who, "patients", `ssn = "123-45" || ssn = "999-99"`, a, b)
		e.wantFilter(t, who, "patients", `(ssn = "123-45" && name = "N")`, a)
		e.wantFilter(t, who, "patients", `ssn = "123-45" && ssn = "999-99"`)
		// a quote inside the value is data, not syntax
		e.wantFilter(t, who, "patients", `ssn = "12'3"`)
	}
}

func TestFilterEqualityRejectedShapes(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.createSSN(t, "123-45")
	for _, f := range []string{
		`ssn ~ "123"`, `ssn !~ "123"`, `ssn > "1"`, `ssn <= "1"`, `ssn ?~ "1"`,
		`ssn:lower = "123-45"`, `ssn:length = 1`, `ssn = name`, `name = ssn`, `ssn = ssn`,
		`ssn = 1`, `ssn = ""`, `ssn != ""`, `ssn = null`, `ssn = @request.auth.id`,
		`diagnosis = "flu"`, `diagnosis != "x"`, `email = "a@b.c"`,
		`ssn = "123-45" || diagnosis = "flu"`, `(name = "N" && (ssn ~ "1"))`,
		`strftime("%Y", ssn) = "2020"`, `ssn.x = "1"`,
	} {
		code, b := e.do(t, e.su, "GET", "/api/collections/patients/records?"+fq(f), "")
		if code != 400 {
			t.Fatalf("%s: status %d %v", f, code, b)
		}
	}
	for _, q := range []string{"sort=ssn", "sort=-ssn", "sort=name,ssn"} {
		if code, _ := e.do(t, e.su, "GET", "/api/collections/patients/records?"+q, ""); code != 400 {
			t.Fatalf("%s: status %d", q, code)
		}
	}
}

func TestFilterEqualityRelationPaths(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	pa := e.createSSN(t, "123-45")
	pb := e.createSSN(t, "999-99")
	pat, _ := e.app.FindCollectionByNameOrId("patients")
	v := core.NewBaseCollection("visits")
	v.Fields.Add(
		&core.RelationField{Name: "patient", CollectionId: pat.Id, MaxSelect: 1},
		&core.RelationField{Name: "patients", CollectionId: pat.Id, MaxSelect: 5},
	)
	open := ""
	v.ListRule, v.ViewRule, v.CreateRule = &open, &open, &open
	if err := e.app.Save(v); err != nil {
		t.Fatal(err)
	}
	mk := func(body string) string {
		code, b := e.do(t, e.su, "POST", "/api/collections/visits/records", body)
		if code != 200 {
			t.Fatal(code, b)
		}
		return b["id"].(string)
	}
	v1 := mk(fmt.Sprintf(`{"patient":%q,"patients":[%q]}`, pa, pa))
	v2 := mk(fmt.Sprintf(`{"patient":%q,"patients":[%q,%q]}`, pb, pa, pb))

	for _, who := range []*core.Record{e.su, nil} {
		e.wantFilter(t, who, "visits", `patient.ssn = "123-45"`, v1)
		e.wantFilter(t, who, "visits", `patient.ssn != "123-45"`, v2)
		e.wantFilter(t, who, "visits", `patient.ssn = "nope"`)
		// multi relation: ?= is any-match, = needs every related record to match
		e.wantFilter(t, who, "visits", `patients.ssn ?= "999-99"`, v2)
		e.wantFilter(t, who, "visits", `patients.ssn ?= "123-45"`, v1, v2)
		e.wantFilter(t, who, "visits", `patients.ssn = "123-45"`, v1)
		e.wantFilter(t, who, "visits", `patients.ssn ?!= "123-45"`, v2)
		// back relation
		e.wantFilter(t, who, "patients", `visits_via_patient.id = "`+v1+`" && ssn = "123-45"`, pa)
		// @collection join
		e.wantFilter(t, who, "visits", `@collection.patients.ssn = "nope"`)
		e.wantFilter(t, who, "visits", `@collection.patients.ssn = "999-99" && @collection.patients.id = patient`, v2)
	}
	// still rejected through a relation path
	if code, _ := e.do(t, e.su, "GET", "/api/collections/visits/records?"+fq(`patient.ssn ~ "1"`), ""); code != 400 {
		t.Fatalf("relation ~: %d", code)
	}
	if code, _ := e.do(t, e.su, "GET", "/api/collections/visits/records?"+fq(`patient.diagnosis = "flu"`), ""); code != 400 {
		t.Fatalf("relation random: %d", code)
	}
}

func TestFilterEqualityAcrossKeyVersions(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id1 := e.create(t)
	col, _ := e.app.FindCollectionByNameOrId("patients")
	if _, err := e.m.newKey(col.Id); err != nil {
		t.Fatal(err)
	}
	id2 := e.create(t)
	e.wantFilter(t, e.su, "patients", `ssn = "123-45"`, id1, id2)
	if _, _, err := Rotate(e.app, "patients", nil); err != nil {
		t.Fatal(err)
	}
	e.wantFilter(t, e.su, "patients", `ssn = "123-45"`, id1, id2)
	e.wantFilter(t, e.su, "patients", `ssn != "123-45"`)
	// a changed value is found under the new value only
	e.do(t, e.su, "PATCH", "/api/collections/patients/records/"+id1, `{"ssn":"555-55"}`)
	e.wantFilter(t, e.su, "patients", `ssn = "123-45"`, id2)
	e.wantFilter(t, e.su, "patients", `ssn = "555-55"`, id1)
}

// A caller who cannot read the field must not learn its value by filtering.
func TestFilterEqualityIsNotAnOracle(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	a := e.createSSN(t, "123-45")
	b := e.createSSN(t, "999-99")

	// field hidden at enrich time (what fieldperm does) for non superusers
	e.app.OnRecordEnrich().BindFunc(func(ev *core.RecordEnrichEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		if ev.Record.Collection().Name == "patients" && ev.RequestInfo != nil && !ev.RequestInfo.HasSuperuserAuth() {
			ev.Record.Hide("ssn")
		}
		return nil
	})
	e.wantFilter(t, nil, "patients", `ssn = "123-45"`)
	e.wantFilter(t, nil, "patients", `ssn ?= "123-45"`)
	// "!=" must not drop the hidden match either (that would reveal it)
	e.wantFilter(t, nil, "patients", `ssn != "123-45"`, a, b)
	e.wantFilter(t, nil, "patients", `ssn != "nope"`, a, b)
	// superusers are unaffected
	e.wantFilter(t, e.su, "patients", `ssn = "123-45"`, a)
	e.wantFilter(t, e.su, "patients", `ssn != "123-45"`, b)
}

func TestFilterEqualityHiddenField(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	a := e.createSSN(t, "123-45")
	col := patients(t, e)
	col.Fields.GetByName("ssn").SetHidden(true)
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	// hidden fields are filterable only by superusers (same error for any value)
	for _, v := range []string{"123-45", "nope"} {
		if code, _ := e.list(t, nil, "patients", fq(`ssn = "`+v+`"`)); code != 400 {
			t.Fatalf("hidden field must not be filterable by guests: %d", code)
		}
	}
	e.wantFilter(t, e.su, "patients", `ssn = "123-45"`, a)
}

// Collection rules go through the same resolver: a rule with "=" on a
// blind-index field works.
func TestRuleEqualityOnBlindIndex(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	a := e.createSSN(t, "123-45")
	e.createSSN(t, "999-99")
	col := patients(t, e)
	rule := `ssn = "123-45"`
	col.ListRule, col.ViewRule = &rule, &rule
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	for _, who := range []*core.Record{nil, e.usr} {
		code, ids := e.list(t, who, "patients", "")
		if code != 200 || strings.Join(ids, ",") != a {
			t.Fatalf("rule list: %d %v", code, ids)
		}
	}
	if code, _ := e.do(t, nil, "GET", "/api/collections/patients/records/"+a, ""); code != 200 {
		t.Fatalf("rule view: %d", code)
	}
	// rule combined with a client filter
	e.wantFilter(t, nil, "patients", `ssn = "999-99"`)
	e.wantFilter(t, nil, "patients", `ssn = "123-45"`, a)
	// a rule with a request value
	rule2 := `ssn = @request.query.s`
	col.ListRule = &rule2
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	if code, ids := e.list(t, nil, "patients", "s=123-45"); code != 200 || strings.Join(ids, ",") != a {
		t.Fatalf("rule with query value: %d %v", code, ids)
	}
	if _, ids := e.list(t, nil, "patients", "s=zzz"); len(ids) != 0 {
		t.Fatalf("rule with other value: %v", ids)
	}
}
