package denylog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
	"github.com/tokibase/tokibase/tools/logger"
)

func setup(t *testing.T) (*tests.TestApp, http.Handler) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	app.Settings().Logs.MaxDays = 5 // the test app disables logs

	Register(app)

	c := kernel.NewBaseCollection("locked") // all rules nil: superusers only
	c.Fields.Add(&kernel.TextField{Name: "title"})
	if err := app.Save(c); err != nil {
		t.Fatal(err)
	}

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	var h http.Handler
	err = app.OnServe().Trigger(&core.ServeEvent{App: app, Router: router}, func(se *core.ServeEvent) error {
		h, err = se.Router.BuildMux()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, h
}

func flush(t *testing.T, app *tests.TestApp) {
	t.Helper()
	h, ok := app.Logger().Handler().(*logger.BatchHandler)
	if !ok {
		t.Fatal("unexpected logger handler")
	}
	h.WriteAll(context.WithValue(context.Background(), logger.BlockKey, true))
}

func do(h http.Handler, method, url, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestForbiddenGuest(t *testing.T) {
	app, h := setup(t)

	if rec := do(h, "POST", "/api/collections/locked/records", `{"title":"x"}`); rec.Code != 403 {
		t.Fatalf("want 403, got %d %s", rec.Code, rec.Body.String())
	}
	flush(t, app)

	rows, err := Tail(app, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 entry, got %d: %+v", len(rows), rows)
	}
	d := rows[0].Data
	if d["status"] != float64(403) || d["auth_kind"] != "guest" || d["collection"] != "locked" ||
		d["rule_kind"] != "create" || d["method"] != "POST" || d["path"] != "/api/collections/locked/records" ||
		d["reason"] == "" || d["reason"] == nil {
		t.Fatalf("unexpected data: %+v", d)
	}
	if _, ok := d["rate_limited"]; ok {
		t.Fatal("rate_limited must not be set on 403")
	}
}

func TestRateLimited429(t *testing.T) {
	app, h := setup(t)
	app.Settings().RateLimits.Enabled = true
	app.Settings().RateLimits.Rules = []kernel.RateLimitRule{{Label: "*:create", Duration: 60, MaxRequests: 1}}

	if rec := do(h, "POST", "/api/collections/locked/records", `{"title":"x"}`); rec.Code != 403 {
		t.Fatalf("first: want 403, got %d", rec.Code)
	}
	if rec := do(h, "POST", "/api/collections/locked/records", `{"title":"x"}`); rec.Code != 429 {
		t.Fatalf("second: want 429, got %d %s", rec.Code, rec.Body.String())
	}
	flush(t, app)

	rows, err := Tail(app, time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range rows {
		if r.Data["status"] == float64(429) {
			found = true
			if r.Data["rate_limited"] != true || r.Data["auth_kind"] != "guest" {
				t.Fatalf("unexpected data: %+v", r.Data)
			}
			if _, ok := r.Data["rule_kind"]; ok {
				t.Fatal("rule_kind must only be set on 403")
			}
		}
	}
	if !found {
		t.Fatalf("no 429 entry in %+v", rows)
	}
}

func TestSuccessNotLogged(t *testing.T) {
	app, h := setup(t)
	if rec := do(h, "GET", "/api/health", ""); rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	flush(t, app)
	rows, err := Tail(app, time.Hour, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("want none, got %v %v", rows, err)
	}
}

func TestOffRegistersNothing(t *testing.T) {
	t.Setenv("TOKI_DENYLOG", "off")
	if Enabled() {
		t.Fatal("must be disabled")
	}
}

func TestSampling(t *testing.T) {
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	s := newSampler(func() time.Time { return now })
	for i := 0; i < MaxPerMinute; i++ {
		if r := s.allow(403, "r"); r != sampleLog {
			t.Fatalf("#%d: %v", i, r)
		}
	}
	if r := s.allow(403, "r"); r != sampleCap {
		t.Fatalf("want cap notice, got %v", r)
	}
	if r := s.allow(403, "r"); r != sampleDrop {
		t.Fatalf("want drop, got %v", r)
	}
	if r := s.allow(401, "r"); r != sampleLog {
		t.Fatal("separate status must have its own budget")
	}
	now = now.Add(61 * time.Second)
	if r := s.allow(403, "r"); r != sampleLog {
		t.Fatal("new window must log again")
	}
	sums := s.takeSummaries()
	if len(sums) != 1 || sums[0].count != 2 || sums[0].status != 403 {
		t.Fatalf("summary: %+v", sums)
	}
}

func TestRuleKind(t *testing.T) {
	cases := []struct{ m, p, want string }{
		{"GET", "/api/collections/a/records", "list"},
		{"GET", "/api/collections/a/records/x", "view"},
		{"POST", "/api/collections/a/records", "create"},
		{"PATCH", "/api/collections/a/records/x", "update"},
		{"DELETE", "/api/collections/a/records/x", "delete"},
		{"POST", "/api/collections/a/auth-with-password", ""},
		{"DELETE", "/api/collections/a/records", ""},
	}
	for _, c := range cases {
		if got := ruleKind(c.m, c.p); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.m, c.p, got, c.want)
		}
	}
}
