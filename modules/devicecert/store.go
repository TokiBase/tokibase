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

	stateCA = "ca"
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
	if serialOrName == "" {
		return nil, kernel.ErrDeviceCertNotFound
	}
	recs, err := m.app.FindRecordsByFilter(CertsCollection, "serial = {:s}", "", 1, 0, dbx.Params{"s": strings.ToLower(serialOrName)})
	if err == nil && len(recs) == 1 {
		return recs[0], nil
	}
	recs, err = m.app.FindRecordsByFilter(CertsCollection, "name = {:s}", "-created", 100, 0, dbx.Params{"s": serialOrName})
	if err != nil || len(recs) == 0 {
		return nil, kernel.ErrDeviceCertNotFound
	}
	for _, r := range recs {
		if r.GetDateTime("revoked_at").IsZero() {
			return r, nil
		}
	}
	return recs[0], nil
}

// denyList is the set of revoked serials, re-read from `_device_certs` at most
// every ttl. On a node that has no rows (PR 7 pulls them from the hub) it is empty.
type denyList struct {
	m   *Module
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	set  map[string]bool
	load func() (map[string]bool, error)
}

func (d *denyList) has(serial string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.m.now()
	if d.set == nil || now.Sub(d.at) >= d.ttl {
		set, err := d.load()
		if err != nil {
			d.m.app.Logger().Warn("devicecert: failed to read the revoked serials", "error", err)
			if d.set == nil {
				set = map[string]bool{}
			} else {
				set = d.set
			}
		}
		d.set, d.at = set, now
	}
	return d.set[strings.ToLower(serial)]
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
