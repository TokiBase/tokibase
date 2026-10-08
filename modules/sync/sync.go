//go:build !no_sync

// Package sync is the phase 3 hub/spoke replication module. This part
// (PR1) contains change capture only: HLC, the `_changes`, `_sync_meta`,
// `_sync_tombstones` and `_sync_state` tables, and the capture hooks. There is
// no network code yet. See docs/SYNC_DESIGN.md and docs/modules/sync.md.
package sync

import (
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/tools/hook"
)

// Role is the sync role of the process.
type Role string

// Roles. The default is off.
const (
	RoleOff   Role = "off"
	RoleHub   Role = "hub"
	RoleSpoke Role = "spoke"
)

// EnvRole selects the role: off (default), hub or spoke.
const EnvRole = "TOKI_SYNC_ROLE"

const hookId = "__tokiSync__"

// capturePriority makes the capture handlers outer than every other
// Execute handler (crypto binds at 0), so they read the stored values.
const capturePriority = -1 << 19

// RoleFromEnv parses TOKI_SYNC_ROLE. Unknown values count as off.
func RoleFromEnv() Role {
	switch Role(strings.ToLower(strings.TrimSpace(os.Getenv(EnvRole)))) {
	case RoleHub:
		return RoleHub
	case RoleSpoke:
		return RoleSpoke
	}
	return RoleOff
}

// Enabled reports whether the process has a sync role other than off.
func Enabled() bool { return RoleFromEnv() != RoleOff }

// Module is the registered sync module (nil when the role is off).
type Module struct {
	app  core.App
	role Role

	// now is the wall clock of the HLC (tests inject it).
	now func() time.Time

	ready  atomic.Bool
	nodeID atomic.Value // string
	clock  atomic.Pointer[hlc.Clock]

	pol policyCache

	// stash maps the record of a client request to its actor (see actor.go).
	stash stdsync.Map // *core.Record -> string
	// txs holds the tx group state per open transaction.
	txs stdsync.Map // *kernel.TxAppInfo -> *txState
}

// Register binds the module according to TOKI_SYNC_ROLE. With the role off it
// binds nothing and returns nil (only the module marker exists).
func Register(app core.App) *Module { return RegisterRole(app, RoleFromEnv()) }

// RegisterRole is Register with an explicit role.
func RegisterRole(app core.App, role Role) *Module {
	if role != RoleHub && role != RoleSpoke {
		return nil
	}
	m := &Module{app: app, role: role, now: time.Now}
	m.pol.m = m

	init := func() error {
		err := m.Init()
		if err != nil {
			app.Logger().Error("sync: failed to initialize", "error", err)
		}
		return err
	}
	if app.IsBootstrapped() {
		_ = init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: -1,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			// fail closed: a node that cannot capture must not accept writes
			return init()
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId,
		Func: func(e *core.TerminateEvent) error {
			m.persistFloorOnStop()
			return e.Next()
		},
	})
	m.bindCapture()
	m.pol.bind()
	return m
}

// Role returns the configured role.
func (m *Module) Role() Role { return m.role }

// NodeID returns the id of this node ("" before Init).
func (m *Module) NodeID() string {
	s, _ := m.nodeID.Load().(string)
	return s
}

// Clock returns the HLC clock (nil before Init).
func (m *Module) Clock() *hlc.Clock { return m.clock.Load() }

// Init creates the tables and collections (all IF NOT EXISTS), loads the node
// id and boots the clock. It is idempotent.
func (m *Module) Init() error {
	if err := ensureSchema(m.app); err != nil {
		return err
	}
	if err := EnsurePolicyCollection(m.app); err != nil {
		return err
	}
	st := dbState{db: m.app.NonconcurrentDB()}
	id, ok, err := st.Get(keyNodeID)
	if err != nil {
		return err
	}
	if !ok || id == "" {
		// placeholder identity; the key-derived node id arrives with PR2
		id = randomNodeID()
		if err := st.Set(keyNodeID, id); err != nil {
			return err
		}
	}
	m.nodeID.Store(id)

	var maxHLC int64
	if err := m.app.DB().NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _changes").Row(&maxHLC); err != nil {
		return err
	}
	start, err := hlc.Boot(st, hlc.HLC(maxHLC))
	if err != nil {
		return err
	}
	c := hlc.NewClock(m.now, start)
	m.clock.Store(c)
	m.ready.Store(true)
	m.pol.invalidate()
	return nil
}

// persistFloorOnStop writes the clock floor outside of any record transaction.
func (m *Module) persistFloorOnStop() {
	c := m.Clock()
	if c == nil || !m.ready.Load() {
		return
	}
	if err := hlc.SaveFloor(dbState{db: m.app.NonconcurrentDB()}, c.Last()); err != nil {
		m.app.Logger().Warn("sync: failed to persist the hlc floor", "error", err)
	}
}

func randomNodeID() string {
	var b [9]byte
	_, _ = rand.Read(b[:])
	return "n" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:14]
}

// dbState implements hlc.Store on `_sync_state`.
type dbState struct{ db dbx.Builder }

const (
	keyNodeID        = "node_id"
	keySchemaVersion = "schema_version"
)

func (s dbState) Get(key string) (string, bool, error) {
	var v string
	err := s.db.NewQuery("SELECT value FROM _sync_state WHERE key={:k}").Bind(dbx.Params{"k": key}).Row(&v)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (s dbState) Set(key, value string) error {
	_, err := s.db.NewQuery("INSERT INTO _sync_state (key, value) VALUES ({:k}, {:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
		Bind(dbx.Params{"k": key, "v": value}).Execute()
	return err
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

var _ hlc.Store = dbState{}

func errf(format string, a ...any) error { return fmt.Errorf("sync: "+format, a...) }
