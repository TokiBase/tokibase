//go:build !no_crypto

package crypto

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/types"
)

// collKeys are the unwrapped data keys of one collection.
type collKeys struct {
	loaded  time.Time
	deks    map[int][]byte // usable versions (not retired)
	cur     int            // active version (highest usable), 0 if none
	retired map[int]bool
}

// KeyInfo describes one stored key version.
type KeyInfo struct {
	Version   int    `json:"version"`
	Created   string `json:"created"`
	RetiredAt string `json:"retired_at,omitempty"`
	Active    bool   `json:"active"`
}

func dekAAD(collId string, ver int) []byte {
	return []byte(fmt.Sprintf("tkc-dek\x00%s\x00%d", collId, ver))
}

func (m *Module) wrap(collId string, ver int, dek []byte) (string, error) {
	if !m.Active() {
		return "", ErrNoMasterKey
	}
	return sealRaw(m.master, dekAAD(collId, ver), dek)
}

func (m *Module) unwrap(collId string, ver int, wrapped string) ([]byte, error) {
	if !m.Active() {
		return nil, ErrNoMasterKey
	}
	return openRaw(m.master, dekAAD(collId, ver), wrapped)
}

func (m *Module) keyRecords(collId string) ([]*core.Record, error) {
	recs, err := m.app.FindRecordsByFilter(KeysCollection, "collection={:c} && version>0", "version", 0, 0, dbx.Params{"c": collId})
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].GetInt("version") < recs[j].GetInt("version") })
	return recs, nil
}

// keysFor returns the (cached) unwrapped keys of a collection.
func (m *Module) keysFor(collId string) (*collKeys, error) {
	m.keyMu.Lock()
	if k := m.keys[collId]; k != nil && time.Since(k.loaded) < cacheTTL {
		m.keyMu.Unlock()
		return k, nil
	}
	m.keyMu.Unlock()

	recs, err := m.keyRecords(collId)
	if err != nil {
		return nil, err
	}
	k := &collKeys{loaded: time.Now(), deks: map[int][]byte{}, retired: map[int]bool{}}
	for _, r := range recs {
		ver := r.GetInt("version")
		if !r.GetDateTime("retired_at").IsZero() || r.GetString("wrapped_dek") == "" {
			k.retired[ver] = true
			continue
		}
		dek, err := m.unwrap(collId, ver, r.GetString("wrapped_dek"))
		if err != nil {
			return nil, fmt.Errorf("crypto: cannot unwrap key v%d of %s (wrong master key?): %w", ver, collId, err)
		}
		k.deks[ver] = dek
		if ver > k.cur {
			k.cur = ver
		}
	}
	m.keyMu.Lock()
	m.keys[collId] = k
	m.keyMu.Unlock()
	return k, nil
}

func (k *collKeys) keyFor(ver int) ([]byte, error) {
	if d, ok := k.deks[ver]; ok {
		return d, nil
	}
	return nil, errKeyVersion
}

// activeKey returns the key used for new writes.
func (m *Module) activeKey(collId string) (int, []byte, error) {
	k, err := m.keysFor(collId)
	if err != nil {
		return 0, nil, err
	}
	if k.cur == 0 {
		return 0, nil, errors.New("crypto: no active data key for collection " + collId)
	}
	return k.cur, k.deks[k.cur], nil
}

// newKey creates the next DEK version of a collection and returns it.
func (m *Module) newKey(collId string) (int, error) {
	if !m.Active() {
		return 0, ErrNoMasterKey
	}
	recs, err := m.keyRecords(collId)
	if err != nil {
		return 0, err
	}
	ver := 1
	if n := len(recs); n > 0 {
		ver = recs[n-1].GetInt("version") + 1
	}
	dek, err := randomBytes(32)
	if err != nil {
		return 0, err
	}
	w, err := m.wrap(collId, ver, dek)
	if err != nil {
		return 0, err
	}
	coll, err := m.app.FindCollectionByNameOrId(KeysCollection)
	if err != nil {
		return 0, err
	}
	rec := core.NewRecord(coll)
	rec.Set("collection", collId)
	rec.Set("version", ver)
	rec.Set("wrapped_dek", w)
	if err := m.app.Save(rec); err != nil {
		return 0, fmt.Errorf("crypto: cannot store key: %w", err)
	}
	m.Invalidate()
	return ver, nil
}

// ensureKey makes sure the collection has an active key.
func (m *Module) ensureKey(collId string) error {
	k, err := m.keysFor(collId)
	if err != nil {
		return err
	}
	if k.cur > 0 {
		return nil
	}
	if _, err := m.newKey(collId); err != nil {
		// another process may have created version 1 meanwhile
		m.Invalidate()
		if k, err2 := m.keysFor(collId); err2 == nil && k.cur > 0 {
			return nil
		}
		return err
	}
	return nil
}

// KeyInfos lists the key versions of a collection.
func (m *Module) KeyInfos(collId string) ([]KeyInfo, error) {
	recs, err := m.keyRecords(collId)
	if err != nil {
		return nil, err
	}
	cur := 0
	for _, r := range recs {
		if r.GetDateTime("retired_at").IsZero() && r.GetString("wrapped_dek") != "" {
			cur = r.GetInt("version")
		}
	}
	out := []KeyInfo{}
	for _, r := range recs {
		ki := KeyInfo{Version: r.GetInt("version"), Created: r.GetDateTime("created").String()}
		if t := r.GetDateTime("retired_at"); !t.IsZero() {
			ki.RetiredAt = t.String()
		}
		ki.Active = ki.Version == cur
		out = append(out, ki)
	}
	return out, nil
}

func (m *Module) retireKeys(collId string, versions []int) error {
	recs, err := m.keyRecords(collId)
	if err != nil {
		return err
	}
	set := map[int]bool{}
	for _, v := range versions {
		set[v] = true
	}
	for _, r := range recs {
		if !set[r.GetInt("version")] {
			continue
		}
		r.Set("wrapped_dek", "") // destroy the key material
		dt, _ := types.ParseDateTime(time.Now().UTC())
		r.Set("retired_at", dt)
		if err := m.app.Save(r); err != nil {
			return err
		}
	}
	m.Invalidate()
	return nil
}

// ----- advisory operation lock -----
//
// An operation (enable, disable, rotate, retire) on a collection holds a row
// in `_crypto_keys` with version lockVersion; the unique (collection, version)
// index makes taking it atomic across processes. Normal key reads ignore it.

const lockVersion = -1

// lockTTL is how long a lock blocks other operations before it counts as
// abandoned (the longest background job runs 6 hours).
var lockTTL = 6 * time.Hour

type lockInfo struct {
	Collection string
	Op         string
	Since      time.Time
}

func parseLock(r *core.Record) lockInfo {
	l := lockInfo{Collection: r.GetString("collection")}
	parts := strings.SplitN(r.GetString("wrapped_dek"), "|", 2)
	l.Op = parts[0]
	if len(parts) == 2 {
		l.Since, _ = time.Parse(time.RFC3339, parts[1])
	}
	return l
}

func (m *Module) lockRecord(collId string) *core.Record {
	r, _ := m.app.FindFirstRecordByFilter(KeysCollection, "collection={:c} && version<=0", dbx.Params{"c": collId})
	return r
}

// locks lists every lock row.
func (m *Module) locks() ([]lockInfo, error) {
	recs, err := m.app.FindRecordsByFilter(KeysCollection, "version<=0", "", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]lockInfo, 0, len(recs))
	for _, r := range recs {
		out = append(out, parseLock(r))
	}
	return out, nil
}

func (m *Module) dropLock(collId string) {
	if r := m.lockRecord(collId); r != nil {
		_ = m.app.Delete(r)
	}
}

// acquireLock takes the operation lock of a collection. takeover replaces an
// existing lock (used by resume). The returned func releases the lock.
func (m *Module) acquireLock(collId, op string, takeover bool) (func(), error) {
	if r := m.lockRecord(collId); r != nil {
		l := parseLock(r)
		if !takeover && time.Since(l.Since) < lockTTL {
			return nil, fmt.Errorf("a crypto %s on this collection is running or was interrupted (since %s); wait for it or run `toki crypto resume`",
				l.Op, l.Since.Format(time.RFC3339))
		}
		if err := m.app.Delete(r); err != nil {
			return nil, err
		}
	}
	coll, err := m.app.FindCollectionByNameOrId(KeysCollection)
	if err != nil {
		return nil, err
	}
	rec := core.NewRecord(coll)
	rec.Set("collection", collId)
	rec.Set("version", lockVersion)
	rec.Set("wrapped_dek", op+"|"+time.Now().UTC().Format(time.RFC3339))
	if err := m.app.Save(rec); err != nil {
		return nil, fmt.Errorf("crypto: another operation holds the lock of this collection: %w", err)
	}
	return func() { m.dropLock(collId) }, nil
}
