//go:build no_mcp

package mcp

import (
	"net/http"
	"testing"

	"github.com/tokibase/tokibase/tests"
)

// With the no_mcp tag /api/mcp does not exist, even with TOKI_MCP=on.
func TestHTTPRouteAbsentWithoutModule(t *testing.T) {
	t.Setenv("TOKI_MCP", "on")
	s := tests.ApiScenario{
		Name:   "no_mcp: /api/mcp is 404",
		Method: http.MethodPost, URL: "/api/mcp",
		Headers:         map[string]string{"Authorization": "Bearer tka_x"},
		ExpectedStatus:  404,
		ExpectedContent: []string{`"code":404`},
		TestAppFactory: func(tb testing.TB) *tests.TestApp {
			app, err := tests.NewTestApp()
			if err != nil {
				tb.Fatal(err)
			}
			Register(app)
			RegisterHTTP(app)
			return app
		},
	}
	s.Test(t)
}
