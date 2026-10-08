//go:build !no_payments

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// orphanGrace is how long an event whose intent is unknown is retried (the
// webhook can beat the save of provider_ref by a few milliseconds).
const orphanGrace = 15 * time.Minute

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
	case "processed", "ignored", "rejected", "invalid":
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
			_ = m.finishEvent(ev, "failed", "no matching intent yet")
			return errors.New("payments: no matching intent yet")
		}
		audit(ActionRejected, EventsCollection, ev.Id, map[string]any{"reason": "no matching intent", "provider": provider})
		return m.finishEvent(ev, "rejected", "no matching intent")
	}
	ev.Set("intent", intent.Id)

	if we.Type == EventPaid || we.Type == EventRefunded {
		if we.Currency != "" && we.Currency != intent.GetString("currency") ||
			(we.Type == EventPaid && we.Amount > 0 && we.Amount != int64(intent.GetInt("amount"))) {
			audit(ActionMismatch, IntentsCollection, intent.Id, map[string]any{"event": ev.Id, "amount": we.Amount, "currency": we.Currency})
			return m.finishEvent(ev, "rejected", "amount or currency does not match the intent")
		}
	}

	perr := m.applyEvent(ctx, intent, &we)
	switch {
	case perr == nil:
		return m.finishEvent(ev, "processed", "")
	case errors.Is(perr, ErrIllegalTransition):
		audit(ActionRejected, IntentsCollection, intent.Id, map[string]any{"event": ev.Id, "type": we.Type, "error": perr.Error()})
		return m.finishEvent(ev, "rejected", perr.Error())
	default:
		_ = m.finishEvent(ev, "failed", perr.Error())
		return perr
	}
}

func (m *Module) applyEvent(ctx context.Context, intent *core.Record, we *WebhookEvent) error {
	info := TransitionInfo{Source: "webhook", Data: we.Data}
	switch we.Type {
	case EventPaid:
		_, err := m.Transition(intent.Id, StatusPaid, info)
		return err
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
// the echo of a refund created through the API.
func (m *Module) applyProviderRefund(intent *core.Record, we *WebhookEvent) error {
	ref, _ := we.Data["refund_ref"].(string)
	if ref != "" {
		if rf, _ := m.app.FindFirstRecordByFilter(RefundsCollection, "provider_ref={:r}", dbx.Params{"r": ref}); rf != nil {
			if rf.GetString("status") == "succeeded" {
				return nil
			}
			return m.completeRefund(rf, "webhook")
		}
		col, err := m.app.FindCachedCollectionByNameOrId(RefundsCollection)
		if err != nil {
			return err
		}
		rf := core.NewRecord(col)
		rf.Set("intent", intent.Id)
		rf.Set("amount", we.Amount)
		rf.Set("currency", intent.GetString("currency"))
		rf.Set("status", "pending")
		rf.Set("source", "provider")
		rf.Set("provider_ref", ref)
		if err := m.app.Save(rf); err != nil {
			return err
		}
		return m.completeRefund(rf, "webhook")
	}
	return m.applyRefund(intent.Id, we.Amount, "webhook")
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
func (m *Module) applyStatus(intentID string, st *Status, source string) error {
	info := TransitionInfo{Source: source, Data: st.Data}
	var err error
	switch st.State {
	case StatusPaid:
		_, err = m.Transition(intentID, StatusPaid, info)
	case StatusFailed:
		_, err = m.Transition(intentID, StatusFailed, info)
	case StatusExpired:
		_, err = m.Transition(intentID, StatusExpired, info)
	case StatusRefunded, StatusPartiallyRefunded:
		if _, err = m.Transition(intentID, StatusPaid, info); err != nil && !errors.Is(err, ErrIllegalTransition) {
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
		if delta > 0 {
			err = m.applyRefund(intentID, delta, source)
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
}

// Reconcile asks the provider about pending intents older than ReconcileAfter,
// fails abandoned `created` intents, expires pending intents past IntentTTL,
// sweeps lapsed entitlements and prunes rejected webhook attempts.
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
	n, err := m.Sweep()
	rep.Swept = n
	if err != nil {
		return rep, err
	}
	old := m.dt(now.Add(-7 * 24 * time.Hour)).String()
	if inv, err := m.app.FindRecordsByFilter(EventsCollection, "status='invalid' && created<{:o}", "", 500, 0, dbx.Params{"o": old}); err == nil {
		for _, e := range inv {
			if m.app.Delete(e) == nil {
				rep.Pruned++
			}
		}
	}
	return rep, nil
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
	ev.Set("error", "")
	if err := m.app.Save(ev); err != nil {
		return err
	}
	return m.ProcessEvent(ctx, eventID)
}
