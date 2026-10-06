package audit

import (
	"os"
	"strings"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/security"
)

const hookId = "__tokiAudit__"

// hookPriority makes the audit handlers the outermost ones, so they snapshot
// the state before any other handler and observe the final result.
const hookPriority = -1 << 20

// Enabled reports whether auditing is enabled (env TOKI_AUDIT=off disables it).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_AUDIT"))) {
	case "off", "false", "0", "disabled":
		return false
	}
	return true
}

// Register binds the audit hooks to app and returns the chain writer.
// The table is created (IF NOT EXISTS) in auxiliary.db on bootstrap, or
// immediately if the app is already bootstrapped.
func Register(app core.App) *Log {
	l := New(app)

	init := func() {
		if err := l.Init(); err != nil {
			app.Logger().Error("audit: failed to initialize the _audit table", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})

	// records (superuser or impersonated sessions only)
	app.OnRecordCreateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			if err := e.Next(); err != nil || !ok {
				return err
			}
			after := recordSnapshot(e.Record)
			l.write(e.RequestEvent, act, ActionRecordCreate, e.Collection.Name, e.Record.Id, nil, after, false)
			return nil
		},
	})
	app.OnRecordUpdateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			var before snapshot
			if ok {
				// the request body is already loaded in e.Record: use the pristine copy
				before = recordSnapshot(e.Record.Original())
			}
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionRecordUpdate, e.Collection.Name, e.Record.Id, before, recordSnapshot(e.Record), true)
			return nil
		},
	})
	app.OnRecordDeleteRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			var before snapshot
			if ok {
				before = recordSnapshot(e.Record)
			}
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionRecordDelete, e.Collection.Name, e.Record.Id, before, nil, false)
			return nil
		},
	})

	// collections (the collections API is superuser only)
	app.OnCollectionCreateRequest().Bind(&hook.Handler[*core.CollectionRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.CollectionRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionCollectionCreate, e.Collection.Name, e.Collection.Id, nil, objectSnapshot(e.Collection), false)
			return nil
		},
	})
	app.OnCollectionUpdateRequest().Bind(&hook.Handler[*core.CollectionRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.CollectionRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			var before snapshot
			if ok {
				// e.Collection already carries the submitted changes: read the stored one
				if old, err := e.App.FindCollectionByNameOrId(e.Collection.Id); err == nil {
					before = objectSnapshot(old)
				}
			}
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionCollectionUpdate, e.Collection.Name, e.Collection.Id, before, objectSnapshot(e.Collection), true)
			return nil
		},
	})
	app.OnCollectionDeleteRequest().Bind(&hook.Handler[*core.CollectionRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.CollectionRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			var before snapshot
			if ok {
				before = objectSnapshot(e.Collection)
			}
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionCollectionDelete, e.Collection.Name, e.Collection.Id, before, nil, false)
			return nil
		},
	})
	app.OnCollectionsImportRequest().Bind(&hook.Handler[*core.CollectionsImportRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.CollectionsImportRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			if err := e.Next(); err != nil || !ok {
				return err
			}
			names := make([]any, 0, len(e.CollectionsData))
			for _, c := range e.CollectionsData {
				if n, _ := c["name"].(string); n != "" {
					names = append(names, n)
				}
			}
			l.write(e.RequestEvent, act, ActionCollectionUpdate, "", "import", nil,
				snapshot{"collections": names, "deleteMissing": e.DeleteMissing}, false)
			return nil
		},
	})

	// settings
	app.OnSettingsUpdateRequest().Bind(&hook.Handler[*core.SettingsUpdateRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.SettingsUpdateRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			var before snapshot
			if ok {
				before = objectSnapshot(e.OldSettings)
			}
			if err := e.Next(); err != nil || !ok {
				return err
			}
			l.write(e.RequestEvent, act, ActionSettingsUpdate, "", "", before, objectSnapshot(e.NewSettings), true)
			return nil
		},
	})

	// impersonation (POST /api/collections/{c}/impersonate/{id}); upstream
	// triggers OnRecordAuthRequest with an empty AuthMethod for it.
	app.OnRecordAuthRequest().Bind(&hook.Handler[*core.RecordAuthRequestEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *core.RecordAuthRequestEvent) error {
			act, ok := actorOf(e.RequestEvent)
			if err := e.Next(); err != nil || !ok {
				return err
			}
			if e.AuthMethod != "" || !strings.Contains(e.Request.URL.Path, "/impersonate/") {
				return nil
			}
			l.write(e.RequestEvent, act, ActionAuthImpersonate, e.Collection.Name, e.Record.Id, nil, nil, false)
			return nil
		},
	})

	// backups (no request context in the kernel hooks: actor is "system")
	app.OnBackupCreate().Bind(&hook.Handler[*kernel.BackupEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *kernel.BackupEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			l.writeSystem(ActionBackupCreate, e.Name)
			return nil
		},
	})
	app.OnBackupRestore().Bind(&hook.Handler[*kernel.BackupEvent]{
		Id: hookId, Priority: hookPriority,
		Func: func(e *kernel.BackupEvent) error {
			// logged BEFORE the restore: a successful restore restarts the
			// process and swaps the data directory (including auxiliary.db).
			l.writeSystem(ActionBackupRestore, e.Name)
			return e.Next()
		},
	})

	return l
}

type actor struct {
	kind, id, collection string
	impersonated         bool
}

// actorOf returns the request actor and whether the request is in capture
// scope (superuser auth, or an impersonated/non-refreshable session).
func actorOf(e *core.RequestEvent) (actor, bool) {
	if e == nil || e.Auth == nil {
		return actor{}, false
	}
	a := actor{kind: ActorUser, id: e.Auth.Id, collection: e.Auth.Collection().Name}
	if e.Auth.IsSuperuser() {
		a.kind = ActorSuperuser
	}
	if tok := bearer(e); tok != "" {
		if claims, err := security.ParseUnverifiedJWT(tok); err == nil {
			if v, ok := claims[core.TokenClaimRefreshable].(bool); ok && !v {
				a.impersonated = true
			}
		}
	}
	return a, a.kind == ActorSuperuser || a.impersonated
}

func bearer(e *core.RequestEvent) string {
	t := e.Request.Header.Get("Authorization")
	if len(t) > 7 && strings.EqualFold(t[:7], "Bearer ") {
		return t[7:]
	}
	return t
}

// write builds and appends an entry; failures are logged, never returned.
func (l *Log) write(e *core.RequestEvent, a actor, action, collection, record string, before, after snapshot, withDiff bool) {
	en := &Entry{
		ActorKind: a.kind, ActorID: a.id, ActorCollection: a.collection,
		Action: action, Collection: collection, Record: record,
		Before: finalize(before), After: finalize(after),
		Request: capJSON(map[string]any{
			"method": e.Request.Method, "path": e.Request.URL.Path,
			"ip": e.RealIP(), "user_agent": e.Request.UserAgent(),
		}),
	}
	if a.impersonated {
		unknown := "unknown" // static tokens carry no issuer identity
		en.ImpersonatedBy = &unknown
	}
	if withDiff {
		en.Diff = capJSON(diffOf(before, after))
	}
	l.append(en)
}

func (l *Log) writeSystem(action, name string) {
	l.append(&Entry{
		ActorKind: ActorSystem, Action: action, Record: name,
	})
}

func (l *Log) append(en *Entry) {
	if err := l.Append(en); err != nil {
		l.app.Logger().Error("audit: failed to write entry", "action", en.Action, "error", err)
	}
}
