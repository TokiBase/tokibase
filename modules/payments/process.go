//go:build !no_payments

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// orphanGrace is how long an event whose intent is unknown is retried (the
// webhook can beat the save of provider_ref by a few milliseconds). It is a
// little shorter than the job queue's backoff window (8 attempts, 5 s doubling
// = about 10.6 minutes): the last attempts and the reconcile cron both see the
// final verdict (`rejected`, audited) instead of a silently failed event.
const orphanGrace = 10 * time.Minute

var errRefundInFlight = errors.New("payments: an API refund of this intent is still being created, retry")

// retryLater parks a failing event as `failed` for another attempt, or
// dead-letters it (status `dead`, audited at error level, no more retries) once
// it failed MaxEventAttempts times.
func (m *Module) retryLater(ev *core.Record, err error) error {
	if m.MaxEventAttempts > 0 && ev.GetInt("attempts") >= m.MaxEventAttempts {
		m.app.Logger().Error("payments: event dead-lettered", "event", ev.Id, "attempts", ev.GetInt("attempts"), "error", err)
		audit(ActionDead, EventsCollection, ev.Id, map[string]any{"level": "error", "attempts": ev.GetInt("attempts"), "error": truncate(err.Error(), 300)})
		return m.finishEvent(ev, "dead", err.Error())
	}
	_ = m.finishEvent(ev, "failed", err.Error())
	return err
}

func (m *Module) handleProcessJob(ctx context.Context, _ kernel.App, job *kernel.Job) error {
	var p struct {
		Event string `json:"event"`
	}
	if err := json.Unmarshal(job.Payload, &p); err != nil || p.Event == "" {
		return nil // malformed payload can never succeed
	}
	return m.ProcessEvent(ctx, p.Event)
}

func (m *Module) handleReconcileJob(ctx context.Context, _ kernel.App, _ *kernel.Job) error {
	_, err := m.Reconcile(ctx)
	return err
}

func (m *Module) finishEvent(ev *core.Record, status, errMsg string) error {
	ev.Set("status", status)
	ev.Set("error", truncate(errMsg, 1900))
	if status == "processed" || status == "ignored" || status == "rejected" {
		ev.Set("processed_at", m.now())
	}
	return m.app.Save(ev)
}

// ProcessEvent applies one stored, verified event to its intent. It is
// idempotent: finished events are skipped, status changes to the current
// status are no-ops, so job retries and replays are safe.
func (m *Module) ProcessEvent(ctx context.Context, eventID string) error {
	ev, err := m.app.FindRecordById(EventsCollection, eventID)
	if err != nil {
		return nil
	}
	switch ev.GetString("status") {
	case "processed", "ignored", "rejected", "invalid", "dead":
		return nil
	}
	if !ev.GetBool("verified") {
		return nil
	}
	ev.Set("attempts", ev.GetInt("attempts")+1)

	var we WebhookEvent
	if err := ev.UnmarshalJSONField("event", &we); err != nil {
		return m.finishEvent(ev, "rejected", "unreadable event: "+err.Error())
	}
	provider := ev.GetString("provider")

	var intent *core.Record
	if we.IntentID != "" {
		if r, err := m.app.FindRecordById(IntentsCollection, we.IntentID); err == nil && r.GetString("provider") == provider {
			intent = r
		}
	}
	if intent == nil {
		intent = m.findIntentByRefs(provider, append([]string{we.ProviderRef}, we.AltRefs...))
	}
	if intent == nil {
		if m.now().Sub(ev.GetDateTime("created").Time()) < orphanGrace {
			return m.retryLater(ev, errors.New("no matching intent yet"))
		}
		audit(ActionRejected, EventsCollection, ev.Id, map[string]any{"reason": "no matching intent", "provider": provider})
		return m.finishEvent(ev, "rejected", "no matching intent")
	}
	ev.Set("intent", intent.Id)

	if !eventMatchesIntent(&we, intent) {
		audit(ActionMismatch, IntentsCollection, intent.Id, map[string]any{"event": ev.Id, "amount": we.Amount, "currency": we.Currency})
		return m.finishEvent(ev, "rejected", "amount or currency does not match the intent")
	}

	// A refund that overtook the payment (queue backlog, replay, outage) is
	// parked, not rejected: the reconcile pass (and the paid event itself)
	// processes it again once the intent is paid.
	if we.Type == EventRefunded {
		if st := intent.GetString("status"); st == StatusCreated || st == StatusPending {
			if m.now().Sub(ev.GetDateTime("created").Time()) > m.IntentTTL {
				audit(ActionRejected, IntentsCollection, intent.Id, map[string]any{"event": ev.Id, "type": we.Type, "error": "refund for an intent that never got paid"})
				return m.finishEvent(ev, "rejected", "refund for an intent that never got paid")
			}
			return m.finishEvent(ev, "pending_order", "refund arrived before the payment")
		}
	}

	perr := m.applyEvent(ctx, intent, &we)
	switch {
	case perr == nil:
		if err := m.finishEvent(ev, "processed", ""); err != nil {
			return err
		}
		if we.Type == EventPaid {
			m.retryParked(ctx, intent.Id)
		}
		return nil
	case errors.Is(perr, ErrIllegalTransition):
		audit(ActionRejected, IntentsCollection, intent.Id, map[string]any{"event": ev.Id, "type": we.Type, "error": perr.Error()})
		return m.finishEvent(ev, "rejected", perr.Error())
	default:
		return m.retryLater(ev, perr)
	}
}

// eventMatchesIntent is the amount/currency gate. A paid event must carry the
// intent's exact amount AND currency: a missing or zero amount never matches.
func eventMatchesIntent(we *WebhookEvent, intent *core.Record) bool {
	cur := intent.GetString("currency")
	switch we.Type {
	case EventPaid:
		return we.Amount > 0 && we.Amount == int64(intent.GetInt("amount")) && strings.EqualFold(we.Currency, cur)
	case EventRefunded:
		return we.Currency == "" || strings.EqualFold(we.Currency, cur)
	}
	return true
}

// retryParked processes the refund events that waited for this intent to be paid.
func (m *Module) retryParked(ctx context.Context, intentID string) {
	rs, err := m.app.FindRecordsByFilter(EventsCollection, "status='pending_order' && intent={:i}", "created", 50, 0, dbx.Params{"i": intentID})
	if err != nil {
		return
	}
	for _, r := range rs {
		_ = m.ProcessEvent(ctx, r.Id)
	}
}

// refsOverlap reports whether the event names the intent by a provider
// reference the intent already holds (not merely by the echoed intent id).
func refsOverlap(intent *core.Record, we *WebhookEvent) bool {
	have := strings.Split(intent.GetString("provider_ref"), ",")
	for _, r := range append([]string{we.ProviderRef}, we.AltRefs...) {
		if r == "" {
			continue
		}
		for _, h := range have {
			if h == r {
				return true
			}
		}
	}
	return false
}

// applyPaid moves an intent to paid. For an intent that already failed or
// expired the payment is never dropped: inside LateWindow and with a matching
// provider reference it re-opens to paid (entitlement granted, OnPaid with
// Late=true); otherwise it becomes paid_late and waits for an operator
// (`toki payments intents resolve|--attention`, or a refund).
func (m *Module) applyPaid(intentID string, info TransitionInfo, refMatch bool) error {
	r, err := m.GetIntent(intentID)
	if err != nil {
		return err
	}
	switch r.GetString("status") {
	case StatusFailed, StatusExpired:
		if refMatch && m.now().Sub(r.GetDateTime("updated").Time()) <= m.LateWindow {
			info.Reopen = true
			_, err = m.Transition(intentID, StatusPaid, info)
		} else {
			_, err = m.Transition(intentID, StatusPaidLate, info)
		}
		return err
	}
	_, err = m.Transition(intentID, StatusPaid, info)
	return err
}

func (m *Module) applyEvent(ctx context.Context, intent *core.Record, we *WebhookEvent) error {
	info := TransitionInfo{Source: "webhook", Data: we.Data}
	switch we.Type {
	case EventPaid:
		return m.applyPaid(intent.Id, info, refsOverlap(intent, we))
	case EventFailed:
		_, err := m.Transition(intent.Id, StatusFailed, info)
		return err
	case EventExpired:
		_, err := m.Transition(intent.Id, StatusExpired, info)
		return err
	case EventRefunded:
		return m.applyProviderRefund(intent, we)
	case EventApproved:
		return m.capture(ctx, intent, "webhook")
	}
	return nil
}

// applyProviderRefund counts a refund reported by the provider unless it is
// the echo of a refund created through the API (matched by provider refund id).
// Each provider refund id is counted once, however often it is delivered.
func (m *Module) applyProviderRefund(intent *core.Record, we *WebhookEvent) error {
	ref, _ := we.Data["refund_ref"].(string)
	if ref == "" {
		return m.applyRefund(intent.Id, we.Amount, "webhook")
	}
	if rf, _ := m.app.FindFirstRecordByFilter(RefundsCollection, "provider_ref={:r}", dbx.Params{"r": ref}); rf != nil {
		return m.completeRefund(rf, "webhook") // no-op when already succeeded
	}
	// An API refund whose provider id is not saved yet may be exactly this
	// refund: wait (job retry) instead of counting it a second time.
	cut := m.dt(m.now().Add(-10 * time.Minute)).String()
	if n, _ := m.app.CountRecords(RefundsCollection, dbx.HashExp{"intent": intent.Id, "source": "api", "status": "pending", "provider_ref": ""},
		dbx.NewExp("[[created]] > {:c}", dbx.Params{"c": cut})); n > 0 {
		return errRefundInFlight
	}
	return m.recordProviderRefund(intent, we.Amount, ref, "webhook")
}

// recordProviderRefund inserts a source=provider refund row keyed by the
// provider's refund id and counts it. The unique index on provider_ref makes a
// concurrent duplicate collapse into the existing row.
func (m *Module) recordProviderRefund(intent *core.Record, amount int64, ref, source string) error {
	if amount <= 0 { // 0 = the whole payment, as documented for EventRefunded
		amount = int64(intent.GetInt("amount")) - int64(intent.GetInt("refunded_amount"))
	}
	col, err := m.app.FindCachedCollectionByNameOrId(RefundsCollection)
	if err != nil {
		return err
	}
	rf := core.NewRecord(col)
	rf.Set("intent", intent.Id)
	rf.Set("amount", amount)
	rf.Set("currency", intent.GetString("currency"))
	rf.Set("status", "pending")
	rf.Set("source", "provider")
	rf.Set("provider_ref", ref)
	if err := m.app.Save(rf); err != nil {
		if isUnique(err) {
			if ex, _ := m.app.FindFirstRecordByFilter(RefundsCollection, "provider_ref={:r}", dbx.Params{"r": ref}); ex != nil {
				return m.completeRefund(ex, source)
			}
		}
		return err
	}
	return m.completeRefund(rf, source)
}

// capture runs the merchant capture step of providers that need one.
func (m *Module) capture(ctx context.Context, intent *core.Record, source string) error {
	prov, err := m.Provider(intent.GetString("provider"))
	if err != nil {
		return err
	}
	c, ok := prov.(Capturer)
	if !ok {
		return nil
	}
	data := map[string]any{}
	_ = intent.UnmarshalJSONField("provider_data", &data)
	st, err := c.Capture(ctx, intent.GetString("provider_ref"), data)
	if err != nil {
		return err
	}
	return m.applyStatus(intent.Id, st, source)
}

// applyStatus applies a provider status (reconciliation, capture result).
// A paid (or refunded) status must carry the intent's exact amount and
// currency, like a webhook; anything else is an ErrAmountMismatch and changes nothing.
func (m *Module) applyStatus(intentID string, st *Status, source string) error {
	info := TransitionInfo{Source: source, Data: st.Data}
	switch st.State {
	case StatusPaid, StatusRefunded, StatusPartiallyRefunded:
		r, err := m.GetIntent(intentID)
		if err != nil {
			return err
		}
		if st.Amount <= 0 || st.Amount != int64(r.GetInt("amount")) ||
			(st.Currency != "" && !strings.EqualFold(st.Currency, r.GetString("currency"))) {
			audit(ActionMismatch, IntentsCollection, intentID, map[string]any{"source": source, "amount": st.Amount, "currency": st.Currency})
			return fmt.Errorf("%w (provider status %s, amount %d)", ErrAmountMismatch, st.State, st.Amount)
		}
	}
	var err error
	switch st.State {
	case StatusPaid:
		err = m.applyPaid(intentID, info, true)
	case StatusFailed:
		_, err = m.Transition(intentID, StatusFailed, info)
	case StatusExpired:
		_, err = m.Transition(intentID, StatusExpired, info)
	case StatusRefunded, StatusPartiallyRefunded:
		if err = m.applyPaid(intentID, info, true); err != nil && !errors.Is(err, ErrIllegalTransition) {
			return err
		}
		err = nil
		r, gerr := m.GetIntent(intentID)
		if gerr != nil {
			return gerr
		}
		delta := st.RefundedAmount - int64(r.GetInt("refunded_amount"))
		if st.State == StatusRefunded {
			delta = int64(r.GetInt("amount")) - int64(r.GetInt("refunded_amount"))
		}
		if delta > 0 && pendingRefundSum(m.app, intentID) == 0 {
			// pending API refunds are completed by their own echo; counting the
			// provider total now would count them twice
			err = m.recordProviderRefund(r, delta, fmt.Sprintf("reconcile:%s:%d", intentID, r.GetInt("refunded_amount")+int(delta)), source)
		}
	}
	return err
}

// ReconcileReport counts what a reconciliation pass did.
type ReconcileReport struct {
	Checked int `json:"checked"`
	Changed int `json:"changed"`
	Errors  int `json:"errors"`
	Swept   int `json:"swept"`
	Pruned  int `json:"pruned"`
	Retried int `json:"retried"` // stored events processed again (parked refunds, failed, never dispatched)
}

// Reconcile asks the provider about pending intents older than ReconcileAfter,
// fails abandoned `created` intents, expires pending intents past IntentTTL,
// re-checks recently paid intents for refunds, retries stored webhook events
// that did not finish (parked refunds, `received`/`failed`), marks unconfirmed
// API refunds, sweeps lapsed entitlements and prunes rejected webhook attempts.
func (m *Module) Reconcile(ctx context.Context) (ReconcileReport, error) {
	var rep ReconcileReport
	now := m.now()
	cutoff := m.dt(now.Add(-m.ReconcileAfter)).String()
	rs, err := m.app.FindRecordsByFilter(IntentsCollection, "(status='pending' || status='created') && created<{:c}", "created", 200, 0, dbx.Params{"c": cutoff})
	if err != nil {
		return rep, err
	}
	for _, r := range rs {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.Checked++
		before := r.GetString("status")
		if before == StatusCreated && r.GetString("provider_ref") == "" {
			if ch, err := m.Transition(r.Id, StatusFailed, TransitionInfo{Source: "reconcile", Error: "abandoned before the provider answered"}); err == nil && ch {
				rep.Changed++
			}
			continue
		}
		prov, err := m.Provider(r.GetString("provider"))
		if err != nil {
			rep.Errors++
			continue
		}
		data := map[string]any{}
		_ = r.UnmarshalJSONField("provider_data", &data)
		st, err := prov.FetchStatus(ctx, r.GetString("provider_ref"), data)
		if err != nil {
			rep.Errors++
			m.app.Logger().Warn("payments: reconcile fetch failed", "intent", r.Id, "error", err)
			continue
		}
		if st.State == "approved" {
			if err := m.capture(ctx, r, "reconcile"); err != nil {
				rep.Errors++
			}
		} else if st.State == StatusPending || st.State == "" {
			if now.Sub(r.GetDateTime("created").Time()) > m.IntentTTL {
				_, _ = m.Transition(r.Id, StatusExpired, TransitionInfo{Source: "reconcile", Error: "never completed"})
			}
		} else if err := m.applyStatus(r.Id, st, "reconcile"); err != nil {
			rep.Errors++
			m.app.Logger().Warn("payments: reconcile apply failed", "intent", r.Id, "error", err)
		}
		if after, _ := m.GetIntent(r.Id); after != nil && after.GetString("status") != before {
			rep.Changed++
		}
	}

	// paid intents changed within the last 14 days: did the provider refund them behind our back?
	recent := m.dt(now.Add(-14 * 24 * time.Hour)).String()
	paid, _ := m.app.FindRecordsByFilter(IntentsCollection, "(status='paid' || status='partially_refunded') && updated>{:u}", "-updated", 50, 0, dbx.Params{"u": recent})
	for _, r := range paid {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		prov, err := m.Provider(r.GetString("provider"))
		if err != nil {
			continue
		}
		data := map[string]any{}
		_ = r.UnmarshalJSONField("provider_data", &data)
		st, err := prov.FetchStatus(ctx, r.GetString("provider_ref"), data)
		if err != nil {
			rep.Errors++
			continue
		}
		rep.Checked++
		if (st.State == StatusRefunded || st.State == StatusPartiallyRefunded) && st.RefundedAmount > int64(r.GetInt("refunded_amount")) {
			if err := m.applyStatus(r.Id, st, "reconcile"); err != nil {
				rep.Errors++
				continue
			}
			rep.Changed++
		}
	}

	// stored events that did not finish
	evCut := m.dt(now.Add(-2 * time.Minute)).String()
	evs, _ := m.app.FindRecordsByFilter(EventsCollection, "(status='pending_order' || status='received' || status='failed') && created<{:c}", "created", 200, 0, dbx.Params{"c": evCut})
	for _, ev := range evs {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		rep.Retried++
		_ = m.ProcessEvent(ctx, ev.Id)
	}

	m.sweepPendingRefunds()
	n, err := m.Sweep()
	rep.Swept = n
	if err != nil {
		return rep, err
	}
	rep.Pruned = m.pruneInvalid(now)
	return rep, nil
}

// pruneInvalid deletes rejected-webhook traces older than a day and keeps at
// most 1000 of them, each with one statement (no row-at-a-time loop).
func (m *Module) pruneInvalid(now time.Time) int {
	total := 0
	del := func(q string, p dbx.Params) {
		if res, err := m.app.DB().NewQuery(q).Bind(p).Execute(); err == nil {
			n, _ := res.RowsAffected()
			total += int(n)
		}
	}
	t := "{{" + EventsCollection + "}}"
	del("DELETE FROM "+t+" WHERE [[status]]='invalid' AND [[created]] < {:o}", dbx.Params{"o": m.dt(now.Add(-24 * time.Hour)).String()})
	del("DELETE FROM "+t+" WHERE [[status]]='invalid' AND [[id]] NOT IN (SELECT [[id]] FROM "+t+" WHERE [[status]]='invalid' ORDER BY [[created]] DESC LIMIT 1000)", nil)
	return total
}

// ReplayEvent resets a verified event and processes it again (idempotent).
func (m *Module) ReplayEvent(ctx context.Context, eventID string) error {
	ev, err := m.app.FindRecordById(EventsCollection, eventID)
	if err != nil {
		return ErrNotFound
	}
	if !ev.GetBool("verified") {
		return fmt.Errorf("%w: event was not verified, refusing to replay", ErrInvalid)
	}
	ev.Set("status", "received")
	ev.Set("attempts", 0)
	ev.Set("error", "")
	if err := m.app.Save(ev); err != nil {
		return err
	}
	return m.ProcessEvent(ctx, eventID)
}
