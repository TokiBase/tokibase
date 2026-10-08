//go:build !no_payments

package payments

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/types"
)

// Entitlement statuses.
const (
	EntActive = "active"
	EntTrial  = "trial"
	EntGrace  = "grace"
	EntLapsed = "lapsed"
)

func (m *Module) grantForIntent(tx kernel.App, intent *core.Record, now time.Time) error {
	var g Grant
	if intent.GetString("grant") == "" || intent.GetString("grant") == "null" {
		return nil
	}
	if err := intent.UnmarshalJSONField("grant", &g); err != nil || g.Key == "" {
		return nil
	}
	subject := intent.GetString("customer")
	if subject == "" {
		return nil // anonymous server side intent: nobody to entitle
	}
	_, err := grantEntitlement(tx, subject, intent.GetString("customer_collection"), g, EntActive, intent.Id, now)
	return err
}

// revokeForIntent takes back what the intent contributed: its days are taken
// off the entitlement's end, its balance off the balance; the entitlement
// lapses only when no other un-revoked grant is left. Entitlements written
// before the ledger existed (no rows for the intent) are lapsed whole by
// source_intent, as before.
func (m *Module) revokeForIntent(tx kernel.App, intent *core.Record, now time.Time) error {
	grants, err := tx.FindRecordsByFilter(GrantsCollection, "intent={:i} && revoked=false", "", 0, 0, dbx.Params{"i": intent.Id})
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return m.revokeLegacy(tx, intent, now)
	}
	for _, gr := range grants {
		gr.Set("revoked", true)
		if err := tx.Save(gr); err != nil {
			return err
		}
		e, err := tx.FindRecordById(EntitlementsCollection, gr.GetString("entitlement"))
		if err != nil {
			continue
		}
		if err := shrinkEntitlement(tx, e, gr, intent.Id, now); err != nil {
			return err
		}
	}
	return syncSubscriptions(tx, now)
}

func shrinkEntitlement(tx kernel.App, e, gr *core.Record, intentID string, now time.Time) error {
	if b := int64(gr.GetInt("balance")); b > 0 {
		nb := int64(e.GetInt("balance")) - b
		if nb < 0 {
			nb = 0
		}
		e.Set("balance", nb)
	}
	rest, err := tx.FindRecordsByFilter(GrantsCollection, "entitlement={:e} && revoked=false", "", 0, 0, dbx.Params{"e": e.Id})
	if err != nil {
		return err
	}
	lapse := len(rest) == 0
	if !lapse && e.GetString("status") != EntLapsed {
		until := e.GetDateTime("until")
		days := gr.GetInt("days")
		switch {
		case days > 0 && !until.IsZero():
			nu := until.Time().Add(-time.Duration(days) * 24 * time.Hour)
			if !nu.After(now) {
				lapse = true
			} else {
				e.Set("until", nu)
				e.Set("period_end", nu)
			}
		case days == 0 && until.IsZero():
			// an open ended grant was revoked: fall back to the latest end of the others
			var latest time.Time
			forever := false
			for _, o := range rest {
				end := o.GetDateTime("ends")
				if end.IsZero() {
					forever = true
					break
				}
				if end.Time().After(latest) {
					latest = end.Time()
				}
			}
			if !forever {
				if !latest.After(now) {
					lapse = true
				} else {
					e.Set("until", latest)
					e.Set("period_end", latest)
				}
			}
		}
	}
	if lapse {
		e.Set("status", EntLapsed)
		e.Set("until", now)
	}
	if err := tx.Save(e); err != nil {
		return err
	}
	audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{"action": "revoke", "key": e.GetString("key"),
		"subject": e.GetString("subject"), "intent": intentID, "lapsed": lapse})
	return nil
}

func (m *Module) revokeLegacy(tx kernel.App, intent *core.Record, now time.Time) error {
	rs, err := tx.FindRecordsByFilter(EntitlementsCollection, "source_intent={:i} && status!='lapsed'", "", 0, 0, dbx.Params{"i": intent.Id})
	if err != nil {
		return err
	}
	var g Grant
	_ = intent.UnmarshalJSONField("grant", &g)
	for _, e := range rs {
		e.Set("status", EntLapsed)
		e.Set("until", now)
		if g.Balance > 0 {
			b := int64(e.GetInt("balance")) - g.Balance
			if b < 0 {
				b = 0
			}
			e.Set("balance", b)
		}
		if err := tx.Save(e); err != nil {
			return err
		}
		audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{"action": "revoke", "key": e.GetString("key"), "subject": e.GetString("subject"), "intent": intent.Id})
	}
	return syncSubscriptions(tx, now)
}

// grantEntitlement creates or extends an entitlement. Days extend from the
// later of now and the current paid period end; an expired or grace row
// restarts from now. status is active or trial.
func grantEntitlement(tx kernel.App, subject, subjectCol string, g Grant, status, intentID string, now time.Time) (*core.Record, error) {
	if status != EntActive && status != EntTrial {
		return nil, fmt.Errorf("%w: status must be active or trial", ErrInvalid)
	}
	e, _ := tx.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c} && key={:k}",
		dbx.Params{"s": subject, "c": subjectCol, "k": g.Key})
	isNew := e == nil
	if isNew {
		col, err := tx.FindCachedCollectionByNameOrId(EntitlementsCollection)
		if err != nil {
			return nil, err
		}
		e = core.NewRecord(col)
		e.Set("subject", subject)
		e.Set("subject_collection", subjectCol)
		e.Set("key", g.Key)
	}
	base := now
	forever := false
	if !isNew {
		cur := e.GetString("status")
		until := e.GetDateTime("until")
		if cur == EntActive || cur == EntTrial {
			if until.IsZero() {
				forever = true
			} else if until.Time().After(now) {
				base = until.Time()
			}
		}
	}
	if g.Days == 0 || forever {
		e.Set("until", "")
		e.Set("period_end", "")
	} else {
		end := base.Add(time.Duration(g.Days) * 24 * time.Hour)
		e.Set("until", end)
		e.Set("period_end", end)
	}
	e.Set("status", status)
	e.Set("grace_seconds", int64(g.GraceDays)*86400)
	if g.Quota > 0 {
		e.Set("quota", g.Quota)
	}
	if g.Balance > 0 {
		e.Set("balance", int64(e.GetInt("balance"))+g.Balance)
	}
	if intentID != "" {
		e.Set("source_intent", intentID)
	}
	if g.Subscription && intentID != "" {
		sub, err := upsertSubscription(tx, subject, subjectCol, g, e, intentID)
		if err != nil {
			return nil, err
		}
		e.Set("source_subscription", sub.Id)
	}
	if err := tx.Save(e); err != nil {
		return nil, err
	}
	col, err := tx.FindCachedCollectionByNameOrId(GrantsCollection)
	if err != nil {
		return nil, err
	}
	gr := core.NewRecord(col)
	gr.Set("entitlement", e.Id)
	gr.Set("intent", intentID)
	gr.Set("days", g.Days)
	gr.Set("balance", g.Balance)
	gr.Set("ends", e.GetString("until"))
	if err := tx.Save(gr); err != nil {
		return nil, err
	}
	audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{
		"action": "grant", "key": g.Key, "subject": subject, "status": status, "until": e.GetString("until"), "intent": intentID,
	})
	return e, nil
}

func upsertSubscription(tx kernel.App, subject, subjectCol string, g Grant, ent *core.Record, intentID string) (*core.Record, error) {
	s, _ := tx.FindFirstRecordByFilter(SubscriptionsCollection, "subject={:s} && subject_collection={:c} && product={:p}",
		dbx.Params{"s": subject, "c": subjectCol, "p": g.Key})
	if s == nil {
		col, err := tx.FindCachedCollectionByNameOrId(SubscriptionsCollection)
		if err != nil {
			return nil, err
		}
		s = core.NewRecord(col)
		s.Set("subject", subject)
		s.Set("subject_collection", subjectCol)
		s.Set("product", g.Key)
	}
	s.Set("status", "active")
	s.Set("current_period_end", ent.GetDateTime("until"))
	s.Set("last_intent", intentID)
	if err := tx.Save(s); err != nil {
		return nil, err
	}
	return s, nil
}

// syncSubscriptions marks subscriptions expired when their entitlement lapsed
// (one statement, no per-row queries).
func syncSubscriptions(tx kernel.App, now time.Time) error {
	nd, _ := types.ParseDateTime(now.UTC())
	_, err := tx.DB().NewQuery("UPDATE {{" + SubscriptionsCollection + "}} SET [[status]]='expired', [[updated]]={:u} " +
		"WHERE [[status]] IN ('active','trial') AND EXISTS (SELECT 1 FROM {{" + EntitlementsCollection + "}} AS [[e]] " +
		"WHERE [[e.source_subscription]] = {{" + SubscriptionsCollection + "}}.[[id]] AND [[e.status]]='lapsed')").
		Bind(dbx.Params{"u": nd.String()}).Execute()
	return err
}

// GrantEntitlement grants (or extends) an entitlement by hand (CLI, Go API).
func (m *Module) GrantEntitlement(subject, subjectCollection string, g Grant, status string) (*core.Record, error) {
	if subject == "" || g.Key == "" {
		return nil, fmt.Errorf("%w: subject and key are required", ErrInvalid)
	}
	if g.Days < 0 || g.GraceDays < 0 || g.Quota < 0 || g.Balance < 0 {
		return nil, fmt.Errorf("%w: days, grace days, quota and balance cannot be negative", ErrInvalid)
	}
	if status == "" {
		status = EntActive
	}
	var out *core.Record
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		if status == EntTrial {
			if ex, _ := tx.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c} && key={:k}",
				dbx.Params{"s": subject, "c": subjectCollection, "k": g.Key}); ex != nil {
				return fmt.Errorf("%w: a trial needs a subject without an existing %q entitlement", ErrInvalid, g.Key)
			}
		}
		e, err := grantEntitlement(tx, subject, subjectCollection, g, status, "", m.now())
		out = e
		return err
	})
	return out, err
}

// RevokeEntitlement lapses an entitlement immediately (all its grants are revoked).
func (m *Module) RevokeEntitlement(subject, subjectCollection, key string) (bool, error) {
	found := false
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		e, _ := tx.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c} && key={:k}",
			dbx.Params{"s": subject, "c": subjectCollection, "k": key})
		if e == nil {
			return nil
		}
		found = true
		now := m.now()
		e.Set("status", EntLapsed)
		e.Set("until", now)
		if err := tx.Save(e); err != nil {
			return err
		}
		grs, err := tx.FindRecordsByFilter(GrantsCollection, "entitlement={:e} && revoked=false", "", 0, 0, dbx.Params{"e": e.Id})
		if err != nil {
			return err
		}
		for _, gr := range grs {
			gr.Set("revoked", true)
			if err := tx.Save(gr); err != nil {
				return err
			}
		}
		audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{"action": "revoke", "key": key, "subject": subject})
		return syncSubscriptions(tx, now)
	})
	return found && err == nil, err
}

// effective is the single definition of "holds the entitlement right now",
// computed from the stored `until` alone so the answer never depends on when
// the sweep last ran: status active/trial/grace and `until` empty or in the
// future, or (active/grace with grace_seconds) not past until + grace. The
// `entitled()` rule function emits the same predicate in SQL.
func effective(e *core.Record, now time.Time) bool {
	st := e.GetString("status")
	if st != EntActive && st != EntTrial && st != EntGrace {
		return false
	}
	until := e.GetDateTime("until")
	if until.IsZero() || until.Time().After(now) {
		return true
	}
	gs := int64(e.GetInt("grace_seconds"))
	return gs > 0 && (st == EntActive || st == EntGrace) && until.Time().Add(time.Duration(gs)*time.Second).After(now)
}

// Entitled reports whether subject currently holds key (same predicate as the
// `@entitled()` rule function).
func (m *Module) Entitled(subject, subjectCollection, key string) bool {
	e, _ := m.app.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c} && key={:k}",
		dbx.Params{"s": subject, "c": subjectCollection, "k": key})
	return e != nil && effective(e, m.now())
}

// Sweep persists what time already decided: active/trial past `until` ->
// grace (when grace_seconds is set and not over) or lapsed; grace past
// until + grace -> lapsed. `until` stays the end of the paid period. Access
// never depends on the sweep (see effective), a late sweep neither grants nor
// removes grace.
func (m *Module) Sweep() (int, error) {
	now := m.now()
	nowStr := m.dt(now).String()
	n := 0
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		var rs []*core.Record
		for off := 0; ; off += 500 {
			page, err := tx.FindRecordsByFilter(EntitlementsCollection,
				"(status='active' || status='trial' || status='grace') && until!='' && until<{:n}", "until,id", 500, off, dbx.Params{"n": nowStr})
			if err != nil {
				return err
			}
			rs = append(rs, page...)
			if len(page) < 500 {
				break
			}
		}
		for _, e := range rs {
			cur := e.GetString("status")
			until := e.GetDateTime("until").Time()
			next := EntLapsed
			if gs := int64(e.GetInt("grace_seconds")); gs > 0 && (cur == EntActive || cur == EntGrace) &&
				until.Add(time.Duration(gs)*time.Second).After(now) {
				next = EntGrace
			}
			if next == cur {
				continue
			}
			e.Set("status", next)
			if err := tx.Save(e); err != nil {
				return err
			}
			n++
			audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{"action": next, "key": e.GetString("key"), "subject": e.GetString("subject")})
		}
		return syncSubscriptions(tx, now)
	})
	return n, err
}
