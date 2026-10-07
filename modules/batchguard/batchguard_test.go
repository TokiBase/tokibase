//go:build !no_batchguard

package batchguard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/hook"
)

type env struct {
	app *tests.TestApp
	m   *Module
	mux http.Handler
	su  *core.Record
}

func setup(t *testing.T) *env {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	m := Register(app)
	if err := EnsureCollection(app); err != nil {
		t.Fatal(err)
	}

	open := ""
	mk := func(name string, fields ...core.Field) {
		c := core.NewBaseCollection(name)
		c.Fields.Add(fields...)
		c.ListRule, c.ViewRule, c.CreateRule, c.UpdateRule, c.DeleteRule = &open, &open, &open, &open, &open
		if err := app.Save(c); err != nil {
			t.Fatal(err)
		}
	}
	mk("orders", &core.NumberField{Name: "total_qty"})
	mk("order_items", &core.TextField{Name: "order"}, &core.TextField{Name: "product"}, &core.NumberField{Name: "qty"})
	mk("products", &core.NumberField{Name: "stock"})
	mk("wallets", &core.NumberField{Name: "balance"})

	app.Settings().Batch.Enabled = true
	app.Settings().Batch.MaxRequests = 50
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	if err := app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		var err error
		h, err = se.Router.BuildMux()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	su, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return &env{app: app, m: m, mux: h, su: su}
}

func (e *env) batch(t *testing.T, reqs ...map[string]any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"requests": reqs})
	req := httptest.NewRequest("POST", "/api/batch", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	tok, _ := e.su.NewAuthToken()
	req.Header.Set("Authorization", tok)
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out == nil { // success is an array
		var arr []any
		_ = json.Unmarshal(rec.Body.Bytes(), &arr)
		out = map[string]any{"results": arr}
	}
	return rec.Code, out
}

func post(coll string, body map[string]any) map[string]any {
	return map[string]any{"method": "POST", "url": "/api/collections/" + coll + "/records", "body": body}
}

func patch(coll, id string, body map[string]any) map[string]any {
	return map[string]any{"method": "PATCH", "url": "/api/collections/" + coll + "/records/" + id, "body": body}
}

func (e *env) count(t *testing.T, coll string) int {
	t.Helper()
	n, err := e.app.CountRecords(coll)
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func (e *env) rule(t *testing.T, r Rule) {
	t.Helper()
	r.Enabled = true
	if _, err := Save(e.app, r); err != nil {
		t.Fatal(err)
	}
}

func (e *env) seed(t *testing.T, coll, field string, v float64) string {
	t.Helper()
	c, _ := e.app.FindCollectionByNameOrId(coll)
	r := core.NewRecord(c)
	r.Set(field, v)
	if err := e.app.Save(r); err != nil {
		t.Fatal(err)
	}
	return r.Id
}

func (e *env) num(t *testing.T, coll, id, field string) float64 {
	t.Helper()
	r, err := e.app.FindRecordById(coll, id)
	if err != nil {
		t.Fatal(err)
	}
	return r.GetFloat(field)
}

func batchErr(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, _ := out["data"].(map[string]any)
	b, _ := d["batch"].(map[string]any)
	if b == nil {
		t.Fatalf("no data.batch in %v", out)
	}
	return b
}

var checkout = Rule{
	Name:    "checkout",
	Match:   []Match{{"orders", "POST"}, {"order_items", "POST"}},
	Assert:  "sum(order_items, qty) == req(0).body.total_qty",
	Message: "Order total does not match its items",
}

func TestRejectsAndPasses(t *testing.T) {
	e := setup(t)
	e.rule(t, checkout)

	code, out := e.batch(t,
		post("orders", map[string]any{"total_qty": 5}),
		post("order_items", map[string]any{"qty": 2}),
		post("order_items", map[string]any{"qty": 2}))
	if code != 400 || out["message"] != "Batch rejected." {
		t.Fatalf("want 400, got %d %v", code, out)
	}
	b := batchErr(t, out)
	if b["code"] != "validation_batch_rule" || b["message"] != "Order total does not match its items" || b["rule"] != "checkout" {
		t.Fatalf("bad error: %v", b)
	}
	if e.count(t, "orders") != 0 || e.count(t, "order_items") != 0 {
		t.Fatal("nothing must be written")
	}

	code, out = e.batch(t,
		post("orders", map[string]any{"total_qty": 5}),
		post("order_items", map[string]any{"qty": 2}),
		post("order_items", map[string]any{"qty": 3}))
	if code != 200 {
		t.Fatalf("want 200, got %d %v", code, out)
	}
	if res, _ := out["results"].([]any); len(res) != 3 {
		t.Fatalf("results: %v", out)
	}
	if e.count(t, "orders") != 1 || e.count(t, "order_items") != 2 {
		t.Fatal("rows missing")
	}
}

func TestPostPhaseRollsBack(t *testing.T) {
	e := setup(t)
	a := e.seed(t, "wallets", "balance", 100)
	b := e.seed(t, "wallets", "balance", 0)
	e.rule(t, Rule{
		Name:       "non-negative",
		Match:      []Match{{"wallets", "PATCH"}},
		AssertPost: "all(wallets, balance, '>=', 0)",
		Message:    "Balance would become negative",
	})

	// transfer 150 from a (100): a goes to -50, b to 150; the orders row must roll back too
	code, out := e.batch(t,
		post("orders", map[string]any{"total_qty": 1}),
		patch("wallets", a, map[string]any{"balance": -50}),
		patch("wallets", b, map[string]any{"balance": 150}))
	if code != 400 || batchErr(t, out)["rule"] != "non-negative" {
		t.Fatalf("want rejection, got %d %v", code, out)
	}
	if e.count(t, "orders") != 0 || e.num(t, "wallets", a, "balance") != 100 || e.num(t, "wallets", b, "balance") != 0 {
		t.Fatal("batch was not rolled back")
	}

	code, out = e.batch(t,
		patch("wallets", a, map[string]any{"balance": 40}),
		patch("wallets", b, map[string]any{"balance": 60}))
	if code != 200 {
		t.Fatalf("valid transfer: %d %v", code, out)
	}
	if e.num(t, "wallets", a, "balance") != 40 || e.num(t, "wallets", b, "balance") != 60 {
		t.Fatal("transfer not applied")
	}
}

func TestStockReadsCurrentValuesInsideTx(t *testing.T) {
	e := setup(t)
	p := e.seed(t, "products", "stock", 3)
	e.rule(t, Rule{
		Name:    "pre",
		Match:   []Match{{"order_items", "POST"}},
		Assert:  "stock(order_items, product, qty, products, stock) >= 0",
		Message: "Not enough stock",
	})
	item := func(q float64) map[string]any { return post("order_items", map[string]any{"product": p, "qty": q}) }

	if code, out := e.batch(t, item(5)); code != 400 || batchErr(t, out)["message"] != "Not enough stock" {
		t.Fatalf("5 > 3 must be rejected: %d %v", code, out)
	}
	if code, out := e.batch(t, item(2), item(1)); code != 200 {
		t.Fatalf("3 <= 3 must pass: %d %v", code, out)
	}

	// post phase: the PATCH in the same batch lowers the stock; the rule must see it (in-tx read)
	e.rule(t, Rule{
		Name:       "post",
		Match:      []Match{{"order_items", "POST"}, {"products", "PATCH"}},
		AssertPost: "stock(order_items, product, qty, products, stock) >= 0",
	})
	code, out := e.batch(t, patch("products", p, map[string]any{"stock": 1}), item(2))
	if code != 400 || batchErr(t, out)["rule"] != "post" {
		t.Fatalf("want post rejection, got %d %v", code, out)
	}
	if e.num(t, "products", p, "stock") != 3 {
		t.Fatal("stock must be unchanged after rollback")
	}
	if code, out = e.batch(t, patch("products", p, map[string]any{"stock": 9}), item(2)); code != 200 {
		t.Fatalf("want 200, got %d %v", code, out)
	}
}

func TestInvalidExpressionRefusedAtSave(t *testing.T) {
	e := setup(t)
	for _, src := range []string{
		"sum(order_items",                // unbalanced
		"nope(1) == 1",                   // unknown function
		"count() == 1",                   // arity
		"1 +",                            // dangling operator
		"@request.auth == 'x'",           // bad variable
		"req(0) == 1",                    // req needs a path
		"1 == 1 && $",                    // bad character
		strings.Repeat("1+", 1100) + "1", // too long
	} {
		if _, err := Save(e.app, Rule{Name: "bad", Enabled: true, Match: []Match{{"orders", ""}}, Assert: src}); err == nil {
			t.Errorf("%.30q must be refused", src)
		}
	}
	// the API path (a plain record save) is validated too
	col, _ := e.app.FindCollectionByNameOrId(CollectionName)
	rec := core.NewRecord(col)
	rec.Set("name", "bad2")
	rec.Set("assert", "((")
	rec.Set("match", []Match{{"orders", ""}})
	if err := e.app.Save(rec); err == nil {
		t.Fatal("record save must be refused")
	}
	if n := e.count(t, CollectionName); n != 0 {
		t.Fatalf("no rule should exist, got %d", n)
	}
}

func TestTimeoutFailsClosed(t *testing.T) {
	e := setup(t)
	e.m.timeout = 1 // 1ns: expired before the first node
	e.rule(t, Rule{Name: "slow", Match: []Match{{"orders", "POST"}}, Assert: "1 + 1 == 2 && count(orders) == 1"})
	code, out := e.batch(t, post("orders", map[string]any{"total_qty": 1}))
	if code != 400 || !strings.Contains(batchErr(t, out)["message"].(string), "timed out") {
		t.Fatalf("want timeout rejection, got %d %v", code, out)
	}
	if e.count(t, "orders") != 0 {
		t.Fatal("must not write")
	}
}

func TestRuleWithoutMatchDoesNotApply(t *testing.T) {
	e := setup(t)
	e.rule(t, Rule{Name: "never", Match: nil, Assert: "false"})
	e.rule(t, Rule{Name: "other", Match: []Match{{"wallets", ""}}, Assert: "false"})
	e.rule(t, Rule{Name: "needs-both", Match: []Match{{"orders", "POST"}, {"order_items", "POST"}}, Assert: "false"})
	if code, out := e.batch(t, post("orders", map[string]any{"total_qty": 1})); code != 200 {
		t.Fatalf("no rule applies: %d %v", code, out)
	}
	// disabled rules never apply
	e.rule(t, Rule{Name: "off", Match: []Match{{"orders", ""}}, Assert: "false"})
	rec, _ := e.app.FindFirstRecordByData(CollectionName, "name", "off")
	rec.Set("enabled", false)
	if err := e.app.Save(rec); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.batch(t, post("orders", map[string]any{"total_qty": 1})); code != 200 {
		t.Fatal("disabled rule applied")
	}
}

func TestKernelHooks(t *testing.T) {
	e := setup(t)
	var seen []string
	var after kernel.BatchEvent
	id := kernel.OnBatchFor(e.app).Bind(&hook.Handler[*kernel.BatchEvent]{Func: func(ev *kernel.BatchEvent) error {
		seen = append(seen, ev.Name)
		if ev.Name == kernel.BatchAfter && after.Name == "" {
			after = *ev
			if ev.Requests[0].Body["total_qty"] != float64(7) || ev.Requests[0].ID == "" {
				t.Errorf("after event must carry stored records: %+v", ev.Requests)
			}
			if ev.Auth == nil || ev.App == nil {
				t.Error("auth/app missing")
			}
		}
		return ev.Next()
	}})
	defer kernel.OnBatchFor(e.app).Unbind(id)

	if code, out := e.batch(t, post("orders", map[string]any{"total_qty": 7})); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if strings.Join(seen, ",") != "batch.before,batch.after" || after.Name == "" {
		t.Fatalf("events: %v", seen)
	}

	// an error from batch.after rolls the whole batch back
	kernel.OnBatchFor(e.app).Bind(&hook.Handler[*kernel.BatchEvent]{Id: "boom", Priority: 10, Func: func(ev *kernel.BatchEvent) error {
		if ev.Name == kernel.BatchAfter {
			return errors.New("nope")
		}
		return ev.Next()
	}})
	defer kernel.OnBatchFor(e.app).Unbind("boom")
	before := e.count(t, "orders")
	if code, _ := e.batch(t, post("orders", map[string]any{"total_qty": 1})); code != 400 {
		t.Fatalf("want 400, got %d", code)
	}
	if e.count(t, "orders") != before {
		t.Fatal("not rolled back")
	}
}

func TestExpressionLanguage(t *testing.T) {
	reqs := []reqView{
		{Index: 0, Collection: "a", Method: "POST", Data: map[string]any{"n": 2.0, "s": "x", "str": "4"}},
		{Index: 1, Collection: "b", Method: "POST", Data: map[string]any{"n": 3.0}},
		{Index: 2, Collection: "b", Method: "DELETE", ID: "z", Deleted: true},
	}
	cases := map[string]bool{
		"1 + 2 * 3 == 7":                                true,
		"(1 + 2) * 3 == 9":                              true,
		"10 / 4 == 2.5":                                 true,
		"-1 < 0 && !(1 > 2)":                            true,
		"false || true":                                 true,
		"count(b) == 2 && count(b, 'POST') == 1":        true,
		"sum(b, n) == 3":                                true,
		"sum(a, str) == 4":                              true,
		"all(b, n, '>', 2)":                             true,
		"all(b, n, '>', 3)":                             false,
		"all(b, missing, '==', 1)":                      false,
		"exists(a, s, 'x')":                             true,
		"exists(a, s, 'y')":                             false,
		"req(1).body.n == 3":                            true,
		"req(2).method == 'DELETE' && req(2).id == 'z'": true,
		"req(0).body.nope == null":                      true,
		"@request.auth.id == ''":                        true,
		"'a' < 'b'":                                     true,
		"1 == 1 || 1/0 == 1":                            true, // short circuit
	}
	for src, want := range cases {
		n, err := Parse(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		got, err := evalBool(n, &evalCtx{reqs: reqs})
		if err != nil || got != want {
			t.Errorf("%s = %v, %v; want %v", src, got, err, want)
		}
	}
	for _, src := range []string{"1/0 == 1", "req(9).body.n == 1", "1 + 'a' == 1", "1", "!1"} {
		n, err := Parse(src)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if _, err := evalBool(n, &evalCtx{reqs: reqs}); err == nil {
			t.Errorf("%s must fail at eval", src)
		}
	}
}

func postURL(url string, body map[string]any) map[string]any {
	return map[string]any{"method": "POST", "url": url, "body": body}
}

// B1: `?fields=` must not hide the record id from assert_post.
func TestAssertPostNotEvadedByFields(t *testing.T) {
	e := setup(t)
	e.rule(t, Rule{
		Name:       "cap",
		Match:      []Match{{"order_items", "POST"}},
		AssertPost: "sum(order_items, qty) <= 10",
	})
	url := "/api/collections/order_items/records?fields=product"
	code, out := e.batch(t,
		postURL(url, map[string]any{"qty": 1000}),
		postURL(url, map[string]any{"qty": 1000}))
	if code != 400 || batchErr(t, out)["rule"] != "cap" {
		t.Fatalf("want rejection, got %d %v", code, out)
	}
	if e.count(t, "order_items") != 0 {
		t.Fatal("must roll back")
	}
	if code, out := e.batch(t, postURL(url, map[string]any{"qty": 3})); code != 200 {
		t.Fatalf("valid batch: %d %v", code, out)
	}
}

// B2: modifier keys for fields read by `assert` are refused.
func TestModifierKeysRefused(t *testing.T) {
	e := setup(t)
	e.rule(t, Rule{
		Name:   "cap",
		Match:  []Match{{"order_items", "POST"}},
		Assert: "sum(order_items, qty) <= 10",
	})
	code, out := e.batch(t, post("order_items", map[string]any{"qty+": 9999}))
	if code != 400 || !strings.Contains(batchErr(t, out)["message"].(string), "modifier key") {
		t.Fatalf("want modifier refusal, got %d %v", code, out)
	}
	if e.count(t, "order_items") != 0 {
		t.Fatal("must not write")
	}
	// unreferenced fields may use modifiers
	if code, out := e.batch(t, post("order_items", map[string]any{"qty": 1, "product+": "x"})); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	for k, want := range map[string]string{"qty+": "qty", "+tags": "tags", "qty-": "qty", "slug:autogenerate": "slug", "qty": "qty"} {
		if got := modifierBase(k); got != want {
			t.Errorf("modifierBase(%q)=%q want %q", k, got, want)
		}
	}
}

// B3: a failing rule load refuses the batch.
func TestRuleLoadFailsClosed(t *testing.T) {
	e := setup(t)
	e.m.listRules = func(core.App) ([]Rule, error) { return nil, errors.New("database is locked") }
	code, out := e.batch(t, post("orders", map[string]any{"total_qty": 1}))
	if code != 500 {
		t.Fatalf("want 500, got %d %v", code, out)
	}
	if e.count(t, "orders") != 0 {
		t.Fatal("must not write")
	}
}

// B4: DELETE then PUT of the same id is a create at execution time.
func TestPutClassifiedByReplay(t *testing.T) {
	e := setup(t)
	id := e.seed(t, "orders", "total_qty", 1)
	reqs := parseRequests(e.app, []*core.InternalRequest{
		{Method: "PUT", URL: "/api/collections/orders/records", Body: map[string]any{"id": id}},
		{Method: "DELETE", URL: "/api/collections/orders/records/" + id},
		{Method: "PUT", URL: "/api/collections/orders/records", Body: map[string]any{"id": id}},
		{Method: "PUT", URL: "/api/collections/orders/records", Body: map[string]any{"id": id}},
		{Method: "PUT", URL: "/api/collections/orders/records", Body: map[string]any{"id": 12345}},
	})
	want := []string{"PATCH", "DELETE", "POST", "PATCH", "POST"}
	for i, w := range want {
		if reqs[i].Method != w {
			t.Errorf("req %d: got %s want %s", i, reqs[i].Method, w)
		}
	}
}

// B5: numeric strings compare numerically only for number-typed fields.
func TestStringCompare(t *testing.T) {
	cases := []struct {
		l, r    any
		numeric bool
		want    bool
	}{
		{"10", "5", true, false},  // number field: 10 <= 5 false
		{"10", "5", false, true},  // plain strings: lexical
		{"10", 5.0, false, false}, // number on one side: numeric
	}
	for _, c := range cases {
		got, err := compare(c.l, "<=", c.r, c.numeric)
		if err != nil || got != c.want {
			t.Errorf("%v <= %v (numeric=%v) = %v, %v; want %v", c.l, c.r, c.numeric, got, err, c.want)
		}
	}
}

// B7, B8: NaN/Inf and huge req() indexes fail instead of passing or panicking.
func TestExprEdgeCases(t *testing.T) {
	reqs := []reqView{
		{Index: 0, Collection: "a", Method: "POST", Data: map[string]any{"x": 1e308, "i": 1e30}},
		{Index: 1, Collection: "a", Method: "POST", Data: map[string]any{"x": 1e308}},
	}
	for _, src := range []string{
		"sum(a, x) - sum(a, x) >= 0",
		"req(0).body.x * 10 >= 0",
		"req(req(0).body.i).body.x == 1",
	} {
		n, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if _, err := evalBool(n, &evalCtx{reqs: reqs}); err == nil {
			t.Errorf("%s must error", src)
		}
	}
}

// B6: handlers bound for one app do not see another app's batches.
func TestBatchHooksPerApp(t *testing.T) {
	e := setup(t)
	other := setup(t)
	called := 0
	kernel.OnBatchFor(other.app).Bind(&hook.Handler[*kernel.BatchEvent]{Func: func(ev *kernel.BatchEvent) error {
		called++
		return ev.Next()
	}})
	if code, out := e.batch(t, post("orders", map[string]any{"total_qty": 1})); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if called != 0 {
		t.Fatalf("foreign handler called %d times", called)
	}
}

func TestAfterHookNeverSeesHiddenFields(t *testing.T) {
	e := setup(t)
	open := ""
	c := core.NewBaseCollection("vault")
	c.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "api_secret", Hidden: true})
	c.CreateRule = &open
	if err := e.app.Save(c); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	id := kernel.OnBatchFor(e.app).Bind(&hook.Handler[*kernel.BatchEvent]{Func: func(ev *kernel.BatchEvent) error {
		if ev.Name == kernel.BatchAfter {
			got = ev.Requests[0].Body
		}
		return ev.Next()
	}})
	defer kernel.OnBatchFor(e.app).Unbind(id)
	if code, out := e.batch(t, post("vault", map[string]any{"title": "t", "api_secret": "s3cret"})); code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	if got == nil || got["title"] != "t" {
		t.Fatalf("after body: %v", got)
	}
	if _, ok := got["api_secret"]; ok {
		t.Fatalf("hidden field reached the hook: %v", got)
	}
}

func TestAddedIDNotLeakedToClient(t *testing.T) {
	e := setup(t)
	e.rule(t, Rule{Name: "cap", Match: []Match{{"order_items", "POST"}}, AssertPost: "sum(order_items, qty) <= 10"})
	code, out := e.batch(t, postURL("/api/collections/order_items/records?fields=product", map[string]any{"qty": 1, "product": "p"}))
	if code != 200 {
		t.Fatalf("%d %v", code, out)
	}
	res, _ := out["results"].([]any)
	body, _ := res[0].(map[string]any)["body"].(map[string]any)
	if _, ok := body["id"]; ok || body["product"] != "p" {
		t.Fatalf("client asked for fields=product only: %v", body)
	}
}

func TestHookBoundMidRequestFailsClosed(t *testing.T) {
	e := setup(t)
	e.rule(t, Rule{Name: "t", Match: []Match{{"orders", "POST"}}, Assert: "req(0).body.total_qty == 3"})
	bound := false
	e.app.OnRecordCreateRequest("orders").BindFunc(func(ev *core.RecordRequestEvent) error {
		if !bound {
			bound = true
			kernel.OnBatchFor(e.app).Bind(&hook.Handler[*kernel.BatchEvent]{Id: "late", Func: func(b *kernel.BatchEvent) error { return b.Next() }})
		}
		return ev.Next()
	})
	defer kernel.OnBatchFor(e.app).Unbind("late")
	code, out := e.batch(t, post("orders", map[string]any{"total_qty": 3}))
	if !bound {
		t.Fatal("record hook did not run inside the batch")
	}
	if code != 503 {
		t.Fatalf("want 503 fail-closed, got %d %v", code, out)
	}
	if e.count(t, "orders") != 0 {
		t.Fatal("must roll back")
	}
}
