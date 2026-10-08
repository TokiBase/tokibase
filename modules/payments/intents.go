//go:build !no_payments

package payments

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Errors of the intent API.
var (
	ErrIdempotencyConflict = errors.New("payments: idempotency key was used with different parameters")
	ErrInvalid             = errors.New("payments: invalid request")
	ErrNotFound            = errors.New("payments: not found")
	ErrTooManyIntents      = errors.New("payments: too many open intents, try again later")
)

// MaxAmount is the largest accepted amount in the minor unit (1e12: far above
// any real price and exactly representable in the float64 column).
const MaxAmount = 1_000_000_000_000

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

// Grant describes what a paid intent entitles the customer to. It is set by
// server side code or by a `_payment_products` row, never by a client.
type Grant struct {
	Key          string `json:"key"`
	Days         int    `json:"days"` // 0 = no end
	GraceDays    int    `json:"grace_days"`
	Quota        int64  `json:"quota"`
	Balance      int64  `json:"balance"`
	Subscription bool   `json:"subscription"`
	Product      string `json:"product,omitempty"`
}

// CreateParams are the inputs of CreateIntent.
type CreateParams struct {
	Provider       string
	Amount         int64
	Currency       string
	OrderRef       string
	Description    string
	Product        string // slug in _payment_products
	IdempotencyKey string
	Customer       *core.Record // auth record, nil for server side intents
	Metadata       map[string]any
	// Grant overrides the product grant (server side only).
	Grant *Grant
}

func customerOf(c *core.Record) (id, col string) {
	if c == nil {
		return "", ""
	}
	return c.Id, c.Collection().Name
}

// CreateIntent validates, deduplicates by idempotency key, persists the intent
// and creates the checkout at the provider. replay is true when an existing
// intent with the same idempotency key was returned.
func (m *Module) CreateIntent(ctx context.Context, p CreateParams) (rec *core.Record, replay bool, err error) {
	p.Provider = strings.ToLower(strings.TrimSpace(p.Provider))
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	prov, perr := m.Provider(p.Provider)
	if perr != nil {
		return nil, false, fmt.Errorf("%w: provider %q is not configured", ErrInvalid, p.Provider)
	}
	if len(p.OrderRef) > 200 || len(p.Description) > 500 || len(p.IdempotencyKey) > 200 {
		return nil, false, fmt.Errorf("%w: field too long", ErrInvalid)
	}

	grant := p.Grant
	if p.Product != "" {
		prod, perr := m.app.FindFirstRecordByFilter(ProductsCollection, "slug={:s} && enabled=true", dbx.Params{"s": p.Product})
		if perr != nil {
			return nil, false, fmt.Errorf("%w: unknown product", ErrInvalid)
		}
		// The price and currency of a product are always the product's. A
		// product without a fixed price cannot be bought (fail closed): the
		// client must never pick what it pays for a product that grants access.
		pa, pc := int64(prod.GetInt("amount")), strings.ToUpper(strings.TrimSpace(prod.GetString("currency")))
		if pa <= 0 || !currencyRe.MatchString(pc) {
			return nil, false, fmt.Errorf("%w: product has no fixed price and currency", ErrInvalid)
		}
		if p.Amount != 0 && p.Amount != pa {
			return nil, false, fmt.Errorf("%w: amount does not match the product price", ErrInvalid)
		}
		if p.Currency != "" && p.Currency != pc {
			return nil, false, fmt.Errorf("%w: currency does not match the product", ErrInvalid)
		}
		p.Amount, p.Currency = pa, pc
		if grant == nil && prod.GetString("entitlement_key") != "" {
			grant = &Grant{
				Key: prod.GetString("entitlement_key"), Days: prod.GetInt("duration_days"),
				GraceDays: prod.GetInt("grace_days"), Quota: int64(prod.GetInt("quota")),
				Balance: int64(prod.GetInt("balance_add")), Subscription: prod.GetBool("subscription"),
				Product: p.Product,
			}
		}
	}
	if p.Amount <= 0 {
		return nil, false, fmt.Errorf("%w: amount must be a positive integer in the minor unit", ErrInvalid)
	}
	if p.Amount > MaxAmount {
		return nil, false, fmt.Errorf("%w: amount is too large", ErrInvalid)
	}
	if !currencyRe.MatchString(p.Currency) {
		return nil, false, fmt.Errorf("%w: currency must be a 3 letter ISO code", ErrInvalid)
	}
	if grant != nil && (grant.Key == "" || grant.Days < 0 || grant.GraceDays < 0) {
		return nil, false, fmt.Errorf("%w: bad grant", ErrInvalid)
	}

	cid, ccol := customerOf(p.Customer)
	if p.IdempotencyKey != "" {
		ex, _ := m.app.FindFirstRecordByFilter(IntentsCollection,
			"customer={:c} && customer_collection={:cc} && idempotency_key={:k}",
			dbx.Params{"c": cid, "cc": ccol, "k": p.IdempotencyKey})
		if ex != nil {
			return m.replayOf(ex, p)
		}
	}

	if p.Customer != nil {
		if err := m.checkIntentBrakes(cid, ccol); err != nil {
			return nil, false, err
		}
	}

	col, err := m.app.FindCachedCollectionByNameOrId(IntentsCollection)
	if err != nil {
		return nil, false, err
	}
	rec = core.NewRecord(col)
	rec.Set("provider", p.Provider)
	rec.Set("status", StatusCreated)
	rec.Set("amount", p.Amount)
	rec.Set("currency", p.Currency)
	rec.Set("idempotency_key", p.IdempotencyKey)
	rec.Set("order_ref", p.OrderRef)
	rec.Set("customer", cid)
	rec.Set("customer_collection", ccol)
	rec.Set("product", p.Product)
	rec.Set("description", p.Description)
	rec.Set("metadata", nonNil(p.Metadata))
	if grant != nil {
		rec.Set("grant", grant)
	}
	if err := m.app.Save(rec); err != nil {
		if p.IdempotencyKey != "" && isUnique(err) { // lost a race with the same key
			if ex, _ := m.app.FindFirstRecordByFilter(IntentsCollection,
				"customer={:c} && customer_collection={:cc} && idempotency_key={:k}",
				dbx.Params{"c": cid, "cc": ccol, "k": p.IdempotencyKey}); ex != nil {
				return m.replayOf(ex, p)
			}
		}
		return nil, false, err
	}
	audit(ActionStatus, IntentsCollection, rec.Id, map[string]any{"from": "", "to": StatusCreated, "source": "api"})

	spec := &IntentSpec{
		ID: rec.Id, Amount: p.Amount, Currency: p.Currency, OrderRef: p.OrderRef, Description: p.Description,
		CustomerID: cid, Metadata: nonNil(p.Metadata),
	}
	if p.Customer != nil {
		spec.CustomerEmail = p.Customer.GetString("email")
		spec.CustomerName = p.Customer.GetString("name")
	}
	url, ref, cerr := prov.CreateIntent(ctx, spec)
	if cerr != nil {
		_, _ = m.Transition(rec.Id, StatusFailed, TransitionInfo{Source: "api", Error: cerr.Error()})
		return nil, false, fmt.Errorf("payments: provider %s: %w", p.Provider, cerr)
	}
	out, _, uerr := m.update(rec.Id, "api", func(tx kernel.App, r *core.Record, now time.Time) error {
		r.Set("checkout_url", url)
		r.Set("provider_ref", ref)
		if r.GetString("status") == StatusCreated {
			r.Set("status", StatusPending)
		}
		return nil
	})
	if uerr != nil {
		return nil, false, uerr
	}
	return out, false, nil
}

// checkIntentBrakes is the default flood protection per customer, active even
// when no `payments:intent` rate limit rule is configured: at most
// IntentPerMinute new intents per minute and MaxOpenIntents created/pending ones.
func (m *Module) checkIntentBrakes(cid, ccol string) error {
	who := dbx.HashExp{"customer": cid, "customer_collection": ccol}
	if m.IntentPerMinute > 0 {
		n, err := m.app.CountRecords(IntentsCollection, who, dbx.NewExp("[[created]] > {:t}", dbx.Params{"t": m.dt(m.now().Add(-time.Minute)).String()}))
		if err == nil && n >= int64(m.IntentPerMinute) {
			return ErrTooManyIntents
		}
	}
	if m.MaxOpenIntents > 0 {
		n, err := m.app.CountRecords(IntentsCollection, who, dbx.NewExp("[[status]] IN ('created','pending')"))
		if err == nil && n >= int64(m.MaxOpenIntents) {
			return ErrTooManyIntents
		}
	}
	return nil
}

func (m *Module) replayOf(ex *core.Record, p CreateParams) (*core.Record, bool, error) {
	if ex.GetString("provider") != p.Provider || int64(ex.GetInt("amount")) != p.Amount ||
		ex.GetString("currency") != p.Currency || ex.GetString("order_ref") != p.OrderRef ||
		ex.GetString("product") != p.Product {
		return nil, false, ErrIdempotencyConflict
	}
	return ex, true, nil
}

func nonNil(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// GetIntent loads an intent by id.
func (m *Module) GetIntent(id string) (*core.Record, error) {
	r, err := m.app.FindRecordById(IntentsCollection, id)
	if err != nil {
		return nil, ErrNotFound
	}
	return r, nil
}

// findIntentByRefs finds an intent of provider whose provider_ref (a comma
// separated list, first element canonical) contains one of refs exactly.
func (m *Module) findIntentByRefs(provider string, refs []string) *core.Record {
	for _, ref := range refs {
		if ref == "" || strings.Contains(ref, ",") {
			continue
		}
		// exact match first: it can use idx_payint_ref
		if r, err := m.app.FindFirstRecordByFilter(IntentsCollection, "provider={:p} && provider_ref={:r}", dbx.Params{"p": provider, "r": ref}); err == nil && r != nil {
			return r
		}
		rs, err := m.app.FindRecordsByFilter(IntentsCollection,
			"provider={:p} && provider_ref ~ {:r}", "-created", 20, 0, dbx.Params{"p": provider, "r": ref})
		if err != nil {
			continue
		}
		for _, r := range rs { // the ~ operator is a substring match: confirm the element
			for _, e := range strings.Split(r.GetString("provider_ref"), ",") {
				if e == ref {
					return r
				}
			}
		}
	}
	return nil
}

// TransitionInfo carries context of a status change.
type TransitionInfo struct {
	Source string         // webhook|reconcile|api|cli
	Error  string         // stored in last_error
	Data   map[string]any // merged into provider_data
	Detail map[string]any // extra audit details
	// Reopen allows failed|expired -> paid for a verified, matching late
	// payment (set by the framework, see applyPaid; not for callers).
	Reopen bool
}

// Transition moves an intent to status `to`. The same status is an idempotent
// no-op (changed=false); an illegal move returns ErrIllegalTransition. Moving
// to paid grants the entitlements and triggers OnPaid after commit; moving to
// refunded revokes the entitlements granted by the intent.
func (m *Module) Transition(id, to string, info TransitionInfo) (changed bool, err error) {
	rec, from, uerr := m.update(id, info.Source, func(tx kernel.App, r *core.Record, now time.Time) error {
		cur := r.GetString("status")
		if info.Error != "" {
			r.Set("last_error", truncate(info.Error, 1900))
		}
		mergeData(r, info.Data)
		if cur == to {
			return nil
		}
		if err := CheckTransition(cur, to); err != nil {
			if !(info.Reopen && to == StatusPaid && (cur == StatusFailed || cur == StatusExpired)) {
				return err
			}
		}
		r.Set("status", to)
		switch to {
		case StatusPaidLate:
			r.Set("paid_at", now)
		case StatusPaid:
			r.Set("paid_at", now)
			return m.grantForIntent(tx, r, now)
		case StatusRefunded:
			return m.revokeForIntent(tx, r, now)
		}
		return nil
	})
	if uerr != nil {
		return false, uerr
	}
	return rec != nil && from != rec.GetString("status"), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func mergeData(r *core.Record, data map[string]any) {
	if len(data) == 0 {
		return
	}
	cur := map[string]any{}
	_ = r.UnmarshalJSONField("provider_data", &cur)
	for k, v := range data {
		if k == "ref_add" { // extra provider id to match later webhooks by
			add, _ := v.(string)
			refs := strings.Split(r.GetString("provider_ref"), ",")
			if add != "" && !slices.Contains(refs, add) {
				r.Set("provider_ref", strings.Trim(strings.Join(append(refs, add), ","), ","))
			}
			continue
		}
		cur[k] = v
	}
	r.Set("provider_data", cur)
}

// update runs fn on the intent inside a transaction, saves it and, after the
// commit, audits a status change and fires OnPaid.
func (m *Module) update(id, source string, fn func(tx kernel.App, r *core.Record, now time.Time) error) (*core.Record, string, error) {
	var from, to string
	var out *core.Record
	now := m.now()
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		r, err := tx.FindRecordById(IntentsCollection, id)
		if err != nil {
			return ErrNotFound
		}
		from = r.GetString("status")
		if err := fn(tx, r, now); err != nil {
			return err
		}
		to = r.GetString("status")
		if err := tx.Save(r); err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return nil, from, err
	}
	if to != from {
		d := map[string]any{"from": from, "to": to, "source": source}
		audit(ActionStatus, IntentsCollection, id, d)
		late := from == StatusFailed || from == StatusExpired || from == StatusPaidLate
		if late && (to == StatusPaid || to == StatusPaidLate) {
			// money for an intent that had given up: loud on purpose
			m.app.Logger().Error("payments: late payment", "intent", id, "from", from, "to", to, "source", source)
			audit(ActionLate, IntentsCollection, id, map[string]any{"level": "error", "from": from, "to": to, "source": source})
		}
		switch to {
		case StatusPaid:
			ev := &PaidEvent{App: m.app, Intent: out, Late: late}
			if herr := OnPaid(m.app).Trigger(ev); herr != nil {
				m.app.Logger().Error("payments: OnPaid handler failed", "intent", id, "error", herr)
			}
		case StatusPaidLate:
			ev := &PaidEvent{App: m.app, Intent: out, Late: true}
			if herr := OnLatePayment(m.app).Trigger(ev); herr != nil {
				m.app.Logger().Error("payments: OnLatePayment handler failed", "intent", id, "error", herr)
			}
		}
	}
	return out, from, nil
}

// ---- refunds ---------------------------------------------------------------

func pendingRefundSum(app kernel.App, intentID string) int64 {
	rs, _ := app.FindRecordsByFilter(RefundsCollection, "intent={:i} && status='pending'", "", 0, 0, dbx.Params{"i": intentID})
	var n int64
	for _, r := range rs {
		n += int64(r.GetInt("amount"))
	}
	return n
}

func refundable(status string) bool {
	return status == StatusPaid || status == StatusPartiallyRefunded || status == StatusPaidLate
}

// Refund asks the provider to refund amount (0 = everything left) of a paid
// intent (or a paid_late one: refunding is the way to send late money back).
// A provider that completes later reports through a webhook. The remaining
// amount is checked and the pending row inserted in one transaction, so two
// concurrent refunds cannot both pass the check.
func (m *Module) Refund(ctx context.Context, intentID string, amount int64, reason, idemKey string) (*core.Record, error) {
	intent, err := m.GetIntent(intentID)
	if err != nil {
		return nil, err
	}
	prov, err := m.Provider(intent.GetString("provider"))
	if err != nil {
		return nil, err
	}
	col, err := m.app.FindCachedCollectionByNameOrId(RefundsCollection)
	if err != nil {
		return nil, err
	}
	var rf, replay *core.Record
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		if idemKey != "" {
			if ex, _ := tx.FindFirstRecordByFilter(RefundsCollection, "intent={:i} && idempotency_key={:k}",
				dbx.Params{"i": intentID, "k": idemKey}); ex != nil {
				replay = ex
				return nil
			}
		}
		cur, err := tx.FindRecordById(IntentsCollection, intentID)
		if err != nil {
			return ErrNotFound
		}
		if st := cur.GetString("status"); !refundable(st) {
			return fmt.Errorf("%w: intent is %s, only paid intents can be refunded", ErrInvalid, st)
		}
		intent = cur
		total := int64(cur.GetInt("amount"))
		remaining := total - int64(cur.GetInt("refunded_amount")) - pendingRefundSum(tx, intentID)
		if amount == 0 {
			amount = remaining
		}
		if amount <= 0 || amount > remaining {
			return fmt.Errorf("%w: refund amount must be within 1..%d", ErrInvalid, remaining)
		}
		rf = core.NewRecord(col)
		rf.Set("intent", intentID)
		rf.Set("amount", amount)
		rf.Set("currency", cur.GetString("currency"))
		rf.Set("status", "pending")
		rf.Set("source", "api")
		rf.Set("reason", truncate(reason, 900))
		rf.Set("idempotency_key", idemKey)
		return tx.Save(rf)
	})
	if err != nil {
		return nil, err
	}
	if replay != nil {
		return replay, nil
	}
	audit(ActionRefund, RefundsCollection, rf.Id, map[string]any{"intent": intentID, "amount": amount, "status": "pending"})

	data := map[string]any{}
	_ = intent.UnmarshalJSONField("provider_data", &data)
	res, rerr := prov.Refund(ctx, &RefundSpec{
		RefundID: rf.Id, IntentID: intentID, ProviderRef: intent.GetString("provider_ref"), Data: data,
		Amount: amount, Currency: intent.GetString("currency"), Reason: reason,
	})
	if rerr != nil {
		rf.Set("status", "failed")
		rf.Set("error", truncate(rerr.Error(), 1900))
		_ = m.app.Save(rf)
		audit(ActionRefund, RefundsCollection, rf.Id, map[string]any{"intent": intentID, "amount": amount, "status": "failed"})
		return rf, rerr
	}
	if res.ProviderRef != "" {
		// the webhook echo may have beaten us here and already counted this refund
		if dup, _ := m.app.FindFirstRecordByFilter(RefundsCollection, "provider_ref={:r} && id!={:id}",
			dbx.Params{"r": res.ProviderRef, "id": rf.Id}); dup != nil {
			return m.mergeIntoEcho(rf, dup)
		}
		rf.Set("provider_ref", res.ProviderRef)
	}
	if err := m.app.Save(rf); err != nil {
		if isUnique(err) { // the echo created its row between the check and the save
			if dup, _ := m.app.FindFirstRecordByFilter(RefundsCollection, "provider_ref={:r} && id!={:id}",
				dbx.Params{"r": res.ProviderRef, "id": rf.Id}); dup != nil {
				return m.mergeIntoEcho(rf, dup)
			}
		}
		return rf, err
	}
	if res.Done {
		if err := m.completeRefund(rf, "api"); err != nil {
			return rf, err
		}
		rf.Set("status", "succeeded")
	}
	return rf, nil
}

// mergeIntoEcho settles an API refund row whose provider refund was already
// recorded (and counted) from the webhook: it is closed without counting again.
func (m *Module) mergeIntoEcho(rf, echo *core.Record) (*core.Record, error) {
	rf.Set("provider_ref", "")
	rf.Set("status", "succeeded")
	rf.Set("error", "counted by provider row "+echo.Id)
	audit(ActionRefund, RefundsCollection, rf.Id, map[string]any{"intent": rf.GetString("intent"), "status": "merged", "into": echo.Id})
	return rf, m.app.Save(rf)
}

// completeRefund marks a refund row succeeded and counts it on the intent,
// exactly once: the status flip is a compare-and-set in SQL, so a webhook echo
// racing the API answer (or a retried job) cannot count the same refund twice.
func (m *Module) completeRefund(rf *core.Record, source string) error {
	nd := m.dt(m.now()).String()
	res, err := m.app.DB().NewQuery("UPDATE {{" + RefundsCollection + "}} SET [[status]]='succeeded', [[updated]]={:u} WHERE [[id]]={:id} AND [[status]]!='succeeded'").
		Bind(dbx.Params{"u": nd, "id": rf.Id}).Execute()
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // somebody else counted it
	}
	prev := rf.GetString("status")
	rf.Set("status", "succeeded")
	audit(ActionRefund, RefundsCollection, rf.Id, map[string]any{"intent": rf.GetString("intent"), "amount": rf.GetInt("amount"), "status": "succeeded", "source": source})
	if err := m.applyRefund(rf.GetString("intent"), int64(rf.GetInt("amount")), source); err != nil {
		// not counted: put the row back so a retry can count it
		_, _ = m.app.DB().NewQuery("UPDATE {{" + RefundsCollection + "}} SET [[status]]={:s} WHERE [[id]]={:id}").Bind(dbx.Params{"s": prev, "id": rf.Id}).Execute()
		rf.Set("status", prev)
		return err
	}
	return nil
}

// applyRefund adds amount to refunded_amount (0 = all that is left) and moves
// the intent to partially_refunded or refunded.
func (m *Module) applyRefund(intentID string, amount int64, source string) error {
	_, _, err := m.update(intentID, source, func(tx kernel.App, r *core.Record, now time.Time) error {
		cur := r.GetString("status")
		if cur == StatusRefunded {
			return nil
		}
		if !refundable(cur) {
			return fmt.Errorf("%w: %s -> refund", ErrIllegalTransition, cur)
		}
		total := int64(r.GetInt("amount"))
		done := int64(r.GetInt("refunded_amount"))
		if amount <= 0 {
			amount = total - done
		}
		done += amount
		if done > total {
			done = total
		}
		r.Set("refunded_amount", done)
		to := StatusPartiallyRefunded
		if done >= total {
			to = StatusRefunded
		}
		if err := CheckTransition(cur, to); err != nil {
			return err
		}
		r.Set("status", to)
		if to == StatusRefunded {
			return m.revokeForIntent(tx, r, now)
		}
		return nil
	})
	return err
}

// sweepPendingRefunds marks API refunds the provider never confirmed as
// failed (loudly): the operator must check the provider. A late webhook echo
// can still complete such a row. Returns how many were marked.
func (m *Module) sweepPendingRefunds() int {
	cut := m.dt(m.now().Add(-m.RefundPendingTTL)).String()
	rs, err := m.app.FindRecordsByFilter(RefundsCollection, "status='pending' && source='api' && created<{:c}", "created", 200, 0, dbx.Params{"c": cut})
	if err != nil {
		return 0
	}
	n := 0
	for _, rf := range rs {
		rf.Set("status", "failed")
		rf.Set("error", "not confirmed by the provider in time; check the provider dashboard")
		if m.app.Save(rf) == nil {
			n++
			m.app.Logger().Error("payments: refund not confirmed by the provider", "refund", rf.Id, "intent", rf.GetString("intent"))
			audit(ActionRefund, RefundsCollection, rf.Id, map[string]any{"intent": rf.GetString("intent"), "status": "failed", "level": "error", "reason": "unconfirmed"})
		}
	}
	return n
}
