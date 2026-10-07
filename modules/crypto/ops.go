package crypto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

const (
	batchSize = 500
	// JobKind is the kernel job kind that runs an operation in the background.
	JobKind = "crypto.reencrypt"
)

// Progress receives a message per batch.
type Progress func(msg string)

func (p Progress) say(format string, a ...any) {
	if p != nil {
		p(fmt.Sprintf(format, a...))
	}
}

// ----- eligibility -----

func (m *Module) eligible(col *core.Collection, field, mode string) error {
	if mode != ModeRandom && mode != ModeBlindIndex {
		return fmt.Errorf("unknown mode %q (random|blind-index)", mode)
	}
	if col.IsView() {
		return errors.New("view collections cannot have encrypted fields")
	}
	if isCryptoSystem(col.Name) || strings.HasPrefix(col.Name, "_") && col.System {
		return errors.New("system collections cannot have encrypted fields")
	}
	f := col.Fields.GetByName(field)
	if f == nil {
		return fmt.Errorf("collection %q has no field %q", col.Name, field)
	}
	if f.GetSystem() || f.GetName() == "id" {
		return fmt.Errorf("field %q is a system field", field)
	}
	if !allowedTypes[f.Type()] {
		return fmt.Errorf("field type %q cannot be encrypted (text, editor, json, email, url only)", f.Type())
	}
	if mode == ModeBlindIndex && f.Type() == core.FieldTypeJSON {
		return errors.New("blind-index is not supported on json fields")
	}
	if why := identityUse(col, field); why != "" {
		return fmt.Errorf("field %q cannot be encrypted: %s", field, why)
	}
	if why := indexUse(col, field); why != "" {
		return fmt.Errorf("field %q cannot be encrypted: %s", field, why)
	}
	if views := viewsUsing(m.app, col, field); len(views) > 0 {
		return fmt.Errorf("field %q cannot be encrypted: the view %s selects it (a view would return ciphertext)", field, strings.Join(views, ", "))
	}
	return nil
}

// ----- row sweeping -----

type rowFn func(id, field, stored string) (newStored string, changed bool, idx *indexOp, err error)

// sweep walks every row of col in id order, applies fn to each listed field
// and writes the changes with a compare-and-swap UPDATE (so a concurrent
// write by a server is never clobbered) plus the matching index change.
// It bypasses record hooks on purpose: no `updated` bump, no events.
func (m *Module) sweep(col *core.Collection, fields []string, fn rowFn, ver int, ik []byte, progress Progress) (changedRows int, err error) {
	last := ""
	scanned := 0
	for {
		cols := append([]string{"id"}, fields...)
		sel := make([]string, len(cols))
		for i, c := range cols {
			sel[i] = "[[" + c + "]]"
		}
		q := "SELECT " + strings.Join(sel, ",") + " FROM {{" + col.Name + "}} WHERE [[id]] > {:last} ORDER BY [[id]] LIMIT " + fmt.Sprint(batchSize)
		var rows []dbx.NullStringMap
		if err := m.app.DB().NewQuery(q).Bind(dbx.Params{"last": last}).All(&rows); err != nil {
			return changedRows, err
		}
		if len(rows) == 0 {
			return changedRows, nil
		}
		type upd struct {
			id, field, old, new string
			idx                 *indexOp
		}
		var ups []upd
		for _, r := range rows {
			id := r["id"].String
			last = id
			for _, f := range fields {
				old := r[f].String
				nw, changed, idx, err := fn(id, f, old)
				if err != nil {
					return changedRows, fmt.Errorf("row %s field %s: %w", id, f, err)
				}
				if changed || idx != nil {
					ups = append(ups, upd{id, f, old, nw, idx})
				}
			}
		}
		if len(ups) > 0 {
			err := m.app.RunInTransaction(func(tx kernel.App) error {
				db := tx.NonconcurrentDB()
				for _, u := range ups {
					if u.new != u.old {
						res, err := db.NewQuery("UPDATE {{" + col.Name + "}} SET [[" + u.field + "]]={:n} WHERE [[id]]={:id} AND [[" + u.field + "]]={:o}").
							Bind(dbx.Params{"n": u.new, "id": u.id, "o": u.old}).Execute()
						if err != nil {
							return err
						}
						if n, _ := res.RowsAffected(); n == 0 {
							continue // changed concurrently: the second pass picks it up
						}
						changedRows++
					}
					if u.idx != nil {
						var err error
						if u.idx.drop {
							err = indexDel(db, col.Id, u.field, u.id)
						} else {
							err = indexPut(db, col.Id, u.field, u.id, blindHMAC(ik, u.field, u.idx.plain), ver)
						}
						if err != nil {
							return err
						}
					}
				}
				return nil
			})
			if err != nil {
				return changedRows, err
			}
		}
		scanned += len(rows)
		progress.say("%s: %d rows scanned, %d values rewritten", col.Name, scanned, changedRows)
	}
}

// reencrypt rewrites the given fields to the active key (and to ciphertext if
// they are still plaintext). Blind-index fields get their index rebuilt.
func (m *Module) reencrypt(col *core.Collection, modes map[string]string, progress Progress) (int, error) {
	ver, dek, err := m.activeKey(col.Id)
	if err != nil {
		return 0, err
	}
	ik, err := indexKey(dek, col.Id)
	if err != nil {
		return 0, err
	}
	k, _ := m.keysFor(col.Id)
	fields := sortedKeys(modes)
	fn := func(id, field, stored string) (string, bool, *indexOp, error) {
		isJSON := isJSONField(col, field)
		var plain string
		if ct, ok := ctOf(stored, isJSON); ok {
			p, err := open(ct, aadFor(col.Id, field, id), k.keyFor)
			if err != nil {
				return "", false, nil, err
			}
			plain = string(p)
			if v, _, _ := parseCT(ct); v == ver && modes[field] != ModeBlindIndex {
				return stored, false, nil, nil
			}
			var idx *indexOp
			if modes[field] == ModeBlindIndex {
				idx = &indexOp{field: field, plain: plain}
			}
			if v, _, _ := parseCT(ct); v == ver {
				return stored, false, idx, nil
			}
			nct, err := seal(dek, ver, aadFor(col.Id, field, id), p)
			if err != nil {
				return "", false, nil, err
			}
			return storedFromCT(nct, isJSON), true, idx, nil
		}
		plain = stored
		if emptyValue(plain, isJSON) {
			if modes[field] == ModeBlindIndex {
				return stored, false, &indexOp{field: field, drop: true}, nil
			}
			return stored, false, nil, nil
		}
		nct, err := seal(dek, ver, aadFor(col.Id, field, id), []byte(plain))
		if err != nil {
			return "", false, nil, err
		}
		var idx *indexOp
		if modes[field] == ModeBlindIndex {
			idx = &indexOp{field: field, plain: plain}
		}
		return storedFromCT(nct, isJSON), true, idx, nil
	}
	return m.sweep(col, fields, fn, ver, ik, progress)
}

// converge runs reencrypt, waits for running servers to pick up the new
// configuration and runs it once more to catch rows written meanwhile.
func (m *Module) converge(col *core.Collection, modes map[string]string, wait bool, progress Progress) (int, error) {
	n, err := m.reencrypt(col, modes, progress)
	if err != nil || !wait {
		return n, err
	}
	time.Sleep(cacheTTL + time.Second)
	n2, err := m.reencrypt(col, modes, progress)
	return n + n2, err
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ----- operations -----

// WaitForServers makes Enable/Disable/Rotate pause cacheTTL+1s and run a
// second pass so that a running server with a stale cache is caught up.
// Tests turn it off.
var WaitForServers = true

// Enable starts encrypting a field: stores the configuration (state
// "enabling") and encrypts the existing rows in batches of 500. Calling it
// again for the same mode resumes; `toki crypto resume` finishes an
// interrupted run.
func Enable(app core.App, collection, field, mode string, progress Progress) (int, error) {
	m := From(app)
	if m == nil {
		return 0, errors.New("crypto module is not registered")
	}
	if !m.Active() {
		if m.masterErr != nil && !errors.Is(m.masterErr, ErrNoMasterKey) {
			return 0, m.masterErr
		}
		return 0, ErrNoMasterKey
	}
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return 0, fmt.Errorf("collection %q not found", collection)
	}
	if mode == "" {
		mode = ModeRandom
	}
	rec, _ := m.configRecord(col.Id, field)
	if rec == nil { // eligibility only gates new configurations (a running one may already be indexed)
		if err := m.eligible(col, field, mode); err != nil {
			return 0, err
		}
	} else {
		if rec.GetString("mode") != mode {
			return 0, fmt.Errorf("%s.%s is already enabled with mode %s; disable it first", col.Name, field, rec.GetString("mode"))
		}
		if rec.GetString("state") == StateDisabling {
			return 0, fmt.Errorf("%s.%s is being disabled; run `toki crypto resume`", col.Name, field)
		}
	}
	unlock, err := m.acquireLock(col.Id, "enable", false)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return m.enableRun(col, field, mode, progress)
}

func (m *Module) enableRun(col *core.Collection, field, mode string, progress Progress) (int, error) {
	app := m.app
	if err := m.ensureKey(col.Id); err != nil {
		return 0, err
	}
	rec, _ := m.configRecord(col.Id, field)
	if rec == nil {
		coll, err := app.FindCollectionByNameOrId(FieldsCollection)
		if err != nil {
			return 0, err
		}
		rec = core.NewRecord(coll)
		rec.Set("collection", col.Id)
		rec.Set("field", field)
		rec.Set("mode", mode)
		rec.Set("state", StateEnabling)
		if err := app.Save(rec); err != nil {
			return 0, err
		}
		emit(ActionEnable, col.Name, "", map[string]any{"field": field, "mode": mode})
	}
	m.Invalidate()
	cfg, err := m.fieldsFor(col.Id)
	if err != nil {
		return 0, err
	}
	n, err := m.converge(col, map[string]string{field: cfg[field]}, WaitForServers, progress)
	if err != nil {
		return n, err // the row keeps state=enabling: `toki crypto resume` continues
	}
	if rec.GetString("state") != "" {
		rec.Set("state", "")
		if err := app.Save(rec); err != nil {
			return n, err
		}
		m.Invalidate()
	}
	return n, nil
}

// countCiphertext counts the rows whose column holds a ciphertext of any
// version (or of one version when ver > 0).
func (m *Module) countCiphertext(col *core.Collection, field string, ver int) (int, error) {
	pre := Prefix
	if ver > 0 {
		pre = fmt.Sprintf("%s%d:", Prefix, ver)
	}
	var n int
	q := "SELECT COUNT(*) FROM {{" + col.Name + "}} WHERE [[" + field + "]] LIKE {:a} OR [[" + field + "]] LIKE {:b}"
	err := m.app.DB().NewQuery(q).Bind(dbx.Params{"a": pre + "%", "b": `"` + pre + "%"}).Row(&n)
	return n, err
}

// Disable decrypts the rows of a field and removes its configuration. The
// configuration row is switched to state "disabling" first (servers then store
// new values as plaintext) and is deleted only after a final check finds no
// ciphertext left, so an interrupted run can always be finished with
// `toki crypto resume` (or by running disable again).
func Disable(app core.App, collection, field string, progress Progress) (int, error) {
	m := From(app)
	if m == nil {
		return 0, errors.New("crypto module is not registered")
	}
	if !m.Active() {
		return 0, ErrNoMasterKey
	}
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return 0, fmt.Errorf("collection %q not found", collection)
	}
	rec, _ := m.configRecord(col.Id, field)
	if rec == nil {
		return 0, fmt.Errorf("%s.%s is not encrypted", col.Name, field)
	}
	unlock, err := m.acquireLock(col.Id, "disable", false)
	if err != nil {
		return 0, err
	}
	defer unlock()
	return m.disableRun(col, field, rec, progress)
}

func (m *Module) disableRun(col *core.Collection, field string, rec *core.Record, progress Progress) (int, error) {
	app := m.app
	if rec.GetString("state") != StateDisabling {
		rec.Set("state", StateDisabling)
		if err := app.Save(rec); err != nil {
			return 0, err
		}
		m.Invalidate()
		if WaitForServers {
			time.Sleep(cacheTTL + time.Second) // every server now stores plaintext for this field
		}
	}
	isJSON := isJSONField(col, field)
	pass := func() (int, error) {
		k, err := m.keysFor(col.Id)
		if err != nil {
			return 0, err
		}
		fn := func(id, f, stored string) (string, bool, *indexOp, error) {
			ct, ok := ctOf(stored, isJSON)
			if !ok {
				return stored, false, nil, nil
			}
			p, err := open(ct, aadFor(col.Id, f, id), k.keyFor)
			if err != nil {
				return "", false, nil, err
			}
			return string(p), true, nil, nil
		}
		return m.sweep(col, []string{field}, fn, 0, nil, progress)
	}
	n, err := pass()
	if err != nil {
		return n, err
	}
	if WaitForServers {
		time.Sleep(cacheTTL + time.Second)
		n2, err := pass()
		n += n2
		if err != nil {
			return n, err
		}
	}
	left, err := m.countCiphertext(col, field, 0)
	if err != nil {
		return n, err
	}
	if left > 0 {
		return n, fmt.Errorf("%s.%s: %d rows still hold ciphertext; the field stays in state disabling, run `toki crypto resume`", col.Name, field, left)
	}
	if _, err := app.NonconcurrentDB().NewQuery("DELETE FROM {{" + IndexTable + "}} WHERE collection={:c} AND field={:f}").
		Bind(dbx.Params{"c": col.Id, "f": field}).Execute(); err != nil {
		return n, err
	}
	if err := app.Delete(rec); err != nil {
		return n, err
	}
	m.Invalidate()
	emit(ActionDisable, col.Name, "", map[string]any{"field": field, "rows": n})
	return n, nil
}

// Rotate creates a new data key version and re-encrypts every encrypted field
// of the collection with it. Old versions stay readable until Retire. The
// collection is locked for the duration; if the run is interrupted the lock
// stays and `toki crypto resume` finishes the re-encryption (without creating
// another key).
func Rotate(app core.App, collection string, progress Progress) (newVersion, rows int, err error) {
	m := From(app)
	if m == nil {
		return 0, 0, errors.New("crypto module is not registered")
	}
	if !m.Active() {
		return 0, 0, ErrNoMasterKey
	}
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return 0, 0, fmt.Errorf("collection %q not found", collection)
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil {
		return 0, 0, err
	}
	if len(cfg) == 0 {
		return 0, 0, fmt.Errorf("collection %q has no encrypted fields", col.Name)
	}
	unlock, err := m.acquireLock(col.Id, "rotate", false)
	if err != nil {
		return 0, 0, err
	}
	ver, err := m.newKey(col.Id)
	if err != nil {
		unlock()
		return 0, 0, err
	}
	emit(ActionRotate, col.Name, "", map[string]any{"version": ver})
	rows, err = m.converge(col, cfg, WaitForServers, progress)
	if err == nil {
		unlock() // on error the lock stays: the rotation is unfinished
	}
	return ver, rows, err
}

// RetireResult reports Retire.
type RetireResult struct {
	Retired []int          `json:"retired"`
	InUse   map[int]string `json:"in_use,omitempty"`
}

// RetireCooldown is how long after the last key was created Retire refuses to
// run, so that a server with a stale key cache cannot still write the old
// version after the usage check. Tests set it to 0.
var RetireCooldown = 2 * cacheTTL

// Retire destroys the non-active key versions of a collection, but only when
// no row still holds a ciphertext of that version in ANY text-like column of
// the collection, configured or not (otherwise nothing changes).
//
// This is key retirement, not erasure: the wrapped key is blanked in the
// database, but older copies survive in SQLite free pages, the WAL, backups
// and replicas. Only destroying the master key (or every copy of the old
// wrapped key) makes the old ciphertext unrecoverable.
func Retire(app core.App, collection string) (*RetireResult, error) {
	m := From(app)
	if m == nil {
		return nil, errors.New("crypto module is not registered")
	}
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return nil, fmt.Errorf("collection %q not found", collection)
	}
	unlock, err := m.acquireLock(col.Id, "retire", false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	infos, err := m.KeyInfos(col.Id)
	if err != nil {
		return nil, err
	}
	if RetireCooldown > 0 {
		if recs, err := m.keyRecords(col.Id); err == nil && len(recs) > 0 {
			last := recs[len(recs)-1].GetDateTime("created").Time()
			if d := time.Since(last); d < RetireCooldown {
				return nil, fmt.Errorf("the newest key was created %s ago; wait %s so that every server has loaded it, then retire",
					d.Round(time.Second), (RetireCooldown - d).Round(time.Second))
			}
		}
	}
	var fields []string
	for _, f := range col.Fields {
		if allowedTypes[f.Type()] {
			fields = append(fields, f.GetName())
		}
	}
	res := &RetireResult{InUse: map[int]string{}}
	var old []int
	for _, ki := range infos {
		if ki.Active || ki.RetiredAt != "" || ki.Version <= 0 {
			continue
		}
		for _, field := range fields {
			n, err := m.countCiphertext(col, field, ki.Version)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				res.InUse[ki.Version] = fmt.Sprintf("%d rows of %s still use it", n, field)
			}
		}
		if _, used := res.InUse[ki.Version]; !used {
			old = append(old, ki.Version)
		}
	}
	if len(old) > 0 {
		if err := m.retireKeys(col.Id, old); err != nil {
			return nil, err
		}
		emit(ActionRetire, col.Name, "", map[string]any{"versions": old})
	}
	res.Retired = old
	return res, nil
}

// Resume finishes every interrupted enable, disable or rotate and returns a
// line per action taken.
func Resume(app core.App, progress Progress) ([]string, error) {
	m := From(app)
	if m == nil {
		return nil, errors.New("crypto module is not registered")
	}
	if !m.Active() {
		return nil, ErrNoMasterKey
	}
	var done []string
	recs, err := app.FindAllRecords(FieldsCollection)
	if err != nil {
		return nil, err
	}
	for _, rec := range recs {
		st := rec.GetString("state")
		if st == "" {
			continue
		}
		col, err := app.FindCollectionByNameOrId(rec.GetString("collection"))
		if err != nil {
			return done, fmt.Errorf("config %s: collection %q not found", rec.Id, rec.GetString("collection"))
		}
		field := rec.GetString("field")
		unlock, err := m.acquireLock(col.Id, "resume", true)
		if err != nil {
			return done, err
		}
		var n int
		switch st {
		case StateEnabling:
			n, err = m.enableRun(col, field, rec.GetString("mode"), progress)
		case StateDisabling:
			n, err = m.disableRun(col, field, rec, progress)
		default:
			err = fmt.Errorf("unknown state %q", st)
		}
		if err != nil {
			unlock()
			return done, fmt.Errorf("%s.%s (%s): %w", col.Name, field, st, err)
		}
		unlock()
		done = append(done, fmt.Sprintf("%s.%s: finished %s (%d values)", col.Name, field, st, n))
	}
	locks, err := m.locks()
	if err != nil {
		return done, err
	}
	for _, l := range locks {
		col, err := app.FindCollectionByNameOrId(l.Collection)
		if err != nil {
			m.dropLock(l.Collection) // collection is gone
			continue
		}
		if l.Op == "rotate" {
			cfg, err := m.fieldsFor(col.Id)
			if err != nil {
				return done, err
			}
			n, err := m.converge(col, cfg, WaitForServers, progress)
			if err != nil {
				return done, fmt.Errorf("%s (rotate): %w", col.Name, err)
			}
			done = append(done, fmt.Sprintf("%s: finished rotate (%d values re-encrypted)", col.Name, n))
		} else {
			done = append(done, fmt.Sprintf("%s: released stale %s lock", col.Name, l.Op))
		}
		m.dropLock(l.Collection)
	}
	return done, nil
}

// VerifyReport is the outcome of Verify.
type VerifyReport struct {
	Collection string   `json:"collection"`
	Sampled    int      `json:"sampled"`
	Decrypted  int      `json:"decrypted"`
	Plaintext  int      `json:"plaintext"`
	Failures   []string `json:"failures"`
}

// Verify decrypts a sample of rows and checks blind-index rows.
func Verify(app core.App, collection string, sample int) (*VerifyReport, error) {
	m := From(app)
	if m == nil {
		return nil, errors.New("crypto module is not registered")
	}
	if !m.Active() {
		return nil, ErrNoMasterKey
	}
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return nil, fmt.Errorf("collection %q not found", collection)
	}
	cfg, err := m.fieldsFor(col.Id)
	if err != nil {
		return nil, err
	}
	rep := &VerifyReport{Collection: col.Name, Failures: []string{}}
	if len(cfg) == 0 {
		return rep, fmt.Errorf("collection %q has no encrypted fields", col.Name)
	}
	if sample <= 0 {
		sample = 100
	}
	k, err := m.keysFor(col.Id)
	if err != nil {
		return rep, err
	}
	fields := sortedKeys(cfg)
	sel := []string{"[[id]]"}
	for _, f := range fields {
		sel = append(sel, "[["+f+"]]")
	}
	var rows []dbx.NullStringMap
	if err := app.DB().NewQuery("SELECT " + strings.Join(sel, ",") + " FROM {{" + col.Name + "}} ORDER BY RANDOM() LIMIT " + fmt.Sprint(sample)).All(&rows); err != nil {
		return rep, err
	}
	for _, r := range rows {
		rep.Sampled++
		id := r["id"].String
		for _, f := range fields {
			isJSON := isJSONField(col, f)
			stored := r[f].String
			ct, ok := ctOf(stored, isJSON)
			if !ok {
				if !emptyValue(stored, isJSON) {
					rep.Plaintext++
					rep.Failures = append(rep.Failures, fmt.Sprintf("%s.%s: row %s is not encrypted", col.Name, f, id))
				}
				continue
			}
			p, err := open(ct, aadFor(col.Id, f, id), k.keyFor)
			if err != nil {
				rep.Failures = append(rep.Failures, fmt.Sprintf("%s.%s: row %s: %v", col.Name, f, id, err))
				continue
			}
			rep.Decrypted++
			if cfg[f] == ModeBlindIndex {
				hs, err := m.blindHMACs(col, f, string(p))
				if err != nil {
					rep.Failures = append(rep.Failures, err.Error())
					continue
				}
				var n int
				if err := app.DB().Select("COUNT(*)").From(IndexTable).
					Where(dbx.HashExp{"collection": col.Id, "field": f, "record": id, "hmac": hs}).Row(&n); err != nil || n == 0 {
					rep.Failures = append(rep.Failures, fmt.Sprintf("%s.%s: row %s has no matching blind-index row", col.Name, f, id))
				}
			}
		}
	}
	return rep, nil
}

// ----- status -----

// CollectionStatus is one collection in Status.
type CollectionStatus struct {
	Collection string            `json:"collection"`
	Fields     map[string]string `json:"fields"`
	Keys       []KeyInfo         `json:"keys"`
}

// StatusReport is the output of `toki crypto status`.
type StatusReport struct {
	MasterKey      bool               `json:"master_key"`
	MasterKeyError string             `json:"master_key_error,omitempty"`
	MasterKeyInDir bool               `json:"master_key_file_inside_data_dir,omitempty"`
	AdminPlaintext bool               `json:"admin_plaintext"`
	Collections    []CollectionStatus `json:"collections"`
	Pending        []Pending          `json:"pending,omitempty"`
	Warnings       []Finding          `json:"warnings"`
}

// Pending is an interrupted operation that `toki crypto resume` finishes.
type Pending struct {
	Collection string `json:"collection"`
	Field      string `json:"field,omitempty"`
	State      string `json:"state"` // enabling | disabling | locked:<op>
	Since      string `json:"since,omitempty"`
}

// Status reports the module state.
func Status(app core.App) (*StatusReport, error) {
	m := From(app)
	if m == nil {
		return nil, errors.New("crypto module is not registered")
	}
	rep := &StatusReport{MasterKey: m.Active(), AdminPlaintext: AdminPlaintext(), Collections: []CollectionStatus{}}
	if m.masterErr != nil && !errors.Is(m.masterErr, ErrNoMasterKey) {
		rep.MasterKeyError = m.masterErr.Error()
	}
	if f := os.Getenv(EnvMasterKeyFile); f != "" && app.DataDir() != "" {
		rep.MasterKeyInDir = strings.HasPrefix(absPath(f), absPath(app.DataDir())+string(os.PathSeparator))
	}
	cfgs, err := List(app)
	if err != nil {
		return nil, err
	}
	idx := map[string]*CollectionStatus{}
	for _, c := range cfgs {
		cs := idx[c.CollId]
		if cs == nil {
			cs = &CollectionStatus{Collection: c.Collection, Fields: map[string]string{}}
			cs.Keys, _ = m.KeyInfos(c.CollId)
			idx[c.CollId] = cs
			rep.Collections = append(rep.Collections, *cs)
			cs = &rep.Collections[len(rep.Collections)-1]
			idx[c.CollId] = cs
		}
		cs.Fields[c.Field] = c.Mode
	}
	for _, c := range cfgs {
		if c.State != "" {
			rep.Pending = append(rep.Pending, Pending{Collection: c.Collection, Field: c.Field, State: c.State})
		}
	}
	if locks, err := m.locks(); err == nil {
		for _, l := range locks {
			name := l.Collection
			if col, err := app.FindCachedCollectionByNameOrId(l.Collection); err == nil && col != nil {
				name = col.Name
			}
			rep.Pending = append(rep.Pending, Pending{Collection: name, State: "locked:" + l.Op, Since: l.Since.Format(time.RFC3339)})
		}
	}
	rep.Warnings, _ = Lint(app)
	return rep, nil
}

// MarshalJSON helper for the CLI.
func (r *StatusReport) JSON() string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

// ----- background job -----

type jobPayload struct {
	Op         string `json:"op"` // enable | disable | rotate
	Collection string `json:"collection"`
	Field      string `json:"field,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

func (m *Module) registerJobs() {
	kernel.Jobs(m.app).Register(JobKind, func(ctx context.Context, app kernel.App, job *kernel.Job) error {
		var p jobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return err
		}
		progress := func(msg string) { m.app.Logger().Info("crypto job: "+msg, "job", job.ID) }
		var err error
		switch p.Op {
		case "enable":
			_, err = Enable(m.app, p.Collection, p.Field, p.Mode, progress)
		case "disable":
			_, err = Disable(m.app, p.Collection, p.Field, progress)
		case "rotate":
			_, _, err = Rotate(m.app, p.Collection, progress)
		default:
			err = fmt.Errorf("unknown op %q", p.Op)
		}
		return err
	})
}

// Enqueue schedules an operation as a kernel job (run by a worker process).
func Enqueue(app core.App, op, collection, field, mode string) (string, error) {
	return kernel.Jobs(app).Enqueue(context.Background(), JobKind,
		jobPayload{Op: op, Collection: collection, Field: field, Mode: mode},
		kernel.MaxAttempts(1), kernel.MaxRuntime(6*time.Hour),
		kernel.Unique(fmt.Sprintf("crypto:%s:%s:%s", op, collection, field)))
}
