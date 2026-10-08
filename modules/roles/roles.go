//go:build !no_roles

// Package roles adds named roles and scoped memberships on top of the
// collection rules. Grants live in the system collections `_roles` and
// `_memberships` (superusers only) and are consulted from rules through the
// functions @role() and @member() (see rulefn.go) or from Go through [Has].
//
// Holders are records of any auth collection and of the MCP `_agents`
// collection. A scoped grant is matched on the scope record id and on its
// collection (ids are unique only per collection).
package roles

import (
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
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
	Role            string `db:"role"`
	Scope           string `db:"scope"`
	ScopeCollection string `db:"scope_collection"` // collection id
	Expires         string `db:"expires"`
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
	gen   uint64 // bumped by Invalidate; a load started before a bump is not cached
}

// agentsCollection is the MCP agents collection (modules/mcp); its records may hold roles.
const agentsCollection = "_agents"

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

	// role names are matched exactly: refuse look-alikes with surrounding whitespace
	app.OnRecordValidate(RolesName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.RecordEvent) error {
			name := e.Record.GetString("name")
			if name != strings.TrimSpace(name) || strings.ContainsAny(name, "\"'\\\r\n\t") {
				return validation.Errors{"name": validation.NewError("validation_invalid_name", "Must not have surrounding whitespace or contain quotes, backslashes or control characters.")}
			}
			return e.Next()
		},
	})

	// store collection ids, not names, so a collection rename does not orphan grants
	app.OnRecordValidate(MembershipsName).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.RecordEvent) error {
			var userCol *core.Collection
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
				if f.authOnly && !col.IsAuth() && col.Name != agentsCollection {
					return validation.Errors{f.name: validation.NewError("validation_not_auth_collection", "Must be an auth collection or _agents.")}
				}
				if f.authOnly {
					userCol = col
				}
				e.Record.Set(f.name, col.Id)
			}
			if userCol != nil {
				if _, err := e.App.FindRecordById(userCol, e.Record.GetString("user")); err != nil {
					return validation.Errors{"user": validation.NewError("validation_unknown_user", "Unknown record in user_collection.")}
				}
			}
			scope := strings.TrimSpace(e.Record.GetString("scope"))
			e.Record.Set("scope", scope)
			switch {
			case scope != "" && e.Record.GetString("scope_collection") == "":
				return validation.Errors{"scope_collection": validation.NewError("validation_required", "Required when scope is set (ids are only unique per collection).")}
			case scope == "":
				e.Record.Set("scope_collection", "")
			}
			return e.Next()
		},
	})

	// Deleting an auth record (or a scope record) removes its memberships inside
	// the deleting transaction: if either step fails both roll back.
	app.OnRecordDelete().Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId,
		Func: func(e *core.RecordEvent) error {
			if n := e.Record.Collection().Name; n == MembershipsName || n == RolesName {
				return e.Next()
			}
			originalApp := e.App
			txErr := e.App.RunInTransaction(func(txApp kernel.App) error {
				e.App = txApp
				if err := cascade(txApp, e.Record); err != nil {
					return err
				}
				return e.Next()
			})
			e.App = originalApp
			m.Invalidate()
			return txErr
		},
	})

	// deleting a whole collection drops the memberships it holds or is scope of
	app.OnCollectionDelete().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: hookId,
		Func: func(e *core.CollectionEvent) error {
			if n := e.Collection.Name; n == MembershipsName || n == RolesName {
				return e.Next()
			}
			originalApp := e.App
			txErr := e.App.RunInTransaction(func(txApp kernel.App) error {
				e.App = txApp
				if _, err := txApp.DB().NewQuery("DELETE FROM {{" + MembershipsName + "}} WHERE user_collection = {:c} OR scope_collection = {:c}").
					Bind(map[string]any{"c": e.Collection.Id}).Execute(); err != nil {
					return err
				}
				return e.Next()
			})
			e.App = originalApp
			m.Invalidate()
			return txErr
		},
	})
	return m
}

// cascade deletes the memberships held by rec (when it is an auth record or an
// agent) or scoped to rec. app should be the transactional app of the delete.
func cascade(app kernel.App, rec *core.Record) error {
	col := rec.Collection()
	sql := "SELECT id FROM {{" + MembershipsName + "}} WHERE (scope_collection = {:c} AND scope = {:i})"
	if col.IsAuth() || col.Name == agentsCollection {
		sql += " OR (user_collection = {:c} AND user = {:i})"
	}
	rows := []struct {
		Id string `db:"id"`
	}{}
	if err := app.DB().NewQuery(sql).Bind(map[string]any{"c": col.Id, "i": rec.Id}).All(&rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	mc, err := app.FindCachedCollectionByNameOrId(MembershipsName)
	if err != nil {
		return err
	}
	for _, r := range rows {
		mr, err := app.FindRecordById(mc, r.Id)
		if err != nil {
			return err
		}
		if err := app.Delete(mr); err != nil {
			return err
		}
	}
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
	m.gen++
	m.mu.Unlock()
}

func (m *Module) memberships(app core.App, colId, userId string) ([]Membership, error) {
	key := colId + "\x00" + userId
	m.mu.Lock()
	if e, ok := m.cache[key]; ok && m.now().Sub(e.loaded) < cacheTTL {
		m.mu.Unlock()
		return e.rows, nil
	}
	gen := m.gen
	m.mu.Unlock()

	rows := []Membership{}
	err := app.DB().NewQuery("SELECT r.name AS role, m.scope AS scope, m.scope_collection AS scope_collection, m.expires AS expires FROM {{" + MembershipsName +
		"}} AS m INNER JOIN {{" + RolesName + "}} AS r ON r.id = m.role WHERE m.user_collection = {:c} AND m.user = {:u}").
		Bind(map[string]any{"c": colId, "u": userId}).All(&rows)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.gen == gen { // an Invalidate during the read means these rows may be stale
		if len(m.cache) >= maxCacheEntries {
			m.cache = map[string]cacheEntry{}
		}
		m.cache[key] = cacheEntry{loaded: m.now(), rows: rows}
	}
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

// scopeColId resolves a collection name or id to its id ("" when unknown).
func scopeColId(app core.App, nameOrId string) string {
	if nameOrId == "" {
		return ""
	}
	c, err := app.FindCachedCollectionByNameOrId(nameOrId)
	if err != nil || c == nil {
		return ""
	}
	return c.Id
}

// Has reports whether auth holds an unexpired membership with the role name
// and exactly the given scope record. A scoped grant is matched on the scope id
// and on scopeCollection (name or id; an unknown collection never matches).
// An empty scope means a global grant and ignores scopeCollection. This
// differs from the SQL function, where an empty scope operand is always false.
// A nil auth (guest) never has a role. MCP agents are records of `_agents`.
func Has(app core.App, auth *core.Record, role, scope, scopeCollection string) bool {
	rows, now, ok := lookup(app, auth)
	if !ok {
		return false
	}
	sc := ""
	if scope != "" {
		if sc = scopeColId(app, scopeCollection); sc == "" {
			return false
		}
	}
	for _, r := range rows {
		if r.Role == role && r.Scope == scope && (scope == "" || r.ScopeCollection == sc) && active(r.Expires, now) {
			return true
		}
	}
	return false
}

// IsMember reports whether auth holds any unexpired membership for the scope
// record (id and collection, see [Has]). An empty scope is never a membership.
func IsMember(app core.App, auth *core.Record, scope, scopeCollection string) bool {
	if scope == "" {
		return false
	}
	sc := scopeColId(app, scopeCollection)
	if sc == "" {
		return false
	}
	rows, now, ok := lookup(app, auth)
	if !ok {
		return false
	}
	for _, r := range rows {
		if r.Scope == scope && r.ScopeCollection == sc && active(r.Expires, now) {
			return true
		}
	}
	return false
}

func dbxHashRole(roleId string) dbx.Expression { return dbx.HashExp{"role": roleId} }

func dbxMember(userCol, user, role, scope string) dbx.Expression {
	return dbx.HashExp{"user_collection": userCol, "user": user, "role": role, "scope": scope}
}
