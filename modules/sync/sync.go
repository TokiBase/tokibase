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
	"path/filepath"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
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

// ParseRole parses a role name: "" and "off", "hub", "spoke" (case and
// surrounding space ignored). Any other value is an error, so that a typo
// does not silently run a node without sync.
func ParseRole(s string) (Role, error) {
	switch Role(strings.ToLower(strings.TrimSpace(s))) {
	case "", RoleOff:
		return RoleOff, nil
	case RoleHub:
		return RoleHub, nil
	case RoleSpoke:
		return RoleSpoke, nil
	}
	return RoleOff, fmt.Errorf("sync: invalid %s=%q (want off, hub or spoke)", EnvRole, s)
}

// RoleFromEnv parses TOKI_SYNC_ROLE for read-only callers (status). An unknown
// value reports off here; Register / RegisterFromEnv refuse to start instead.
func RoleFromEnv() Role {
	r, _ := ParseRole(os.Getenv(EnvRole))
	return r
}

// Enabled reports whether the process has a sync role other than off.
func Enabled() bool { return RoleFromEnv() != RoleOff }

// errBox boxes an error for atomic.Pointer.
type errBox struct{ err error }

// Module is the registered sync module (nil when the role is off).
type Module struct {
	app  core.App
	role Role

	// now is the wall clock of the HLC (tests inject it).
	now func() time.Time

	ready atomic.Bool
	// initErr is set when Init failed; capture then refuses writes to
	// capturable collections instead of letting them through uncaptured.
	initErr atomic.Pointer[errBox]
	nodeID  atomic.Value // string
	clock   atomic.Pointer[hlc.Clock]

	pol policyCache

	// hub is the hub key material (role hub), spoke the node keys (role spoke).
	hub   *hubIdentity
	spoke *proto.Identity
	// nonces guards the signed handshake against replays (hub).
	nonces nonceCache

	// stash maps the record of a client request to its actor (see actor.go).
	stash stdsync.Map // *core.Record -> string
	// txs holds the tx group state per open transaction.
	txs stdsync.Map // *kernel.TxAppInfo -> *txState
}

// Register binds the module according to TOKI_SYNC_ROLE. With the role off it
// binds nothing and returns nil (only the module marker exists).
//
// An unknown TOKI_SYNC_ROLE value panics at startup (fail closed: a mistyped
// hub must not run without sync); use RegisterFromEnv to get the error.
func Register(app core.App) *Module {
	m, err := RegisterFromEnv(app)
	if err != nil {
		panic(err)
	}
	return m
}

// RegisterFromEnv is Register returning an error for an invalid TOKI_SYNC_ROLE.
func RegisterFromEnv(app core.App) (*Module, error) {
	role, err := ParseRole(os.Getenv(EnvRole))
	if err != nil {
		return nil, err
	}
	return RegisterRole(app, role), nil
}

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
			m.initErr.Store(&errBox{err})
			app.Logger().Error("sync: failed to initialize, writes to capturable collections are refused", "error", err)
		} else {
			m.initErr.Store(nil)
		}
		return err
	}
	if app.IsBootstrapped() {
		_ = init() // fail closed through m.initErr: capture refuses writes
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
	m.bindRoutes()
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
		// placeholder identity until the key-derived id below replaces it
		id = randomNodeID()
		if err := st.Set(keyNodeID, id); err != nil {
			return err
		}
	}
	m.nodeID.Store(id)

	switch m.role {
	case RoleHub:
		if err := EnsureNodesCollection(m.app); err != nil {
			return err
		}
		h, err := loadHubIdentity(st)
		if err != nil {
			return err
		}
		m.hub = h
		// the hub's own writes are attributed to the hub id
		if err := m.adoptNodeID(h.id); err != nil {
			return err
		}
	case RoleSpoke:
		key, err := proto.LoadOrCreateIdentity(filepath.Join(m.app.DataDir(), NodeKeyFile), os.Getenv(EnvNodeKey))
		if err != nil {
			return err
		}
		m.spoke = key
		if err := m.adoptNodeID(key.NodeID()); err != nil {
			return err
		}
	}

	var maxHLC, maxMeta int64
	if err := m.app.DB().NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _changes").Row(&maxHLC); err != nil {
		return err
	}
	// HLCs observed from remote nodes live only in _sync_meta (replica applies
	// write no _changes row): the clock must not restart below them
	if err := m.app.DB().NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _sync_meta").Row(&maxMeta); err != nil {
		return err
	}
	start, err := hlc.Boot(st, hlc.HLC(maxHLC), hlc.HLC(maxMeta))
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
