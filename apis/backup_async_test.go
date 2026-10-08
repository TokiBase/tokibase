package apis_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tokibase/tokibase/tests"
)

const backupAsyncSuperuserToken = "eyJhbGciOiJIUzI1NiJ9.eyJpZCI6InN5d2JoZWNuaDQ2cmhtMCIsInR5cGUiOiJhdXRoIiwiY29sbGVjdGlvbklkIjoicGJjXzMxNDI2MzU4MjMiLCJleHAiOjI1MjQ2MDQ0NjEsInJlZnJlc2hhYmxlIjp0cnVlfQ.UXgO3j-0BumcugrFjbd7j0M4MQvbrLggLlcu_YNGjoY"

func TestBackupsCreateAsync(t *testing.T) {
	t.Parallel()

	scenarios := []tests.ApiScenario{
		{
			Name:            "status unauthorized",
			Method:          http.MethodGet,
			URL:             "/api/backups/status",
			ExpectedStatus:  401,
			ExpectedContent: []string{`"data":{}`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "status idle",
			Method:          http.MethodGet,
			URL:             "/api/backups/status",
			Headers:         map[string]string{"Authorization": backupAsyncSuperuserToken},
			ExpectedStatus:  200,
			ExpectedContent: []string{`"state":"idle"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
		{
			Name:            "async create answers 202 and finishes in the background",
			Method:          http.MethodPost,
			URL:             "/api/backups?async=true",
			Body:            strings.NewReader(`{"name":"async_test.zip"}`),
			Headers:         map[string]string{"Authorization": backupAsyncSuperuserToken},
			ExpectedStatus:  202,
			ExpectedContent: []string{`"state":"running"`, `"name":"async_test.zip"`},
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				deadline := time.Now().Add(30 * time.Second)
				for {
					files, err := getBackupFiles(app)
					if err != nil {
						t.Fatal(err)
					}
					if len(files) == 1 && files[0].Key == "async_test.zip" && !app.Store().Has("@activeBackup") {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("the async backup did not finish: %v", files)
					}
					time.Sleep(50 * time.Millisecond)
				}
			},
		},
		{
			Name:            "Prefer header with an invalid name still validates first",
			Method:          http.MethodPost,
			URL:             "/api/backups",
			Body:            strings.NewReader(`{"name":"!test.zip"}`),
			Headers:         map[string]string{"Authorization": backupAsyncSuperuserToken, "Prefer": "respond-async"},
			ExpectedStatus:  400,
			ExpectedContent: []string{`"name":{"code":"validation_match_invalid"`},
			ExpectedEvents:  map[string]int{"*": 0},
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
