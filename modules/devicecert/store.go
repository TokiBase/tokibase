//go:build !no_devicecert

package devicecert

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

const (
	// CertsCollection is the superuser-only record of every issued certificate.
	CertsCollection = "_device_certs"
	// StateTable is the plain table that holds the CA (not a collection).
	StateTable = "_devicecert_state"

	stateCA    = "ca"
	stateCAOld = "ca_old"
)

var ensureMu sync.Mutex

func floatPtr(f float64) *float64 { return &f }

// ensureSchema creates the state table and the `_device_certs` collection.
func ensureSchema(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if !app.HasTable("_collections") {
		return errors.New("devicecert: _collections table is not ready")
	}
	if _, err := app.NonconcurrentDB().NewQuery("CREATE TABLE IF NOT EXISTS " + StateTable +
		" (key TEXT PRIMARY KEY NOT NULL, value TEXT NOT NULL DEFAULT '')").Execute(); err != nil {
		return err
	}
	if _, err := app.FindCachedCollectionByNameOrId(CertsCollection); err == nil {
		return nil
	}
	c := core.NewBaseCollection(CertsCollection)
	c.System = true // rules stay nil: superuser only
	c.Fields.Add(
		&core.TextField{Name: "serial", Required: true, Max: 64},
		&core.TextField{Name: "name", Required: true, Max: 128},
		&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: []string{string(kernel.DeviceCertClient), string(kernel.DeviceCertServer)}},
		&core.TextField{Name: "node", Max: 64},
		&core.DateField{Name: "not_after"},
		&core.DateField{Name: "revoked_at"},
		&core.TextField{Name: "route_scope", Max: 256},
		&core.AutodateField{Name: "created", OnCreate: true},
		&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
	)
	c.AddIndex("idx_toki_device_certs_serial", true, "serial", "")
	c.AddIndex("idx_toki_device_certs_name", false, "name", "")
	return app.Save(c)
}

type stateDB struct{ app core.App }

func (s stateDB) get(key string) (string, bool, error) {
	var v string
	err := s.app.DB().NewQuery("SELECT value FROM " + StateTable + " WHERE key={:k}").Bind(dbx.Params{"k": key}).Row(&v)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

// putIfAbsent stores the value unless the key exists.
func (s stateDB) putIfAbsent(key, value string) error {
	_, err := s.app.NonconcurrentDB().NewQuery("INSERT INTO " + StateTable + " (key, value) VALUES ({:k},{:v}) ON CONFLICT(key) DO NOTHING").
		Bind(dbx.Params{"k": key, "v": value}).Execute()
	return err
}

// put stores the value, replacing an existing one.
func (s stateDB) put(key, value string) error {
	_, err := s.app.NonconcurrentDB().NewQuery("INSERT INTO " + StateTable + " (key, value) VALUES ({:k},{:v}) ON CONFLICT(key) DO UPDATE SET value=excluded.value").
		Bind(dbx.Params{"k": key, "v": value}).Execute()
	return err
}

func recordToCert(r *core.Record) *kernel.DeviceCert {
	return &kernel.DeviceCert{
		Serial: r.GetString("serial"), Name: r.GetString("name"),
		Kind: kernel.DeviceCertKind(r.GetString("kind")), Node: r.GetString("node"),
		NotAfter: r.GetDateTime("not_after").Time(), RevokedAt: r.GetDateTime("revoked_at").Time(),
	}
}

func (m *Module) insertCert(serial, name string, kind kernel.DeviceCertKind, node string, notAfter time.Time, scope string) error {
	col, err := m.app.FindCachedCollectionByNameOrId(CertsCollection)
	if err != nil {
		return err
	}
	rec := core.NewRecord(col)
	rec.Set("serial", serial)
	rec.Set("name", name)
	rec.Set("kind", string(kind))
	rec.Set("node", node)
	rec.Set("not_after", notAfter.UTC().Format(types.DefaultDateLayout))
	rec.Set("route_scope", scope)
	return m.app.Save(rec)
}

// find returns the row for a serial or, failing that, the newest unrevoked
// row for a name (the newest row when all are revoked).
func (m *Module) find(serialOrName string) (*core.Record, error) {
	rows, err := m.byName(serialOrName)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.GetDateTime("revoked_at").IsZero() {
			return r, nil
		}
	}
	return rows[0], nil
}

// byName returns the row of a serial, or every row of a name (newest first).
func (m *Module) byName(serialOrName string) ([]*core.Record, error) {
	if serialOrName == "" {
		return nil, kernel.ErrDeviceCertNotFound
	}
	recs, err := m.app.FindRecordsByFilter(CertsCollection, "serial = {:s}", "", 1, 0, dbx.Params{"s": strings.ToLower(serialOrName)})
	if err == nil && len(recs) == 1 {
		return recs, nil
	}
	recs, err = m.app.FindRecordsByFilter(CertsCollection, "name = {:s}", "-created", 500, 0, dbx.Params{"s": serialOrName})
	if err != nil || len(recs) == 0 {
		return nil, kernel.ErrDeviceCertNotFound
	}
	return recs, nil
}

func (m *Module) revokeTargets(serialOrName string) ([]*core.Record, error) {
	return m.byName(serialOrName)
}

// denyGrace is how long a failed reload keeps serving the last good deny set.
// Past it the list fails closed: every client certificate is refused until the
// table can be read again.
const denyGrace = 5 * time.Minute

// denyList is the set of revoked serials, re-read from `_device_certs` at most
// every ttl (lazily, and by a background refresh while the listener runs). On
// a node the rows come from the hub through a pull-only sync policy.
type denyList struct {
	m     *Module
	ttl   time.Duration
	grace time.Duration

	mu   sync.Mutex
	at   time.Time
	set  map[string]bool
	load func() (map[string]bool, error)
}

// get returns the deny set; failed is true when it can not be trusted (never
// loaded, or the reload kept failing past the grace period).
func (d *denyList) get() (set map[string]bool, failed bool) { return d.read(false) }

// read is get; force skips the ttl cache but keeps the previous set and its
// timestamp when the reload fails, so the fail-closed grace still applies.
func (d *denyList) read(force bool) (set map[string]bool, failed bool) {
	d.mu.Lock()
	now := d.m.now()
	if !force && d.set != nil && now.Sub(d.at) < d.ttl {
		set = d.set
		d.mu.Unlock()
		return set, false
	}
	d.mu.Unlock()
	fresh, err := d.load() // the query runs outside the lock
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		d.set, d.at = fresh, now
		return fresh, false
	}
	d.m.app.Logger().Warn("devicecert: failed to read the revoked serials", "error", err)
	grace := d.grace
	if grace <= 0 {
		grace = denyGrace
	}
	if d.set != nil && now.Sub(d.at) < grace {
		return d.set, false
	}
	return nil, true
}

// has reports whether the serial is revoked. It fails closed.
func (d *denyList) has(serial string) bool {
	set, failed := d.get()
	return failed || set[strings.ToLower(serial)]
}

// refresh forces a reload and returns the size of the set.
func (d *denyList) refresh() int {
	set, _ := d.read(true)
	return len(set)
}

func (d *denyList) reset() {
	d.mu.Lock()
	d.set = nil
	d.mu.Unlock()
}

// size is the number of revoked serials (it loads the list when it is stale).
func (d *denyList) size() int {
	set, _ := d.get()
	return len(set)
}

// certInfo is what the route-scope middleware needs to know about a serial.
type certInfo struct {
	found    bool
	name     string
	kind     kernel.DeviceCertKind
	scope    string
	revoked  bool
	notAfter time.Time
	at       time.Time
}

const certInfoTTL = 15 * time.Second

// info returns the `_device_certs` row of a serial (cached for 15 s).
func (m *Module) info(serial string) certInfo {
	serial = strings.ToLower(serial)
	m.infoMu.Lock()
	ci, ok := m.infos[serial]
	m.infoMu.Unlock()
	if ok && m.now().Sub(ci.at) < certInfoTTL {
		return ci
	}
	ci = certInfo{at: m.now()}
	recs, err := m.app.FindRecordsByFilter(CertsCollection, "serial = {:s}", "", 1, 0, dbx.Params{"s": serial})
	if err == nil && len(recs) == 1 {
		r := recs[0]
		ci.found, ci.name, ci.kind = true, r.GetString("name"), kernel.DeviceCertKind(r.GetString("kind"))
		ci.scope, ci.revoked = r.GetString("route_scope"), !r.GetDateTime("revoked_at").IsZero()
		ci.notAfter = r.GetDateTime("not_after").Time()
	}
	m.infoMu.Lock()
	if m.infos == nil || len(m.infos) > 1024 {
		m.infos = map[string]certInfo{}
	}
	m.infos[serial] = ci
	m.infoMu.Unlock()
	return ci
}

func (m *Module) resetInfos() {
	m.infoMu.Lock()
	m.infos = nil
	m.infoMu.Unlock()
}

// PruneAfter is how long an expired certificate row is kept.
const PruneAfter = 30 * 24 * time.Hour

// prune deletes the rows that expired more than [PruneAfter] ago, so a hub
// whose LAN addresses change does not grow `_device_certs` without bound. The
// deletes go through the record API, so sync carries them to the nodes.
func (m *Module) prune() int {
	cut := m.now().Add(-PruneAfter).UTC().Format(types.DefaultDateLayout)
	recs, err := m.app.FindRecordsByFilter(CertsCollection, "not_after != '' && not_after < {:c}", "", 200, 0, dbx.Params{"c": cut})
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range recs {
		if m.app.Delete(r) == nil {
			n++
		}
	}
	return n
}

func (m *Module) loadRevoked() (map[string]bool, error) {
	out := map[string]bool{}
	if _, err := m.app.FindCachedCollectionByNameOrId(CertsCollection); err != nil {
		return out, nil
	}
	var serials []string
	err := m.app.DB().NewQuery("SELECT serial FROM " + CertsCollection + " WHERE revoked_at != ''").Column(&serials)
	for _, s := range serials {
		out[strings.ToLower(s)] = true
	}
	return out, err
}
