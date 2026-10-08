//go:build !no_payments

package payments

import (
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
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

func (m *Module) revokeForIntent(tx kernel.App, intent *core.Record, now time.Time) error {
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

// syncSubscriptions marks subscriptions expired when their entitlement lapsed.
func syncSubscriptions(tx kernel.App, now time.Time) error {
	subs, err := tx.FindRecordsByFilter(SubscriptionsCollection, "status='active' || status='trial'", "", 0, 0)
	if err != nil {
		return err
	}
	for _, s := range subs {
		e, _ := tx.FindFirstRecordByFilter(EntitlementsCollection, "source_subscription={:s}", dbx.Params{"s": s.Id})
		if e != nil && e.GetString("status") == EntLapsed {
			s.Set("status", "expired")
			if err := tx.Save(s); err != nil {
				return err
			}
		}
	}
	return nil
}

// GrantEntitlement grants (or extends) an entitlement by hand (CLI, Go API).
func (m *Module) GrantEntitlement(subject, subjectCollection string, g Grant, status string) (*core.Record, error) {
	if subject == "" || g.Key == "" {
		return nil, fmt.Errorf("%w: subject and key are required", ErrInvalid)
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

// RevokeEntitlement lapses an entitlement immediately.
func (m *Module) RevokeEntitlement(subject, subjectCollection, key string) (bool, error) {
	e, _ := m.app.FindFirstRecordByFilter(EntitlementsCollection, "subject={:s} && subject_collection={:c} && key={:k}",
		dbx.Params{"s": subject, "c": subjectCollection, "k": key})
	if e == nil {
		return false, nil
	}
	e.Set("status", EntLapsed)
	e.Set("until", m.now())
	if err := m.app.Save(e); err != nil {
		return false, err
	}
	audit(ActionEntitlement, EntitlementsCollection, e.Id, map[string]any{"action": "revoke", "key": key, "subject": subject})
	return true, nil
}

// Entitled reports whether subject currently holds key (same predicate as the
// `@entitled()` rule function).
func (m *Module) Entitled(subject, subjectCollection, key string) bool {
	e, _ := m.app.FindFirstRecordByFilter(EntitlementsCollection,
		"subject={:s} && subject_collection={:c} && key={:k} && (status='active' || status='trial' || status='grace') && (until='' || until>{:n})",
		dbx.Params{"s": subject, "c": subjectCollection, "k": key, "n": m.dt(m.now()).String()})
	return e != nil
}

// Sweep moves expired entitlements along active/trial -> grace -> lapsed. The
// rule function already honors `until`, so a late sweep never grants access.
func (m *Module) Sweep() (int, error) {
	now := m.now()
	nowStr := m.dt(now).String()
	n := 0
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		rs, err := tx.FindRecordsByFilter(EntitlementsCollection,
			"(status='active' || status='trial' || status='grace') && until!='' && until<{:n}", "", 500, 0, dbx.Params{"n": nowStr})
		if err != nil {
			return err
		}
		for _, e := range rs {
			cur := e.GetString("status")
			until := e.GetDateTime("until").Time()
			next := EntLapsed
			if cur == EntActive || cur == EntTrial {
				if gs := int64(e.GetInt("grace_seconds")); gs > 0 && cur == EntActive {
					if gu := until.Add(time.Duration(gs) * time.Second); gu.After(now) {
						next = EntGrace
						e.Set("until", gu)
					}
				}
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
