package tlscheck

import (
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tests"
)

func serve(t *testing.T, addr string, headers []string) error {
	t.Helper()
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()
	app.Settings().TrustedProxy.Headers = headers
	Register(app)
	return app.OnServe().Trigger(&core.ServeEvent{App: app, Server: &http.Server{Addr: addr}}, func(*core.ServeEvent) error { return nil })
}

func TestStrict(t *testing.T) {
	t.Setenv("TOKI_TLS_CHECK", "strict")
	if err := serve(t, "0.0.0.0:8090", nil); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("want strict error, got %v", err)
	}
	if err := serve(t, ":8090", nil); err == nil {
		t.Fatal("empty host must be treated as all interfaces")
	}
	for _, addr := range []string{"127.0.0.1:8090", "[::1]:8090", "localhost:8090"} {
		if err := serve(t, addr, nil); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	if err := serve(t, "0.0.0.0:8090", []string{"X-Forwarded-For"}); err != nil {
		t.Fatalf("proxy header set: %v", err)
	}
}

func TestWarnAndOff(t *testing.T) {
	t.Setenv("TOKI_TLS_CHECK", "warn")
	if err := serve(t, "0.0.0.0:8090", nil); err != nil {
		t.Fatalf("warn must not fail: %v", err)
	}
	t.Setenv("TOKI_TLS_CHECK", "off")
	if err := serve(t, "0.0.0.0:8090", nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnixListener(t *testing.T) {
	t.Setenv("TOKI_TLS_CHECK", "strict")
	app, _ := tests.NewTestApp()
	defer app.Cleanup()
	Register(app)
	l, err := net.Listen("unix", t.TempDir()+"/s.sock")
	if err != nil {
		t.Skip(err)
	}
	defer l.Close()
	err = app.OnServe().Trigger(&core.ServeEvent{App: app, Server: &http.Server{}, Listener: l}, func(*core.ServeEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestTLSRequested(t *testing.T) {
	cases := map[string]bool{
		"serve":                     false,
		"serve --http 0.0.0.0:80":   false,
		"serve --https 0.0.0.0:443": true,
		"serve example.com":         true,
		"--dir x serve":             false,
		"serve --origins a.com":     false,
		"serve --https=0.0.0.0:443": true,
		"superuser upsert a@b.c pw": false,
	}
	for line, want := range cases {
		if got := tlsRequested(strings.Fields(line)); got != want {
			t.Errorf("%q: got %v want %v", line, got, want)
		}
	}
}
