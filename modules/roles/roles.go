//go:build !no_roles

// Package roles adds named roles and scoped memberships on top of the
// collection rules. Grants live in the system collections `_roles` and
// `_memberships` (superusers only) and are consulted from rules through the
// functions @role() and @member() (see rulefn.go) or from Go through [Has].
package roles

import (
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/types"
)

const (
	// RolesName and MembershipsName are the system collections of the module.
	RolesName       = "_roles"
	MembershipsName = "_memberships"

	hookId = "__tokiRoles__"
	// cacheTTL bounds the staleness of [Has] for changes made by another
	// process. Changes made through this process invalidate the cache at once.
	cacheTTL = 5 * time.Second
	// maxCacheEntries bounds the memory of the per-auth-record cache.
	maxCacheEntries = 10000
)

// Membership is one grant of a role to an auth record.
type Membership struct {
	Role    string `db:"role"`
	Scope   string `db:"scope"`
	Expires string `db:"expires"`
}

type cacheEntry struct {
	loaded time.Time
	rows   []Membership
}

// Module holds the per-auth-record membership cache.
type Module struct {
	app core.App
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cacheEntry
}

var (
	globalMu sync.RWMutex
	global   *Module
)

// Register creates the system collections (if needed), binds the hooks and returns the module.
func Register(app core.App) *Module {
	m := &Module{app: app, now: time.Now, cache: map[string]cacheEntry{}}
	globalMu.Lock()
	global = m
	globalMu.Unlock()

	ensure := func() {
		if err := EnsureCollections(app); err != nil {
			app.Logger().Error("roles: failed to initialize collections", "error", err)
		}
		m.Invalidate()
	}
	if app.IsBootstrapped() {
		ensure()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			ensure()
			return nil
		},
	})

	inval := func(e *core.RecordEvent) error {
		err := e.Next()
		m.Invalidate()
		return err
	}
	for _, n := range []string{RolesName, MembershipsName} {
		app.OnRecordAfterCreateSuccess(n).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
		app.OnRecordAfterUpdateSuccess(n).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
		app.OnRecordAfterDeleteSuccess(n).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
	}

	// store collection ids, not names, so a collection rename does not orphan grants
	app.OnRecordValidate(MembershipsName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.RecordEvent) error {
			for _, f := range []struct {
				name     string
				authOnly bool
			}{{"user_collection", true}, {"scope_collection", false}} {
				v := strings.TrimSpace(e.Record.GetString(f.name))
				if v == "" {
					continue
				}
				col, err := e.App.FindCachedCollectionByNameOrId(v)
				if err != nil || col == nil {
					return validation.Errors{f.name: validation.NewError("validation_unknown_collection", "Unknown collection.")}
				}
				if f.authOnly && !col.IsAuth() {
					return validation.Errors{f.name: validation.NewError("validation_not_auth_collection", "Must be an auth collection.")}
				}
				e.Record.Set(f.name, col.Id)
			}
			return e.Next()
		},
	})

	// deleting an auth record (or a scope record) removes its memberships
	app.OnRecordAfterDeleteSuccess().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId,
		Func: func(e *core.RecordEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			if n := e.Record.Collection().Name; n == MembershipsName || n == RolesName {
				return nil
			}
			return cascade(e.App, e.Record, m)
		},
	})
	return m
}

// cascade deletes the memberships held by rec (when it is an auth record) or scoped to rec.
func cascade(app core.App, rec *core.Record, m *Module) error {
	col := rec.Collection()
	ids := []string{}
	q := app.DB().NewQuery("SELECT id FROM {{" + MembershipsName + "}} WHERE (scope_collection = {:c} AND scope = {:i})" +
		func() string {
			if col.IsAuth() {
				return " OR (user_collection = {:c} AND user = {:i})"
			}
			return ""
		}())
	rows := []struct {
		Id string `db:"id"`
	}{}
	if err := q.Bind(map[string]any{"c": col.Id, "i": rec.Id}).All(&rows); err != nil {
		return err
	}
	for _, r := range rows {
		ids = append(ids, r.Id)
	}
	if len(ids) == 0 {
		return nil
	}
	mc, err := app.FindCachedCollectionByNameOrId(MembershipsName)
	if err != nil {
		return err
	}
	for _, id := range ids {
		mr, err := app.FindRecordById(mc, id)
		if err != nil {
			continue
		}
		if err := app.Delete(mr); err != nil {
			return err
		}
	}
	m.Invalidate()
	return nil
}

// EnsureCollections creates `_roles` and `_memberships` when missing.
func EnsureCollections(app core.App) error {
	roles, _ := app.FindCollectionByNameOrId(RolesName)
	if roles == nil {
		roles = core.NewBaseCollection(RolesName)
		roles.System = true
		roles.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 100},
			&core.TextField{Name: "description", Max: 500},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		roles.AddIndex("idx_roles_name", true, "[[name]]", "")
		if err := app.Save(roles); err != nil {
			return err
		}
	}
	if c, _ := app.FindCollectionByNameOrId(MembershipsName); c != nil {
		return nil
	}
	c := core.NewBaseCollection(MembershipsName)
	c.System = true
	c.Fields.Add(
		&core.TextField{Name: "user_collection", Required: true},
		&core.TextField{Name: "user", Required: true},
		&core.RelationField{Name: "role", Required: true, CollectionId: roles.Id, CascadeDelete: true, MaxSelect: 1},
		&core.TextField{Name: "scope"},
		&core.TextField{Name: "scope_collection"},
		&core.DateField{Name: "expires"},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_memberships_unique", true, "[[user_collection]], [[user]], [[role]], [[scope]]", "")
	c.AddIndex("idx_memberships_scope", false, "[[scope_collection]], [[scope]]", "")
	c.AddIndex("idx_memberships_role", false, "[[role]]", "")
	return app.Save(c)
}

// Invalidate drops the cache; the next lookup reloads it.
func (m *Module) Invalidate() {
	m.mu.Lock()
	m.cache = map[string]cacheEntry{}
	m.mu.Unlock()
}

func (m *Module) memberships(app core.App, colId, userId string) ([]Membership, error) {
	key := colId + "\x00" + userId
	m.mu.Lock()
	if e, ok := m.cache[key]; ok && m.now().Sub(e.loaded) < cacheTTL {
		m.mu.Unlock()
		return e.rows, nil
	}
	m.mu.Unlock()

	rows := []Membership{}
	err := app.DB().NewQuery("SELECT r.name AS role, m.scope AS scope, m.expires AS expires FROM {{" + MembershipsName +
		"}} AS m INNER JOIN {{" + RolesName + "}} AS r ON r.id = m.role WHERE m.user_collection = {:c} AND m.user = {:u}").
		Bind(map[string]any{"c": colId, "u": userId}).All(&rows)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if len(m.cache) >= maxCacheEntries {
		m.cache = map[string]cacheEntry{}
	}
	m.cache[key] = cacheEntry{loaded: m.now(), rows: rows}
	m.mu.Unlock()
	return rows, nil
}

func active(expires string, now time.Time) bool {
	if expires == "" {
		return true
	}
	dt, err := types.ParseDateTime(expires)
	if err != nil {
		return false // unparsable expiry fails closed
	}
	return dt.Time().After(now)
}

func lookup(app core.App, auth *core.Record) ([]Membership, time.Time, bool) {
	if auth == nil || auth.Collection() == nil || auth.Id == "" {
		return nil, time.Time{}, false
	}
	globalMu.RLock()
	m := global
	globalMu.RUnlock()
	if m == nil || m.app != app {
		m = &Module{app: app, now: time.Now, cache: map[string]cacheEntry{}}
	}
	rows, err := m.memberships(app, auth.Collection().Id, auth.Id)
	if err != nil {
		app.Logger().Warn("roles: failed to load memberships", "error", err)
		return nil, time.Time{}, false // fail closed
	}
	return rows, m.now(), true
}

// Has reports whether auth holds an unexpired membership with the role name
// and exactly the given scope (empty scope = a global grant). A nil auth
// (guest) never has a role. MCP agents are records of `_agents`.
func Has(app core.App, auth *core.Record, role, scope string) bool {
	rows, now, ok := lookup(app, auth)
	if !ok {
		return false
	}
	for _, r := range rows {
		if r.Role == role && r.Scope == scope && active(r.Expires, now) {
			return true
		}
	}
	return false
}

// IsMember reports whether auth holds any unexpired membership for scope.
func IsMember(app core.App, auth *core.Record, scope string) bool {
	if scope == "" {
		return false
	}
	rows, now, ok := lookup(app, auth)
	if !ok {
		return false
	}
	for _, r := range rows {
		if r.Scope == scope && active(r.Expires, now) {
			return true
		}
	}
	return false
}

func dbxHashRole(roleId string) dbx.Expression { return dbx.HashExp{"role": roleId} }

func dbxMember(userCol, user, role, scope string) dbx.Expression {
	return dbx.HashExp{"user_collection": userCol, "user": user, "role": role, "scope": scope}
}
