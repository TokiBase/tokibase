//go:build !no_sync

package sync

import (
	"slices"
	"sort"
	"strings"
	stdsync "sync"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
)

// `crypto: strip` (docs/SYNC_DESIGN.md §7.6) withholds the encrypted fields of
// a collection from every node. WHICH fields are withheld is part of the
// policy row (`_sync_policies.strip_fields`), written by the hub when the
// policy is saved and whenever a field of the collection becomes encrypted. It
// never shrinks while the policy says strip: `toki crypto disable`, a hub whose
// crypto module is off, compiled out or not loaded yet cannot turn a withheld
// field into a synced one. To release a field the admin sets `crypto` to
// ciphertext (or none) explicitly, which clears the list and sends the field to
// the nodes.

// sameCollection reports whether two policy/crypto references (name or id)
// name the same collection.
func sameCollection(app kernel.App, a, b string) bool {
	if a == b {
		return true
	}
	ca, errA := app.FindCachedCollectionByNameOrId(a)
	cb, errB := app.FindCachedCollectionByNameOrId(b)
	return errA == nil && errB == nil && ca != nil && cb != nil && ca.Id == cb.Id
}

// stripFieldsFor computes the strip set of a policy row: empty unless the
// policy says strip, else the union of the persisted list, the live registry
// and the `_crypto_fields` rows of the collection.
func (m *Module) stripFieldsFor(app kernel.App, r *core.Record) []string {
	if r.GetString("crypto") != CryptoStrip {
		return []string{}
	}
	set := map[string]struct{}{}
	for _, f := range stripFieldsOfRow(r) {
		set[f] = struct{}{}
	}
	ref := strings.TrimSpace(r.GetString("collection"))
	if col, err := app.FindCachedCollectionByNameOrId(ref); err == nil && col != nil {
		for _, f := range kernel.SensitiveFieldsOf(col.Id) {
			set[f] = struct{}{}
		}
	}
	if app.HasTable(cryptoFieldsCollection) {
		if recs, err := app.FindAllRecords(cryptoFieldsCollection); err == nil {
			for _, cf := range recs {
				if sameCollection(app, cf.GetString("collection"), ref) {
					set[cf.GetString("field")] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// syncStripFields brings the persisted strip sets in line at boot (a policy
// saved by an older build, or without validation, has none yet).
func (m *Module) syncStripFields() {
	recs, err := m.app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return
	}
	for _, r := range recs {
		if r.GetString("crypto") != CryptoStrip {
			continue
		}
		want := m.stripFieldsFor(m.app, r)
		if slices.Equal(want, stripFieldsOfRow(r)) {
			continue
		}
		r.Set("strip_fields", want)
		if err := m.app.SaveNoValidate(r); err != nil {
			m.app.Logger().Error("sync: cannot persist the strip fields of a policy", "collection", r.GetString("collection"), "error", err)
		}
	}
}

// addStripField adds an encrypted field to the strip set of the strip policy of
// its collection (called before the `_crypto_fields` row is created, so that no
// write sees the field encrypted but not withheld).
func (m *Module) addStripField(app kernel.App, collRef, field string) error {
	recs, err := app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.GetString("crypto") != CryptoStrip || !sameCollection(app, r.GetString("collection"), collRef) {
			continue
		}
		cur := stripFieldsOfRow(r)
		if slices.Contains(cur, field) {
			continue
		}
		cur = append(cur, field)
		sort.Strings(cur)
		r.Set("strip_fields", cur)
		if err := app.SaveNoValidate(r); err != nil {
			return err
		}
	}
	return nil
}

// hasStripPolicy reports whether any enabled policy withholds a field.
func (m *Module) hasStripPolicy() bool {
	rows, err := m.pol.load()
	if err != nil {
		return true // fail closed: assume yes
	}
	for _, p := range rows {
		if p.Crypto == CryptoStrip && p.Direction != DirNone && len(p.StripFields) > 0 {
			return true
		}
	}
	return false
}

// stripWas remembers, per policy record being updated, the strip set before the save.
var stripWas stdsync.Map // *core.Record -> []string

// bindStrip keeps the strip sets current (hub only).
func (m *Module) bindStrip() {
	if m.role != RoleHub {
		return
	}
	m.app.OnRecordCreate(cryptoFieldsCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "cfstrip", Priority: -100, Func: func(e *core.RecordEvent) error {
			if err := m.addStripField(e.App, e.Record.GetString("collection"), e.Record.GetString("field")); err != nil {
				return err
			}
			return e.Next()
		},
	})
	// a policy that stops withholding fields: the nodes never received them, so
	// every record is sent again with the fields (the pull of an old change only
	// carries the fields that were synced when it was written)
	m.app.OnRecordUpdate(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "stripwas", Priority: -100, Func: func(e *core.RecordEvent) error {
			if o := e.Record.Original(); o != nil {
				stripWas.Store(e.Record, stripFieldsOfRow(o))
			}
			err := e.Next()
			if err != nil {
				stripWas.Delete(e.Record)
			}
			return err
		},
	})
	m.app.OnRecordAfterUpdateSuccess(PoliciesCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "stripflip", Func: func(e *core.RecordEvent) error {
			err := e.Next()
			v, ok := stripWas.LoadAndDelete(e.Record)
			if err != nil || !ok {
				return err
			}
			was, _ := v.([]string)
			if len(was) == 0 || e.Record.GetString("crypto") == CryptoStrip {
				return nil
			}
			if col := m.collectionOf(e.Record.GetString("collection")); col != nil {
				if rerr := m.resendFields(col.Id, was); rerr != nil {
					m.app.Logger().Error("sync: cannot queue the fields released from crypto: strip", "collection", col.Name, "error", rerr)
				}
			}
			return nil
		},
	})
}
