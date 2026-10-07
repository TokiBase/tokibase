// Package crypto adds per-field encryption at rest (AES-256-GCM, envelope
// keys) without touching the collection JSON schema: the configuration lives
// in the system collection `_crypto_fields`, the wrapped data keys in
// `_crypto_keys` and the blind index in the table `_crypto_index`.
//
// See docs/modules/crypto.md.
package crypto

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	// FieldsCollection stores which fields are encrypted.
	FieldsCollection = "_crypto_fields"
	// KeysCollection stores the wrapped data keys.
	KeysCollection = "_crypto_keys"
	// IndexTable is the blind index (a plain table in the main database).
	IndexTable = "_crypto_index"

	ModeRandom     = "random"
	ModeBlindIndex = "blind-index"

	EnvMasterKey      = "TOKI_CRYPTO_MASTER_KEY"
	EnvMasterKeyFile  = "TOKI_CRYPTO_MASTER_KEY_FILE"
	EnvAdminPlaintext = "TOKI_CRYPTO_ADMIN_PLAINTEXT"

	// ErrCode is the validation error of a filter/sort on an encrypted field.
	ErrCode = "validation_encrypted_field"

	ActionEnable        = "crypto.enable"
	ActionDisable       = "crypto.disable"
	ActionRotate        = "crypto.rotate"
	ActionRetire        = "crypto.retire"
	ActionDecryptFailed = "crypto.decrypt_failed"

	hookId   = "__tokiCrypto__"
	storeKey = "__tokiCryptoModule__"

	cacheTTL = 5 * time.Second
	failTTL  = 5 * time.Second
)

// ErrNoMasterKey is returned when no master key is configured.
var ErrNoMasterKey = errors.New("crypto: no master key (set " + EnvMasterKey + " or " + EnvMasterKeyFile + ")")

var (
	sinkMu     sync.RWMutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects enable/disable/rotate/retire and decryption failures
// to an external audit log (wired in tokibase.go).
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func emit(action, collection, record string, details map[string]any) {
	sinkMu.RLock()
	fn := globalSink
	sinkMu.RUnlock()
	if fn != nil {
		fn(action, collection, record, details)
	}
}

// AdminPlaintext reports whether superusers get decrypted values from the API
// (default). TOKI_CRYPTO_ADMIN_PLAINTEXT=off makes them see ciphertext.
func AdminPlaintext() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv(EnvAdminPlaintext)), "off")
}

// LoadMasterKey reads the 32 byte master key from the environment.
func LoadMasterKey() ([]byte, error) {
	v := strings.TrimSpace(os.Getenv(EnvMasterKey))
	if v == "" {
		if f := strings.TrimSpace(os.Getenv(EnvMasterKeyFile)); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("crypto: cannot read %s: %w", EnvMasterKeyFile, err)
			}
			v = strings.TrimSpace(string(b))
		}
	}
	if v == "" {
		return nil, ErrNoMasterKey
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(v); err == nil {
			if len(b) != 32 {
				return nil, fmt.Errorf("crypto: master key must be 32 bytes, got %d", len(b))
			}
			return b, nil
		}
	}
	return nil, errors.New("crypto: master key is not valid base64")
}

// Module holds the cached configuration and keys.
type Module struct {
	app       core.App
	master    []byte
	masterErr error

	mu         sync.RWMutex
	cfg        map[string]map[string]string // collection id -> field -> mode
	loaded     time.Time
	invalid    bool
	gen        uint64
	loadedOnce bool
	failUntil  time.Time
	loadMu     sync.Mutex
	load       func() (map[string]map[string]string, error)

	keyMu sync.Mutex
	keys  map[string]*collKeys

	auditMu   sync.Mutex
	auditLast map[string]time.Time
}

// From returns the module registered on app, or nil.
func From(app kernel.App) *Module {
	m, _ := app.Store().Get(storeKey).(*Module)
	return m
}

// Active reports whether a master key is loaded.
func (m *Module) Active() bool { return len(m.master) == 32 }

// Register creates the collections and table, binds the hooks and returns the module.
func Register(app core.App) *Module {
	m := &Module{app: app, invalid: true, keys: map[string]*collKeys{}, auditLast: map[string]time.Time{}}
	m.master, m.masterErr = LoadMasterKey()
	m.load = m.loadConfig
	app.Store().Set(storeKey, m)

	ensure := func() {
		if err := EnsureSchema(app); err != nil {
			app.Logger().Error("crypto: failed to initialize schema", "error", err)
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
			m.bootWarn()
			return nil
		},
	})

	inval := func(e *core.RecordEvent) error {
		err := e.Next()
		m.Invalidate()
		return err
	}
	for _, name := range []string{FieldsCollection, KeysCollection} {
		app.OnRecordAfterCreateSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
		app.OnRecordAfterUpdateSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
		app.OnRecordAfterDeleteSuccess(name).Bind(&hook.Handler[*core.RecordEvent]{Id: hookId, Func: inval})
	}

	m.bindHooks()
	m.registerJobs()
	return m
}

// EnsureSchema creates `_crypto_fields`, `_crypto_keys` and `_crypto_index`.
func EnsureSchema(app core.App) error {
	if c, _ := app.FindCollectionByNameOrId(FieldsCollection); c == nil {
		c = core.NewBaseCollection(FieldsCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "collection", Required: true},
			&core.TextField{Name: "field", Required: true},
			&core.SelectField{Name: "mode", Required: true, MaxSelect: 1, Values: []string{ModeRandom, ModeBlindIndex}},
			&core.AutodateField{Name: "created", OnCreate: true},
		)
		c.AddIndex("idx_crypto_fields_unique", true, "[[collection]], [[field]]", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if c, _ := app.FindCollectionByNameOrId(KeysCollection); c == nil {
		c = core.NewBaseCollection(KeysCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "collection", Required: true},
			&core.NumberField{Name: "version", Required: true, OnlyInt: true},
			&core.TextField{Name: "wrapped_dek", Max: 4096},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.DateField{Name: "retired_at"},
		)
		c.AddIndex("idx_crypto_keys_unique", true, "[[collection]], [[version]]", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS `" + IndexTable + "` (" +
			"`collection` TEXT NOT NULL, `field` TEXT NOT NULL, `record` TEXT NOT NULL, " +
			"`hmac` TEXT NOT NULL, `ver` INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (`collection`,`field`,`record`))",
		"CREATE INDEX IF NOT EXISTS `idx_crypto_index_hmac` ON `" + IndexTable + "` (`collection`,`field`,`hmac`)",
	} {
		if _, err := app.NonconcurrentDB().NewQuery(q).Execute(); err != nil {
			return err
		}
	}
	return nil
}

// Invalidate drops the caches; the next lookup reloads them.
func (m *Module) Invalidate() {
	m.mu.Lock()
	m.invalid = true
	m.failUntil = time.Time{}
	m.gen++
	m.mu.Unlock()
	m.keyMu.Lock()
	m.keys = map[string]*collKeys{}
	m.keyMu.Unlock()
}

// Config is one encrypted field.
type Config struct {
	Id         string `json:"id,omitempty"`
	Collection string `json:"collection"` // collection name
	CollId     string `json:"collection_id"`
	Field      string `json:"field"`
	Mode       string `json:"mode"`
}

func (m *Module) loadConfig() (map[string]map[string]string, error) {
	recs, err := m.app.FindAllRecords(FieldsCollection)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	for _, r := range recs {
		id := m.resolveId(r.GetString("collection"))
		if out[id] == nil {
			out[id] = map[string]string{}
		}
		out[id][r.GetString("field")] = r.GetString("mode")
	}
	return out, nil
}

// resolveId maps a stored collection reference (id or name) to the id.
func (m *Module) resolveId(ref string) string {
	if c, err := m.app.FindCachedCollectionByNameOrId(ref); err == nil && c != nil {
		return c.Id
	}
	return ref
}

// fieldsFor returns field -> mode of a collection. err is set when the
// configuration could never be loaded (callers must fail closed on writes).
func (m *Module) fieldsFor(collId string) (map[string]string, error) {
	m.mu.RLock()
	if m.fresh() {
		r := m.cfg[collId]
		m.mu.RUnlock()
		return r, nil
	}
	if time.Now().Before(m.failUntil) {
		r, once := m.cfg[collId], m.loadedOnce
		m.mu.RUnlock()
		if !once {
			return nil, errors.New("crypto: configuration unavailable")
		}
		return r, nil
	}
	m.mu.RUnlock()

	m.loadMu.Lock()
	defer m.loadMu.Unlock()
	m.mu.RLock()
	if m.fresh() {
		r := m.cfg[collId]
		m.mu.RUnlock()
		return r, nil
	}
	gen := m.gen
	m.mu.RUnlock()

	cfg, err := m.load()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.failUntil = time.Now().Add(failTTL)
		m.app.Logger().Warn("crypto: failed to load configuration", "error", err)
		if !m.loadedOnce {
			return nil, errors.New("crypto: configuration unavailable")
		}
		return m.cfg[collId], nil
	}
	m.cfg, m.loaded, m.loadedOnce, m.failUntil = cfg, time.Now(), true, time.Time{}
	m.invalid = m.gen != gen
	return m.cfg[collId], nil
}

func (m *Module) fresh() bool { return !m.invalid && time.Since(m.loaded) < cacheTTL }

// List returns every configured field, sorted.
func List(app core.App) ([]Config, error) {
	recs, err := app.FindAllRecords(FieldsCollection)
	if err != nil {
		return nil, err
	}
	out := []Config{}
	for _, r := range recs {
		c := Config{Id: r.Id, Collection: r.GetString("collection"), Field: r.GetString("field"), Mode: r.GetString("mode")}
		if col, err := app.FindCachedCollectionByNameOrId(c.Collection); err == nil && col != nil {
			c.Collection, c.CollId = col.Name, col.Id
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Collection != out[j].Collection {
			return out[i].Collection < out[j].Collection
		}
		return out[i].Field < out[j].Field
	})
	return out, nil
}

func (m *Module) configRecord(collId, field string) (*core.Record, error) {
	return m.app.FindFirstRecordByFilter(FieldsCollection, "collection={:c} && field={:f}", dbx.Params{"c": collId, "f": field})
}
