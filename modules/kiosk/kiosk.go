//go:build !no_kiosk

// Package kiosk pairs a locked-down browser (a Pi or mini PC in kiosk mode)
// with the local edge node: a one-time pairing code becomes a device cookie,
// the cookie is exchanged for a normal auth token of a low-privilege service
// actor, and kiosk.js shows connectivity and a PIN lock (docs/modules/kiosk.md).
package kiosk

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"golang.org/x/crypto/bcrypt"
)

const (
	hookId = "__tokiKiosk__"

	// Collection is the superuser-only device collection.
	Collection = "_kiosk_devices"
	// CookieName is the device cookie (HttpOnly, Path=/api/kiosk).
	CookieName = "toki_kiosk"

	// Audit actions.
	AuditPair       = "kiosk.pair"
	AuditUnlockFail = "kiosk.unlock_fail"

	timeLayout = "2006-01-02 15:04:05.000Z"

	defaultTTLHours = 12
	maxTTLHours     = 720
	pairingTTL      = 15 * time.Minute

	// Plain tables (no collections): the sids issued per device and the
	// per-device revocation generation carried by every token as `kiosk_gen`.
	SessionsTable = "_kiosk_sessions"
	GenTable      = "_kiosk_gen"
	claimDev      = "kiosk_dev"
	claimGen      = "kiosk_gen"

	// built-in per-IP throttle, independent of the PocketBase rate-limit rules
	throttleWindow = time.Minute
	pairPerWindow  = 20
	unlockPerWin   = 30
	maxThrottleIPs = 4096

	pinFailures  = 5
	pinLockBase  = 60 * time.Second
	pinLockMax   = time.Hour
	minPin       = 4
	maxPin       = 64
	touchEvery   = time.Minute
	maxDeviceCnt = 4096
)

// Enabled reports whether the module is switched on (TOKI_KIOSK=on).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_KIOSK"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

func allowRemote() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_KIOSK_ALLOW_REMOTE"))) {
	case "1", "on", "true", "yes":
		return true
	}
	return false
}

// defaultTTL is the session lifetime of a device without ttl_hours.
func defaultTTL() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_KIOSK_SESSION_HOURS"))); err == nil && n > 0 && n <= maxTTLHours {
		return n
	}
	return defaultTTLHours
}

var (
	sinkMu     sync.Mutex
	globalSink func(action, collection, record string, details map[string]any)
)

// SetAuditSink connects kiosk.pair and kiosk.unlock_fail to an external audit
// log. Modules must not import each other, so the wiring is in tokibase.go.
func SetAuditSink(fn func(action, collection, record string, details map[string]any)) {
	sinkMu.Lock()
	globalSink = fn
	sinkMu.Unlock()
}

func audit(action, record string, details map[string]any) {
	sinkMu.Lock()
	fn := globalSink
	sinkMu.Unlock()
	if fn != nil {
		fn(action, Collection, record, details)
	}
}

// HashToken returns the stored form (hex SHA-256) of a device token or pairing code.
func HashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// NewSecret returns 32 random bytes as unpadded base64url.
func NewSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the kernel cannot run without randomness
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		t, _ = time.Parse("2006-01-02 15:04:05Z", s)
	}
	return t
}

// Device is a row of _kiosk_devices.
type Device struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	AuthCollection string `json:"auth_collection"`
	AuthRecord     string `json:"auth_record"`
	NodeID         string `json:"node_id,omitempty"`
	BindIP         string `json:"bind_ip,omitempty"`
	Locked         bool   `json:"locked"`
	TTLHours       int    `json:"ttl_hours"`
	LockAfterS     int    `json:"lock_after_s"`
	HasPin         bool   `json:"has_pin"`
	Paired         bool   `json:"paired"`
	PairingPending bool   `json:"pairing_pending"`
	LastSeen       string `json:"last_seen,omitempty"`

	pinHash string
}

func deviceOf(r *core.Record) *Device {
	d := &Device{
		ID: r.Id, Name: r.GetString("name"),
		AuthCollection: r.GetString("auth_collection"), AuthRecord: r.GetString("auth_record"),
		NodeID: r.GetString("node_id"), BindIP: r.GetString("bind_ip"),
		Locked: r.GetBool("locked"), TTLHours: r.GetInt("ttl_hours"), LockAfterS: r.GetInt("lock_after_s"),
		pinHash: r.GetString("pin_hash"),
		Paired:  r.GetString("token_hash") != "",
	}
	d.HasPin = d.pinHash != ""
	d.PairingPending = r.GetString("pairing_hash") != "" && r.GetDateTime("pairing_expires").Time().After(time.Now())
	if ls := r.GetDateTime("last_seen"); !ls.IsZero() {
		d.LastSeen = ls.String()
	}
	if d.TTLHours <= 0 {
		d.TTLHours = defaultTTL()
	}
	return d
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks a device config (the actor is checked against the database
// by [validateActor]).
func (d *Device) Validate() error {
	if !nameRe.MatchString(d.Name) {
		return errors.New("kiosk: name must be 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	if d.AuthCollection == "" || d.AuthRecord == "" {
		return errors.New("kiosk: auth_collection and auth_record (the service actor) are required")
	}
	if d.AuthCollection == core.CollectionNameSuperusers {
		return errors.New("kiosk: a superuser cannot be the service actor of a kiosk")
	}
	if strings.HasPrefix(d.AuthCollection, "_") {
		return errors.New("kiosk: a system collection (_superusers, _agents, ...) cannot hold the service actor of a kiosk")
	}
	if d.TTLHours < 0 || d.TTLHours > maxTTLHours {
		return fmt.Errorf("kiosk: ttl_hours must be between 1 and %d", maxTTLHours)
	}
	if d.LockAfterS < 0 {
		return errors.New("kiosk: lock_after_s cannot be negative")
	}
	if d.BindIP != "" {
		if _, _, err := parseBind(d.BindIP); err != nil {
			return err
		}
	}
	return nil
}

// parseBind parses an IP or a CIDR.
func parseBind(s string) (netip.Addr, netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Addr{}, netip.Prefix{}, fmt.Errorf("kiosk: bind_ip %q is not an IP or CIDR", s)
		}
		return netip.Addr{}, p.Masked(), nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, netip.Prefix{}, fmt.Errorf("kiosk: bind_ip %q is not an IP or CIDR", s)
	}
	return a.Unmap(), netip.Prefix{}, nil
}

// bindOK reports whether ip satisfies the bind_ip of the device (always true when unset).
func (d *Device) bindOK(ip string) bool {
	if d.BindIP == "" {
		return true
	}
	a, p, err := parseBind(d.BindIP)
	if err != nil {
		return false
	}
	got, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	got = got.Unmap()
	if p.IsValid() {
		return p.Contains(got)
	}
	return a == got
}

// checkPin compares a PIN with the bcrypt hash.
func (d *Device) checkPin(pin string) bool {
	if d.pinHash == "" || len(pin) > maxPin {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(d.pinHash), []byte(pin)) == nil
}

// HashPin validates and hashes a PIN.
func HashPin(pin string) (string, error) {
	if len(pin) < minPin || len(pin) > maxPin {
		return "", fmt.Errorf("kiosk: the PIN must be %d-%d characters", minPin, maxPin)
	}
	b, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	return string(b), err
}

func equalHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

var ensureMu sync.Mutex

func floatPtr(f float64) *float64 { return &f }

const createTablesSQL = `CREATE TABLE IF NOT EXISTS {{` + SessionsTable + `}} (
	[[device]]  TEXT NOT NULL,
	[[sid]]     TEXT NOT NULL,
	[[issued]]  TEXT NOT NULL,
	[[expires]] TEXT NOT NULL,
	PRIMARY KEY ([[device]], [[sid]])
)`

const createGenSQL = `CREATE TABLE IF NOT EXISTS {{` + GenTable + `}} (
	[[device]] TEXT PRIMARY KEY NOT NULL,
	[[gen]]    INTEGER NOT NULL DEFAULT 0
)`

// deviceFields lists the fields added after the first release; ensureCollections
// adds the missing ones to an existing collection.
func deviceFields() []core.Field {
	return []core.Field{
		&core.NumberField{Name: "pin_fail_count", OnlyInt: true},
		&core.NumberField{Name: "pin_lockouts", OnlyInt: true},
		&core.DateField{Name: "pin_locked_until"},
	}
}

// ensureCollections creates _kiosk_devices (and the plain tables) when missing.
func ensureCollections(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if !app.HasTable("_collections") {
		return errors.New("kiosk: _collections table is not ready")
	}
	for _, q := range []string{createTablesSQL, createGenSQL} {
		if _, err := app.DB().NewQuery(q).Execute(); err != nil {
			return err
		}
	}
	if c, err := app.FindCachedCollectionByNameOrId(Collection); err == nil {
		missing := false
		for _, f := range deviceFields() {
			if c.Fields.GetByName(f.GetName()) == nil {
				missing = true
			}
		}
		if !missing {
			return nil
		}
		c, err = app.FindCollectionByNameOrId(Collection)
		if err != nil {
			return err
		}
		for _, f := range deviceFields() {
			if c.Fields.GetByName(f.GetName()) == nil {
				c.Fields.Add(f)
			}
		}
		return app.Save(c)
	}
	c := core.NewBaseCollection(Collection)
	c.System = true // rules stay nil: superuser only
	c.Fields.Add(
		&core.TextField{Name: "name", Required: true, Max: 64},
		&core.TextField{Name: "token_hash", Max: 64},
		&core.TextField{Name: "pairing_hash", Max: 64},
		&core.DateField{Name: "pairing_expires"},
		&core.TextField{Name: "auth_collection", Required: true, Max: 100},
		&core.TextField{Name: "auth_record", Required: true, Max: 100},
		&core.TextField{Name: "node_id", Max: 100},
		&core.TextField{Name: "bind_ip", Max: 64},
		&core.TextField{Name: "pin_hash", Max: 100},
		&core.BoolField{Name: "locked"},
		&core.NumberField{Name: "ttl_hours", OnlyInt: true, Min: floatPtr(0), Max: floatPtr(maxTTLHours)},
		&core.NumberField{Name: "lock_after_s", OnlyInt: true, Min: floatPtr(0)},
		&core.DateField{Name: "last_seen"},
	)
	c.Fields.Add(deviceFields()...)
	c.Fields.Add(
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_toki_kiosk_name", true, "name", "")
	c.AddIndex("idx_toki_kiosk_token", false, "token_hash", "")
	c.AddIndex("idx_toki_kiosk_pairing", false, "pairing_hash", "")
	return app.Save(c)
}

func actorChanged(r *core.Record) bool {
	o := r.Original()
	return o == nil || o.GetString("auth_collection") != r.GetString("auth_collection") || o.GetString("auth_record") != r.GetString("auth_record")
}

// validateActor checks that the service actor exists and is an auth record that is not a superuser.
func validateActor(app core.App, d *Device) error {
	if _, err := actorOf(app, d); err != nil {
		return err
	}
	return nil
}

func actorOf(app core.App, d *Device) (*core.Record, error) {
	col, err := app.FindCachedCollectionByNameOrId(d.AuthCollection)
	if err != nil || !col.IsAuth() || col.Name == core.CollectionNameSuperusers || col.System || strings.HasPrefix(col.Name, "_") {
		return nil, fmt.Errorf("kiosk: %q is not an auth collection usable as a service actor", d.AuthCollection)
	}
	rec, err := app.FindRecordById(col, d.AuthRecord)
	if err != nil {
		return nil, fmt.Errorf("kiosk: the service actor %s/%s does not exist", d.AuthCollection, d.AuthRecord)
	}
	return rec, nil
}

// Module is the registered kiosk module.
type Module struct {
	app core.App
	now func() time.Time

	mu    sync.Mutex
	touch map[string]time.Time // device id -> last last_seen write

	thMu     sync.Mutex
	throttle map[string]*window // tag|ip -> attempts in the current window

	pinMus sync.Map // device id -> *sync.Mutex (serializes check+compare+record)

	purgeAt time.Time
}

type window struct {
	start time.Time
	n     int
}

func newModule(app core.App) *Module {
	return &Module{
		app: app, now: func() time.Time { return time.Now().UTC() },
		touch: map[string]time.Time{}, throttle: map[string]*window{},
	}
}

// allow counts one attempt of tag from ip and reports whether it is within limit
// per window. Independent of the PocketBase rate-limit rules.
func (m *Module) allow(tag, ip string, limit int) bool {
	now := m.now()
	m.thMu.Lock()
	defer m.thMu.Unlock()
	k := tag + "|" + ip
	w := m.throttle[k]
	if w == nil || now.Sub(w.start) >= throttleWindow {
		if len(m.throttle) >= maxThrottleIPs {
			m.throttle = map[string]*window{}
		}
		w = &window{start: now}
		m.throttle[k] = w
	}
	w.n++
	return w.n <= limit
}

func (m *Module) deviceMu(id string) *sync.Mutex {
	v, _ := m.pinMus.LoadOrStore(id, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type brake struct {
	Fails    int    `db:"fails"`
	Lockouts int    `db:"lockouts"`
	Until    string `db:"until"`
}

func (m *Module) loadBrake(id string) brake {
	var b brake
	err := m.app.DB().NewQuery(`SELECT COALESCE([[pin_fail_count]],0) AS fails, COALESCE([[pin_lockouts]],0) AS lockouts,
		COALESCE([[pin_locked_until]],'') AS until FROM {{` + Collection + `}} WHERE [[id]]={:id}`).Bind(dbx.Params{"id": id}).One(&b)
	if err != nil {
		return brake{}
	}
	return b
}

// pinAllowed reports whether a PIN attempt may be made now and, when not, how long to wait.
// The caller holds the device mutex.
func (m *Module) pinAllowed(id string) (bool, time.Duration) {
	b := m.loadBrake(id)
	if until := parseTime(b.Until); !until.IsZero() {
		if now := m.now(); now.Before(until) {
			return false, until.Sub(now)
		}
	}
	return true, 0
}

// nextBrake counts a failure; the 5th starts a lockout of 60 s that doubles for
// each following lockout (up to one hour).
func nextBrake(fails, lockouts int, now time.Time) (nf, nl int, until time.Time, lock time.Duration) {
	fails++
	if fails < pinFailures {
		return fails, lockouts, time.Time{}, 0
	}
	d := min(pinLockBase<<min(lockouts, 6), pinLockMax)
	return 0, lockouts + 1, now.Add(d), d
}

// pinFailed persists a failure. It returns the failure count and the lockout
// started by this failure (0 when none). The caller holds the device mutex.
func (m *Module) pinFailed(id string) (int, time.Duration) {
	b := m.loadBrake(id)
	n := b.Fails + 1
	nf, nl, until, lock := nextBrake(b.Fails, b.Lockouts, m.now())
	us := ""
	if !until.IsZero() {
		us = fmtTime(until)
	} else if prev := parseTime(b.Until); !prev.IsZero() {
		us = fmtTime(prev)
	}
	if _, err := m.app.DB().NewQuery(`UPDATE {{` + Collection + `}} SET [[pin_fail_count]]={:f}, [[pin_lockouts]]={:l}, [[pin_locked_until]]={:u} WHERE [[id]]={:id}`).
		Bind(dbx.Params{"f": nf, "l": nl, "u": us, "id": id}).Execute(); err != nil {
		m.app.Logger().Error("kiosk: failed to persist the PIN brake", "error", err)
	}
	return n, lock
}

func (m *Module) pinOK(id string) {
	if _, err := m.app.DB().NewQuery(`UPDATE {{` + Collection + `}} SET [[pin_fail_count]]=0, [[pin_lockouts]]=0, [[pin_locked_until]]='' WHERE [[id]]={:id}`).
		Bind(dbx.Params{"id": id}).Execute(); err != nil {
		m.app.Logger().Error("kiosk: failed to reset the PIN brake", "error", err)
	}
}

// ---- durable revocation ---------------------------------------------------

// generation is the current kiosk_gen of a device (0 until the first revocation).
func generation(app core.App, device string) (int64, error) {
	var g int64
	err := app.DB().NewQuery(`SELECT COALESCE(MAX([[gen]]),0) FROM {{` + GenTable + `}} WHERE [[device]]={:d}`).
		Bind(dbx.Params{"d": device}).Row(&g)
	return g, err
}

func (m *Module) rememberSID(device, sid string, expires time.Time) {
	if sid == "" {
		return
	}
	now := m.now()
	if _, err := m.app.DB().NewQuery(`INSERT OR REPLACE INTO {{` + SessionsTable + `}} ([[device]],[[sid]],[[issued]],[[expires]]) VALUES ({:d},{:s},{:i},{:x})`).
		Bind(dbx.Params{"d": device, "s": sid, "i": fmtTime(now), "x": fmtTime(expires)}).Execute(); err != nil {
		m.app.Logger().Warn("kiosk: failed to record a session", "error", err)
		return
	}
	m.mu.Lock()
	purge := now.Sub(m.purgeAt) > time.Minute
	if purge {
		m.purgeAt = now
	}
	m.mu.Unlock()
	if purge { // sessions whose token already expired need no revocation
		_, _ = m.app.DB().NewQuery(`DELETE FROM {{` + SessionsTable + `}} WHERE [[expires]] < {:n}`).Bind(dbx.Params{"n": fmtTime(now)}).Execute()
	}
}

// RevokeDevice ends every token ever issued to a device: it bumps the device
// generation (tokens carrying an older kiosk_gen are refused by the request
// middleware, with or without modules/sessions) and revokes each recorded sid
// through kernel.RevokeSession. It returns how many sessions were revoked.
// Works when the service actor no longer exists.
func RevokeDevice(app core.App, device, reason string) (int, error) {
	if _, err := app.DB().NewQuery(`INSERT INTO {{` + GenTable + `}} ([[device]],[[gen]]) VALUES ({:d},1)
		ON CONFLICT([[device]]) DO UPDATE SET [[gen]]=[[gen]]+1`).Bind(dbx.Params{"d": device}).Execute(); err != nil {
		return 0, err
	}
	var sids []string
	if err := app.DB().NewQuery(`SELECT [[sid]] FROM {{` + SessionsTable + `}} WHERE [[device]]={:d}`).
		Bind(dbx.Params{"d": device}).Column(&sids); err != nil {
		return 0, err
	}
	n := 0
	if fn := kernel.RevokeSession; fn != nil {
		for _, sid := range sids {
			if ok, err := fn(app, sid, reason); err != nil {
				app.Logger().Warn("kiosk: failed to revoke a session", "error", err)
			} else if ok {
				n++
			}
		}
	}
	_, err := app.DB().NewQuery(`DELETE FROM {{` + SessionsTable + `}} WHERE [[device]]={:d}`).Bind(dbx.Params{"d": device}).Execute()
	return n, err
}

// Register binds the module to app: the collection on bootstrap, validation and routes.
func Register(app core.App) *Module {
	m := newModule(app)
	init := func() {
		if err := ensureCollections(app); err != nil {
			app.Logger().Error("kiosk: failed to initialize the collections", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: 1 << 20,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
	app.OnRecordValidate(Collection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "val",
		Func: func(e *core.RecordEvent) error {
			d := deviceOf(e.Record)
			if err := d.Validate(); err != nil {
				return err
			}
			// the actor is only re-checked when it changed: revoke, rotate and set-pin
			// must keep working after the actor was deleted
			if e.Record.IsNew() || actorChanged(e.Record) {
				if err := validateActor(app, d); err != nil {
					return err
				}
			}
			return e.Next()
		},
	})
	m.bindRoutes()
	return m
}

// ---- store ---------------------------------------------------------------

func findByName(app core.App, name string) (*core.Record, error) {
	return app.FindFirstRecordByData(Collection, "name", name)
}

func listDevices(app core.App) ([]*Device, error) {
	if _, err := app.FindCachedCollectionByNameOrId(Collection); err != nil {
		return nil, nil
	}
	recs, err := app.FindAllRecords(Collection)
	if err != nil {
		return nil, err
	}
	out := make([]*Device, 0, len(recs))
	for _, r := range recs {
		out = append(out, deviceOf(r))
	}
	return out, nil
}

// touchSeen writes last_seen at most once per minute and device.
func (m *Module) touchSeen(id string) {
	now := m.now()
	m.mu.Lock()
	if t, ok := m.touch[id]; ok && now.Sub(t) < touchEvery {
		m.mu.Unlock()
		return
	}
	if len(m.touch) >= maxDeviceCnt {
		m.touch = map[string]time.Time{}
	}
	m.touch[id] = now
	m.mu.Unlock()
	if _, err := m.app.DB().Update(Collection, dbx.Params{"last_seen": fmtTime(now)}, dbx.HashExp{"id": id}).Execute(); err != nil {
		m.app.Logger().Warn("kiosk: failed to record last_seen", "error", err)
	}
}

// Provision creates a device and returns it with its one-time pairing code.
func Provision(app core.App, d *Device, pin string, now time.Time, ttl time.Duration) (*core.Record, string, error) {
	if err := ensureCollections(app); err != nil {
		return nil, "", err
	}
	col, err := app.FindCollectionByNameOrId(Collection)
	if err != nil {
		return nil, "", err
	}
	r := core.NewRecord(col)
	r.Set("name", d.Name)
	r.Set("auth_collection", d.AuthCollection)
	r.Set("auth_record", d.AuthRecord)
	r.Set("node_id", d.NodeID)
	r.Set("bind_ip", d.BindIP)
	r.Set("ttl_hours", d.TTLHours)
	r.Set("lock_after_s", d.LockAfterS)
	if pin != "" {
		h, err := HashPin(pin)
		if err != nil {
			return nil, "", err
		}
		r.Set("pin_hash", h)
	}
	code := newPairing(r, now, ttl)
	if err := app.Save(r); err != nil {
		return nil, "", err
	}
	return r, code, nil
}

// newPairing sets a fresh pairing code on r (and drops the device token, so the
// previous browser stops working) and returns the code.
func newPairing(r *core.Record, now time.Time, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = pairingTTL
	}
	code := NewSecret()
	r.Set("pairing_hash", HashToken(code))
	r.Set("pairing_expires", now.Add(ttl).UTC().Format(timeLayout))
	r.Set("token_hash", "")
	return code
}
