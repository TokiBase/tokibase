//go:build !no_totp

package totp

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
)

// Enforcement is the effective set of users that must have TOTP enabled.
type Enforcement struct {
	Roles      []string `json:"roles"`
	Superusers bool     `json:"superusers"`
}

func (c Enforcement) active() bool { return c.Superusers || len(c.Roles) > 0 }

func splitRoles(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadEnforcement merges the env settings with the `totp_enforce` param written by the CLI.
func loadEnforcement(app core.App) Enforcement {
	c := Enforcement{Roles: splitRoles(os.Getenv(EnvRequiredRoles))}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvRequireSuper))) {
	case "1", "true", "yes", "on":
		c.Superusers = true
	}
	if raw, ok := readParam(app, ParamEnforce); ok {
		var p Enforcement
		if json.Unmarshal([]byte(raw), &p) == nil {
			c.Superusers = c.Superusers || p.Superusers
			for _, r := range p.Roles {
				dup := false
				for _, x := range c.Roles {
					dup = dup || x == r
				}
				if !dup {
					c.Roles = append(c.Roles, r)
				}
			}
		}
	}
	return c
}

func graceDays() int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvGraceDays)))
	if n < 0 {
		n = 0
	}
	return n
}

func (c Enforcement) matches(rec *core.Record) bool {
	if rec.IsSuperuser() {
		return c.Superusers
	}
	for _, f := range []string{"roles", "role"} {
		if rec.Collection().Fields.GetByName(f) == nil {
			continue
		}
		for _, v := range rec.GetStringSlice(f) {
			for _, r := range c.Roles {
				if v == r {
					return true
				}
			}
		}
	}
	return false
}

func readParam(app core.App, key string) (string, bool) {
	var v string
	if err := app.DB().NewQuery("SELECT [[value]] FROM {{_params}} WHERE [[id]]={:k}").Bind(dbx.Params{"k": key}).Row(&v); err != nil {
		return "", false
	}
	return v, true
}

func setParam(app core.App, key, jsonValue string) {
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000Z")
	_, err := app.DB().NewQuery(`INSERT INTO {{_params}} ([[id]],[[value]],[[created]],[[updated]]) VALUES ({:k},{:v},{:t},{:t})
		ON CONFLICT([[id]]) DO UPDATE SET [[value]]=excluded.[[value]], [[updated]]=excluded.[[updated]]`).
		Bind(dbx.Params{"k": key, "v": jsonValue, "t": now}).Execute()
	if err != nil {
		app.Logger().Error("totp: failed to write param", "key", key, "error", err)
	}
}

func deleteParam(app core.App, key string) {
	_, _ = app.DB().NewQuery("DELETE FROM {{_params}} WHERE [[id]]={:k}").Bind(dbx.Params{"k": key}).Execute()
}

// SetEnforcement stores the enforcement set (CLI) and (re)starts the grace
// window when enforcement turns on or the grace window is extended.
func SetEnforcement(app core.App, c Enforcement, restartGrace bool) {
	b, _ := json.Marshal(c)
	setParam(app, ParamEnforce, string(b))
	if !c.active() {
		deleteParam(app, ParamEnforcedAt)
	} else if _, ok := readParam(app, ParamEnforcedAt); !ok || restartGrace {
		ts, _ := json.Marshal(time.Now().UTC().Format(time.RFC3339))
		setParam(app, ParamEnforcedAt, string(ts))
	}
	audit(ActionEnforce, "", "", map[string]any{"roles": c.Roles, "superusers": c.Superusers, "by": "cli", "graceDays": graceDays()})
}

// EnforcedAt returns the time enforcement started, if recorded.
func EnforcedAt(app core.App) (time.Time, bool) {
	raw, ok := readParam(app, ParamEnforcedAt)
	if !ok {
		return time.Time{}, false
	}
	var s string
	if json.Unmarshal([]byte(raw), &s) != nil {
		s = strings.Trim(raw, `"`)
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func (m *Module) inGrace() bool {
	d := graceDays()
	if d == 0 {
		return false
	}
	at, ok := EnforcedAt(m.app)
	return ok && m.now().Before(at.AddDate(0, 0, d))
}

// enforceHook refuses logins of users that must have TOTP but do not.
func (m *Module) enforceHook(e *core.RecordAuthRequestEvent) error {
	if e.AuthMethod == "" || e.AuthMethod == AuthMethod {
		return e.Next()
	}
	cfg := loadEnforcement(e.App)
	if !cfg.active() || !cfg.matches(e.Record) {
		return e.Next()
	}
	if row := findRow(e.App, e.Record); row != nil && row.GetBool("enabled") {
		return e.Next()
	}
	if m.inGrace() {
		return e.Next()
	}
	audit(ActionBlocked, e.Record.Collection().Name, e.Record.Id, map[string]any{"method": e.AuthMethod, "ip": e.RealIP()})
	msg := "Two-factor authentication (TOTP) is required for this account. Enrol an authenticator app while still signed in, or ask an administrator to extend the grace period."
	return e.ForbiddenError(msg, map[string]validation.Error{"totp": validation.NewError(CodeRequired, msg)})
}
