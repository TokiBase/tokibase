package timelint

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tests"
)

func TestZoneless(t *testing.T) {
	cases := map[string]bool{
		"2026-10-07 10:00:00":       true,
		"2026-10-07T10:00:00":       true,
		"2026-10-07 10:00:00.123":   true,
		"2026-10-07 10:00:00Z":      false,
		"2026-10-07 10:00:00.000Z":  false,
		"2026-10-07T10:00:00+07:00": false,
		"2026-10-07T10:00:00-0700":  false,
		"2026-10-07":                false,
		"":                          false,
		"garbage:text":              false,
	}
	for in, want := range cases {
		if got := Zoneless(in); got != want {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
}

func setup(t *testing.T) (*tests.TestApp, http.Handler) {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Cleanup)
	Register(app)

	c := kernel.NewBaseCollection("events")
	c.Fields.Add(&kernel.DateField{Name: "at"})
	c.CreateRule = new(string)
	c.UpdateRule = new(string)
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

func do(h http.Handler, method, url, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStrict(t *testing.T) {
	t.Setenv("TOKI_TIMELINT", "strict")
	_, h := setup(t)

	rec := do(h, "POST", "/api/collections/events/records", `{"at":"2026-10-07 10:00:00"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"code":"`+ErrCode+`"`) || !strings.Contains(rec.Body.String(), `"at"`) {
		t.Fatalf("want 400 %s, got %d %s", ErrCode, rec.Code, rec.Body.String())
	}
	for _, v := range []string{"2026-10-07 10:00:00Z", "2026-10-07T10:00:00+07:00", "2026-10-07"} {
		rec = do(h, "POST", "/api/collections/events/records", `{"at":"`+v+`"}`)
		if rec.Code != 200 {
			t.Fatalf("%s: want 200, got %d %s", v, rec.Code, rec.Body.String())
		}
	}

	// update path
	var id string
	rec = do(h, "POST", "/api/collections/events/records", `{"at":"2026-10-07 10:00:00Z"}`)
	id = strings.Split(strings.Split(rec.Body.String(), `"id":"`)[1], `"`)[0]
	rec = do(h, "PATCH", "/api/collections/events/records/"+id, `{"at":"2026-10-07 11:00:00"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), ErrCode) {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
}

func TestWarnAllowsAndDedupes(t *testing.T) {
	t.Setenv("TOKI_TIMELINT", "warn")
	app, h := setup(t)
	_ = app

	for i := 0; i < 3; i++ {
		rec := do(h, "POST", "/api/collections/events/records", `{"at":"2026-10-07 10:00:00"}`)
		if rec.Code != 200 {
			t.Fatalf("warn must allow: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestWarnDedupeUnit(t *testing.T) {
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	m := &module{policy: PolicyWarn, now: func() time.Time { return base }, warned: map[warnKey]time.Time{}}
	m.warn(app, "c", "f", "x")
	first := m.warned[warnKey{"c", "f"}]
	base = base.Add(30 * time.Minute)
	m.warn(app, "c", "f", "x")
	if !m.warned[warnKey{"c", "f"}].Equal(first) {
		t.Fatal("must not re-log inside the hour")
	}
	base = base.Add(31 * time.Minute)
	m.warn(app, "c", "f", "x")
	if m.warned[warnKey{"c", "f"}].Equal(first) {
		t.Fatal("must re-log after the hour")
	}
}

func TestScan(t *testing.T) {
	t.Setenv("TOKI_TIMELINT", "off")
	app, h := setup(t)
	_ = h
	for _, v := range []string{"2026-10-07", "2026-10-07 10:00:00Z"} {
		r := core.NewRecord(mustColl(t, app))
		r.Set("at", v)
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := Scan(app)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Collection == "events" && r.Field == "at" {
			if r.Total != 2 || r.Midnight != 1 {
				t.Fatalf("got %+v", r)
			}
			return
		}
	}
	t.Fatal("events.at not found")
}

func mustColl(t *testing.T, app *tests.TestApp) *core.Collection {
	c, err := app.FindCollectionByNameOrId("events")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var base = time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
