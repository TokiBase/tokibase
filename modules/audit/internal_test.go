//go:build !no_audit

package audit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCapJSONTruncates(t *testing.T) {
	s := capJSON(map[string]any{"big": strings.Repeat("é", 100<<10)})
	if len(*s) > maxJSONBytes || !strings.Contains(*s, truncatedMarker) {
		t.Fatalf("not truncated: len=%d", len(*s))
	}
}

func TestStripNested(t *testing.T) {
	v := strip(normalize(map[string]any{"smtp": map[string]any{"host": "h", "password": "p"}, "s3": map[string]any{"secret": "s"}, "oauth2": []any{map[string]any{"clientSecret": "c", "name": "n"}}}))
	raw := *capJSON(v)
	for _, bad := range []string{`"p"`, `"s"`, `"c"`} {
		if strings.Contains(raw, bad) {
			t.Fatalf("leaked %s in %s", bad, raw)
		}
	}
	if !strings.Contains(raw, `"host":"h"`) || !strings.Contains(raw, `"name":"n"`) {
		t.Fatalf("over-stripped: %s", raw)
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{"1h": now.Add(-time.Hour), "7d": now.AddDate(0, 0, -7), "": {}} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	if _, err := ParseSince("nope", now); err == nil {
		t.Fatal("expected error")
	}
}

func TestCLI(t *testing.T) {
	app := newTestApp(t)
	l := New(app)
	for i := 0; i < 3; i++ {
		_ = l.Append(&Entry{ActorKind: ActorSystem, Action: ActionBackupCreate, Record: "b.zip"})
	}
	run := func(args ...string) (string, error) {
		c := NewCommand(app)
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetErr(&out)
		c.SetArgs(args)
		err := c.Execute()
		return out.String(), err
	}
	if out, err := run("verify"); err != nil || !strings.Contains(out, "OK: 3") {
		t.Fatalf("verify: %q %v", out, err)
	}
	if out, _ := run("tail", "--limit", "2", "--json"); strings.Count(out, "\n") != 2 {
		t.Fatalf("tail: %q", out)
	}
	if out, _ := run("export", "--since", "1h"); strings.Count(out, "\n") != 3 {
		t.Fatalf("export: %q", out)
	}
	app.AuxDB().NewQuery(`UPDATE _audit SET record='x' WHERE seq=2`).Execute()
	if out, err := run("verify"); err == nil || !strings.Contains(out, "BROKEN at seq 2") {
		t.Fatalf("verify tamper: %q %v", out, err)
	}
}
