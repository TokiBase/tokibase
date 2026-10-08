//go:build !no_crypto

package crypto

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/search"
)

// countingProvider wraps the real provider and counts lookups.
type countingProvider struct {
	kernel.BlindIndexProvider
	calls *atomic.Int32
}

func (c countingProvider) BlindIndexIDs(col *core.Collection, field, value string, info *core.RequestInfo, enforce bool) ([]string, error) {
	c.calls.Add(1)
	return c.BlindIndexProvider.BlindIndexIDs(col, field, value, info, enforce)
}

func (e *env) countLookups() *atomic.Int32 {
	n := &atomic.Int32{}
	kernel.SetBlindIndexProvider(e.app, countingProvider{indexProvider{e.m}, n})
	return n
}

func (e *env) seedIndexRows(t *testing.T, col *core.Collection, field, value string, n int) {
	t.Helper()
	hs, err := e.m.blindHMACs(col, field, value)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		_, err := e.app.DB().NewQuery("INSERT INTO {{" + IndexTable + "}} (collection, field, record, hmac, ver) VALUES ({:c},{:f},{:r},{:h},1)").
			Bind(map[string]any{"c": col.Id, "f": field, "r": fmt.Sprintf("%.4s%011d", value, i), "h": hs[0]}).Execute()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func buildExpr(e *env, info *core.RequestInfo, allowHidden bool, filter string) error {
	col, _ := e.app.FindCollectionByNameOrId("patients")
	r := core.NewRecordFieldResolver(e.app, col, info, allowHidden)
	_, err := search.FilterData(filter).BuildExpr(r)
	return err
}

func isShapeErr(err error) bool {
	var ef *kernel.EncryptedFieldError
	return errors.As(err, &ef)
}

// C1: HEAD takes the same guard as GET.
func TestQCHeadIsGuarded(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.createSSN(t, "123-45")
	for _, f := range []string{`ssn ~ "1"`, `diagnosis = "flu"`, `ssn = name`} {
		if code, _ := e.do(t, e.su, "HEAD", "/api/collections/patients/records?"+fq(f), ""); code != 400 {
			t.Fatalf("HEAD %s: %d", f, code)
		}
	}
	if code, _ := e.do(t, e.su, "HEAD", "/api/collections/patients/records?"+fq(`ssn = "123-45"`), ""); code != 200 {
		t.Fatalf("HEAD supported shape: %d", code)
	}
}

// C1/C2: the shape rule lives in the resolver hook, so resolvers built the way
// geo (rule resolver then SetAllowHiddenFields), realtime (allowHidden=false)
// and MCP (same as geo) build them reject unsupported shapes, without any
// HTTP middleware.
func TestQCResolverRejectsShapesOnEveryPath(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	col, _ := e.app.FindCollectionByNameOrId("patients")
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault}
	bad := []string{`ssn ~ "x"`, `ssn > "a"`, `ssn:lower = "a"`, `ssn = name`, `ssn = ""`, `ssn = null`, `diagnosis = "flu"`, `diagnosis ~ "f"`}
	for _, f := range bad {
		// realtime: constructed with allowHiddenFields=false
		if err := buildExpr(e, info, false, f); !isShapeErr(err) {
			t.Errorf("realtime %s: %v", f, err)
		}
		// geo / MCP / list: built with true, then SetAllowHiddenFields
		r := core.NewRecordFieldResolver(e.app, col, info, true)
		r.SetAllowHiddenFields(true)
		if _, err := search.FilterData(f).BuildExpr(r); !isShapeErr(err) {
			t.Errorf("geo/mcp %s: %v", f, err)
		}
		// a collection rule (resolver never switched to client mode) is lenient
		r = core.NewRecordFieldResolver(e.app, col, info, true)
		if _, err := search.FilterData(f).BuildExpr(r); isShapeErr(err) {
			t.Errorf("rule %s must stay lenient: %v", f, err)
		}
	}
	if err := buildExpr(e, info, false, `ssn = "a" || ssn != "b"`); err != nil {
		t.Fatal(err)
	}
}

// C7: while a blind-index field is being enabled/disabled it is not queryable
// by equality; "!=" cannot fail open for a record that is not indexed yet.
func TestQCNonActiveStateNotQueryable(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	id := e.createSSN(t, "BLOCKED")
	col := patients(t, e)
	// the record is not indexed (as during `enabling`)
	if _, err := e.app.DB().NewQuery("DELETE FROM {{" + IndexTable + "}} WHERE record={:r}").Bind(map[string]any{"r": id}).Execute(); err != nil {
		t.Fatal(err)
	}
	rec, err := e.m.configRecord(col.Id, "ssn")
	if err != nil {
		rec, err = e.m.configRecord(col.Name, "ssn")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{StateEnabling, StateDisabling} {
		rec.Set("state", st)
		if err := e.app.Save(rec); err != nil {
			t.Fatal(err)
		}
		e.m.Invalidate()
		for _, who := range []*core.Record{e.su, nil} {
			for _, f := range []string{`ssn = "BLOCKED"`, `ssn != "BLOCKED"`, `ssn ?!= "BLOCKED"`} {
				if code, _ := e.list(t, who, "patients", fq(f)); code != 400 {
					t.Fatalf("%s %s: %d", st, f, code)
				}
			}
		}
		// a deny style rule fails closed (error) instead of letting the record in
		deny := `ssn != "BLOCKED"`
		col.ListRule = &deny
		if err := e.app.Save(col); err != nil {
			t.Fatal(err)
		}
		if code, ids := e.list(t, nil, "patients", ""); code == 200 {
			t.Fatalf("%s deny rule failed open: %v", st, ids)
		}
		open := ""
		col.ListRule = &open
		if err := e.app.Save(col); err != nil {
			t.Fatal(err)
		}
	}
	rec.Set("state", "")
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	e.m.Invalidate()
	e.wantFilter(t, e.su, "patients", `ssn != "BLOCKED"`, id) // not indexed => reads as different
}

// C3/C4: memoization per resolver and a cap on distinct lookups.
func TestQCLookupMemoAndCap(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.createSSN(t, "v0")
	n := e.countLookups()
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault}

	same := strings.TrimSuffix(strings.Repeat(`ssn = "v0" || `, 30), " || ")
	if err := buildExpr(e, info, false, same); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("30 identical comparisons must do 1 lookup, got %d", n.Load())
	}

	mk := func(c int) string {
		parts := make([]string, c)
		for i := range parts {
			parts[i] = fmt.Sprintf(`ssn = "v%d"`, i)
		}
		return strings.Join(parts, " || ")
	}
	n.Store(0)
	if err := buildExpr(e, info, false, mk(kernel.MaxBlindIndexLookups)); err != nil {
		t.Fatal(err)
	}
	n.Store(0)
	err := buildExpr(e, info, false, mk(kernel.MaxBlindIndexLookups+1))
	if !isShapeErr(err) || !strings.Contains(err.Error(), "too many blind-index comparisons") {
		t.Fatalf("cap: %v", err)
	}
	if n.Load() != int32(kernel.MaxBlindIndexLookups) {
		t.Fatalf("lookups before the cap: %d", n.Load())
	}
	// over HTTP: a clear 400
	if code, _ := e.list(t, e.su, "patients", fq(mk(kernel.MaxBlindIndexLookups+1))); code != 400 {
		t.Fatalf("http cap: %d", code)
	}
}

// C3/C6: the cap on matches is checked on index rows before anything is loaded
// or decrypted, and the message does not reveal the cardinality.
func TestQCMatchCapGenericAndEarly(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	col := patients(t, e)
	e.seedIndexRows(t, col, "ssn", "popular", maxFilterMatches+1)
	for _, who := range []*core.Record{e.su, nil} {
		code, b := e.do(t, who, "GET", "/api/collections/patients/records?"+fq(`ssn = "popular"`), "")
		if code != 400 {
			t.Fatalf("status %d", code)
		}
		msg := fmt.Sprint(b)
		if strings.Contains(msg, "1000") || strings.Contains(msg, "1001") || strings.Contains(msg, "more than") {
			t.Fatalf("message reveals the cardinality: %s", msg)
		}
	}
	// exactly the cap is fine (the rows point to no record, so nothing matches)
	e.seedIndexRows(t, col, "ssn", "limit", maxFilterMatches)
	e.wantFilter(t, e.su, "patients", `ssn = "limit"`)
	if _, err := FindByBlindIndex(e.app, "patients", "ssn", "popular"); !errors.Is(err, errTooManyMatches) {
		t.Fatalf("FindByBlindIndex: %v", err)
	}
	if code, _ := e.do(t, e.su, "POST", "/api/crypto/lookup/patients/ssn", `{"value":"popular"}`); code != 400 {
		t.Fatalf("lookup endpoint: %d", code)
	}
}

// C9: saving a collection does not call the provider, even without a key.
func TestQCCollectionSaveSkipsProvider(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	col := patients(t, e)
	e.seedIndexRows(t, col, "ssn", "popular", maxFilterMatches+1)
	n := e.countLookups()
	e.m.master = nil // no master key loaded
	rule := `ssn = "popular" || ssn != "popular"`
	col.ListRule, col.ViewRule = &rule, &rule
	if err := e.app.Save(col); err != nil {
		t.Fatalf("save: %v", err)
	}
	if n.Load() != 0 {
		t.Fatalf("provider called %d times during validation", n.Load())
	}
}

type failingProvider struct{ kernel.BlindIndexProvider }

func (failingProvider) IsEncrypted(string, string) (bool, error) {
	return false, errors.New("configuration unavailable")
}

// C10: an unavailable configuration fails closed.
func TestQCConfigErrorFailsClosed(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	kernel.SetBlindIndexProvider(e.app, failingProvider{indexProvider{e.m}})
	info := &core.RequestInfo{Context: core.RequestInfoContextDefault}
	if err := buildExpr(e, info, false, `ssn != "x"`); !isShapeErr(err) {
		t.Fatalf("got %v", err)
	}
	// rules too (lenient shapes are unaffected, but the comparison is refused)
	if err := buildExpr(e, info, true, `ssn != "x"`); !isShapeErr(err) {
		t.Fatalf("rule: %v", err)
	}
}

// C11: no RequestInfo still gates visibility.
func TestQCNilInfoStillEnforces(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	e.createSSN(t, "123-45")
	e.app.OnRecordEnrich().BindFunc(func(ev *core.RecordEnrichEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		ev.Record.Hide("ssn")
		return nil
	})
	col := patients(t, e)
	r := core.NewRecordFieldResolver(e.app, col, nil, false)
	expr, err := search.FilterData(`ssn = "123-45"`).BuildExpr(r)
	if err != nil {
		t.Fatal(err)
	}
	q := e.app.RecordQuery(col).AndWhere(expr)
	if err := r.UpdateQuery(q); err != nil {
		t.Fatal(err)
	}
	var recs []*core.Record
	if err := q.All(&recs); err != nil || len(recs) != 0 {
		t.Fatalf("hidden record matched: %v %v", len(recs), err)
	}
}

// C16.1/2: one record visible, one hidden, same value.
func TestQCPartialVisibility(t *testing.T) {
	e := setup(t, "")
	e.enableAll(t)
	mk := func(name string) string {
		code, b := e.do(t, e.su, "POST", "/api/collections/patients/records", fmt.Sprintf(`{"name":%q,"ssn":"same"}`, name))
		if code != 200 {
			t.Fatal(code, b)
		}
		return b["id"].(string)
	}
	vis, hid := mk("V"), mk("H")
	e.app.OnRecordEnrich().BindFunc(func(ev *core.RecordEnrichEvent) error {
		if err := ev.Next(); err != nil {
			return err
		}
		if ev.Record.Collection().Name == "patients" && ev.Record.GetString("name") == "H" &&
			ev.RequestInfo != nil && !ev.RequestInfo.HasSuperuserAuth() {
			ev.Record.Hide("ssn")
		}
		return nil
	})
	e.wantFilter(t, nil, "patients", `ssn = "same"`, vis)
	e.wantFilter(t, nil, "patients", `ssn != "same"`, hid)
	e.wantFilter(t, nil, "patients", `ssn = "same" || ssn != "same"`, vis, hid)
	e.wantFilter(t, nil, "patients", `ssn = "same" && ssn != "same"`)
	e.wantFilter(t, e.su, "patients", `ssn = "same"`, vis, hid)
}

// C8: an encrypted field of the auth record is never rewritten to ciphertext.
func TestQCRequestAuthEncryptedField(t *testing.T) {
	e := setup(t, "")
	mem := core.NewAuthCollection("members")
	mem.Fields.Add(&core.TextField{Name: "ssn"})
	if err := e.app.Save(mem); err != nil {
		t.Fatal(err)
	}
	e.enableAll(t)
	if _, err := Enable(e.app, "members", "ssn", ModeBlindIndex, nil); err != nil {
		t.Fatal(err)
	}
	m := core.NewRecord(mem)
	m.SetEmail("m@example.com")
	m.SetPassword("1234567890")
	m.Set("ssn", "123-45")
	if err := e.app.Save(m); err != nil {
		t.Fatal(err)
	}
	mine := e.createSSN(t, "123-45")
	other := e.createSSN(t, "999-99")
	col := patients(t, e)

	eq, neq := `ssn = @request.auth.ssn`, `ssn != @request.auth.ssn`
	col.ListRule = &eq
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	if code, ids := e.list(t, m, "patients", ""); code == 200 && len(ids) > 0 && strings.Join(ids, ",") != mine {
		t.Fatalf("= matched %v", ids)
	}
	col.ListRule = &neq
	if err := e.app.Save(col); err != nil {
		t.Fatal(err)
	}
	if code, ids := e.list(t, m, "patients", ""); code == 200 {
		for _, id := range ids {
			if id == mine {
				t.Fatalf("!= let the owner's own record in: %v (other=%s)", ids, other)
			}
		}
	}
}
