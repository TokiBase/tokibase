//go:build !no_adminlock

package adminlock_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/modules/adminlock"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/ui"
)

const superuserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

func factory(mode adminlock.Mode) func(testing.TB) *tests.TestApp {
	return func(t testing.TB) *tests.TestApp {
		app, err := tests.NewTestApp()
		if err != nil {
			t.Fatal(err)
		}
		adminlock.RegisterMode(app, mode)
		return app
	}
}

func hdr(extra map[string]string) map[string]string {
	h := map[string]string{"Authorization": superuserToken}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

const collBody = `{"name":"demo","type":"base","fields":[{"name":"a","type":"text"}]}`

func TestReadonly(t *testing.T) {
	ref := map[string]string{"Referer": "http://example.com/_/#/collections"}
	blocked := `"message":"` + adminlock.ReadonlyMessage + `"`
	scenarios := []tests.ApiScenario{
		{
			Name: "collection create with UI referer is blocked", Method: http.MethodPost, URL: "/api/collections",
			Body: strings.NewReader(collBody), Headers: hdr(ref), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 403, ExpectedContent: []string{`"status":403`, blocked, `"data":{}`},
		},
		{
			Name: "collection create without referer is allowed", Method: http.MethodPost, URL: "/api/collections",
			Body: strings.NewReader(collBody), Headers: hdr(nil), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 200, ExpectedContent: []string{`"name":"demo"`},
		},
		{
			Name: "collection update with UI referer is blocked", Method: http.MethodPatch, URL: "/api/collections/demo2",
			Body: strings.NewReader(`{"name":"demo3"}`), Headers: hdr(ref), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 403, ExpectedContent: []string{blocked},
		},
		{
			Name: "settings update with UI referer is blocked", Method: http.MethodPatch, URL: "/api/settings",
			Body: strings.NewReader(`{"meta":{"appName":"x"}}`), Headers: hdr(ref), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 403, ExpectedContent: []string{blocked},
		},
		{
			Name: "superuser record update with UI referer is blocked", Method: http.MethodPatch, URL: "/api/collections/_superusers/records/sywbhecnh46rhm0",
			Body: strings.NewReader(`{"email":"new@example.com"}`), Headers: hdr(ref), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 403, ExpectedContent: []string{blocked},
		},
		{
			Name: "user collection record update with UI referer is allowed", Method: http.MethodPatch, URL: "/api/collections/demo2/records/0yxhwia2amd8gec",
			Body: strings.NewReader(`{"title":"changed"}`), Headers: hdr(ref), TestAppFactory: factory(adminlock.ModeReadonly),
			ExpectedStatus: 200, ExpectedContent: []string{`"title":"changed"`},
		},
		{
			Name: "Origin without /_/ path does not count as UI", Method: http.MethodPatch, URL: "/api/settings",
			Body: strings.NewReader(`{"meta":{"appName":"x"}}`), Headers: hdr(map[string]string{"Origin": "http://example.com"}),
			TestAppFactory: factory(adminlock.ModeReadonly), ExpectedStatus: 200, ExpectedContent: []string{`"appName":"x"`},
		},
		{
			Name: "referer of another host does not count as UI", Method: http.MethodPatch, URL: "/api/settings",
			Body: strings.NewReader(`{"meta":{"appName":"x"}}`), Headers: hdr(map[string]string{"Referer": "http://evil.test/_/"}),
			TestAppFactory: factory(adminlock.ModeReadonly), ExpectedStatus: 200, ExpectedContent: []string{`"appName":"x"`},
		},
	}
	for _, s := range scenarios {
		s.Test(t)
	}
}

func TestSinkAndOn(t *testing.T) {
	var got []adminlock.Block
	adminlock.SetAuditSink(func(b adminlock.Block) { got = append(got, b) })
	defer adminlock.SetAuditSink(nil)

	(&tests.ApiScenario{
		Name: "blocked is reported to the sink", Method: http.MethodPatch, URL: "/api/settings",
		Body: strings.NewReader(`{}`), Headers: hdr(map[string]string{"Referer": "http://example.com/_/"}),
		TestAppFactory: factory(adminlock.ModeReadonly), ExpectedStatus: 403,
		ExpectedContent: []string{`"status":403`},
		AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
			if len(got) != 1 || got[0].Action != "settings.update" {
				t.Fatalf("unexpected sink calls: %+v", got)
			}
		},
	}).Test(t)

	(&tests.ApiScenario{
		Name: "mode on does not block", Method: http.MethodPatch, URL: "/api/settings",
		Body: strings.NewReader(`{"meta":{"appName":"x"}}`), Headers: hdr(map[string]string{"Referer": "http://example.com/_/"}),
		TestAppFactory: factory(adminlock.ModeOn), ExpectedStatus: 200, ExpectedContent: []string{`"appName":"x"`},
	}).Test(t)
}

func TestOff(t *testing.T) {
	old := ui.DistDirFS
	defer func() { ui.DistDirFS = old }()

	(&tests.ApiScenario{
		Name: "GET /_/ is 404 when off", Method: http.MethodGet, URL: "/_/",
		TestAppFactory: factory(adminlock.ModeOff), ExpectedStatus: 404, ExpectedContent: []string{`"status":404`},
	}).Test(t)
	(&tests.ApiScenario{
		Name: "API keeps working when off", Method: http.MethodGet, URL: "/api/health",
		TestAppFactory: factory(adminlock.ModeOff), ExpectedStatus: 200, ExpectedContent: []string{`"code":200`},
	}).Test(t)
}

func TestModeFromEnv(t *testing.T) {
	for in, want := range map[string]adminlock.Mode{"": adminlock.ModeOn, "on": adminlock.ModeOn, "READONLY": adminlock.ModeReadonly, "off": adminlock.ModeOff, "bogus": adminlock.ModeOn} {
		t.Setenv("TOKI_ADMIN_UI", in)
		if got := adminlock.ModeFromEnv(); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
