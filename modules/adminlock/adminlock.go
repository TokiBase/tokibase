// Package adminlock lets production servers run with the Admin UI in
// read-only mode or not served at all, selected by env TOKI_ADMIN_UI.
package adminlock

import (
	"net/url"
	"os"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/ui"
)

const hookId = "__tokiAdminLock__"

// hookPriority runs the guards before every other request handler.
const hookPriority = -1 << 21

// ActionBlocked is the audit action recorded for every rejected request.
const ActionBlocked = "admin.blocked"

// ReadonlyMessage is the error message returned for blocked requests.
const ReadonlyMessage = "Admin UI is read-only on this server (TOKI_ADMIN_UI=readonly). Apply schema and settings changes through migrations or the CLI."

// Mode is the Admin UI mode.
type Mode string

const (
	ModeOn       Mode = "on"
	ModeReadonly Mode = "readonly"
	ModeOff      Mode = "off"
)

// Block describes a rejected request (passed to the audit sink).
type Block struct {
	Action     string // e.g. "collection.update"
	Collection string
	Record     string
	Method     string
	Path       string
	IP         string
	UserAgent  string
	ActorID    string
	ActorColl  string
}

var auditSink func(Block)

// SetAuditSink sets the function called for every blocked request
// (wired by the root package to the audit log).
func SetAuditSink(fn func(Block)) { auditSink = fn }

// ModeFromEnv parses TOKI_ADMIN_UI. Unknown values fall back to "on".
func ModeFromEnv() Mode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_ADMIN_UI"))) {
	case "readonly", "read-only", "ro":
		return ModeReadonly
	case "off", "false", "0", "disabled":
		return ModeOff
	}
	return ModeOn
}

// Register applies the mode from the environment.
func Register(app core.App) Mode {
	m := ModeFromEnv()
	RegisterMode(app, m)
	return m
}

// RegisterMode applies m to app.
//
// "off" clears ui.DistDirFS (process wide, like a no_ui build), so the
// /_/ routes, UI extensions and the installer browser redirect are not
// registered. It must run before the router is created.
func RegisterMode(app core.App, m Mode) {
	switch m {
	case ModeOff:
		ui.DistDirFS = nil
		app.Logger().Warn("adminlock: Admin UI is disabled (TOKI_ADMIN_UI=off)")
	case ModeReadonly:
		app.Logger().Warn("adminlock: Admin UI is read-only (TOKI_ADMIN_UI=readonly)")
		bindGuards(app)
	}
}

// FromAdminUI reports whether the request is a superuser request issued
// by the Admin UI: superuser auth AND Referer or Origin that points at /_/
// on the same host.
func FromAdminUI(e *core.RequestEvent) bool {
	if !e.HasSuperuserAuth() {
		return false
	}
	if e.Request.Header.Get("X-TokiBase-Admin-UI") == "1" {
		return true
	}
	for _, h := range []string{"Referer", "Origin"} {
		if pointsAtUI(e.Request.Header.Get(h), e.Request.Host) {
			return true
		}
	}
	return false
}

func pointsAtUI(raw, host string) bool {
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Host, host) {
		return false
	}
	// an Origin has no path: it only proves the host, which is not enough
	return u.Path == "/_" || strings.HasPrefix(u.Path, "/_/")
}

func reject(e *core.RequestEvent, action, collection, record string) {
	b := Block{
		Action: action, Collection: collection, Record: record,
		Method: e.Request.Method, Path: e.Request.URL.Path,
		IP: e.RealIP(), UserAgent: e.Request.UserAgent(),
	}
	if e.Auth != nil {
		b.ActorID, b.ActorColl = e.Auth.Id, e.Auth.Collection().Name
	}
	e.App.Logger().Warn("adminlock: blocked Admin UI request",
		"action", action, "collection", collection, "record", record,
		"method", b.Method, "path", b.Path, "ip", b.IP, "actor", b.ActorID)
	if auditSink != nil {
		auditSink(b)
	}
}

func forbidden() error {
	return router.NewForbiddenError(ReadonlyMessage, nil)
}

func bindGuards(app core.App) {
	collGuard := func(action string) *hook.Handler[*core.CollectionRequestEvent] {
		return &hook.Handler[*core.CollectionRequestEvent]{
			Id: hookId, Priority: hookPriority,
			Func: func(e *core.CollectionRequestEvent) error {
				if FromAdminUI(e.RequestEvent) {
					reject(e.RequestEvent, action, e.Collection.Name, e.Collection.Id)
					return forbidden()
				}
				return e.Next()
			},
		}
	}
	app.OnCollectionCreateRequest().Bind(collGuard("collection.create"))
	app.OnCollectionUpdateRequest().Bind(collGuard("collection.update"))
	app.OnCollectionDeleteRequest().Bind(collGuard("collection.delete"))

	app.OnCollectionsImportRequest().Bind(&hook.Handler[*core.CollectionsImportRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.CollectionsImportRequestEvent) error {
			if FromAdminUI(e.RequestEvent) {
				reject(e.RequestEvent, "collection.update", "", "import")
				return forbidden()
			}
			return e.Next()
		},
	})

	app.OnSettingsUpdateRequest().Bind(&hook.Handler[*core.SettingsUpdateRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.SettingsUpdateRequestEvent) error {
			if FromAdminUI(e.RequestEvent) {
				reject(e.RequestEvent, "settings.update", "", "")
				return forbidden()
			}
			return e.Next()
		},
	})

	recGuard := func(action string) *hook.Handler[*core.RecordRequestEvent] {
		return &hook.Handler[*core.RecordRequestEvent]{
			Id: hookId, Priority: hookPriority,
			Func: func(e *core.RecordRequestEvent) error {
				if e.Collection.Name == core.CollectionNameSuperusers && FromAdminUI(e.RequestEvent) {
					reject(e.RequestEvent, action, e.Collection.Name, e.Record.Id)
					return forbidden()
				}
				return e.Next()
			},
		}
	}
	app.OnRecordCreateRequest().Bind(recGuard("record.create"))
	app.OnRecordUpdateRequest().Bind(recGuard("record.update"))
	app.OnRecordDeleteRequest().Bind(recGuard("record.delete"))
}
