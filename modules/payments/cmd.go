//go:build !no_payments

package payments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/pocketbase/dbx"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// NewCommand returns the `payments` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "payments", Short: "Payment intents, reconciliation, entitlements and webhook events"}
	mod := func() (*Module, error) {
		if m := For(app); m != nil {
			return m, nil
		}
		return nil, errors.New("payments is not registered (TOKI_PAYMENTS=off?)")
	}

	// intents
	intents := &cobra.Command{Use: "intents", Short: "Inspect payment intents"}
	var status string
	var limit int
	var asJSON, attention bool
	list := &cobra.Command{
		Use: "list", Short: "List intents, newest first", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			filter, params := "id != ''", dbx.Params{}
			if status != "" {
				filter, params = "status={:s}", dbx.Params{"s": status}
			}
			var rs []*core.Record
			var err error
			if attention {
				rs, err = attentionIntents(app, limit)
			} else {
				rs, err = app.FindRecordsByFilter(IntentsCollection, filter, "-created", limit, 0, params)
			}
			if err != nil {
				return err
			}
			if asJSON {
				out := make([]map[string]any, 0, len(rs))
				for _, r := range rs {
					out = append(out, PublicIntent(r))
				}
				return printJSON(c.OutOrStdout(), out)
			}
			for _, r := range rs {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%d %s\t%s\t%s\n", r.Id, r.GetString("status"), r.GetString("provider"),
					r.GetInt("amount"), r.GetString("currency"), r.GetString("order_ref"), r.GetString("created"))
			}
			return nil
		},
	}
	list.Flags().StringVar(&status, "status", "", "filter by status")
	list.Flags().IntVar(&limit, "limit", 50, "max rows")
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	list.Flags().BoolVar(&attention, "attention", false, "only intents that need a human: paid_late (money for a failed/expired intent), intents of rejected or dead-lettered webhook events")
	resolve := &cobra.Command{
		Use: "resolve <id>", Short: "Accept a paid_late payment: the intent becomes paid and its entitlement is granted (to refund instead use `payments refund`)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			r, err := m.GetIntent(a[0])
			if err != nil {
				return err
			}
			if r.GetString("status") != StatusPaidLate {
				return fmt.Errorf("intent is %s, only paid_late intents can be resolved", r.GetString("status"))
			}
			if _, err := m.Transition(a[0], StatusPaid, TransitionInfo{Source: "cli"}); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "paid")
			return nil
		},
	}
	var refAmount int64
	var refReason, refKey string
	refund := &cobra.Command{
		Use: "refund <intent-id>", Short: "Refund (part of) a paid or paid_late intent through its provider", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			rf, err := m.Refund(context.Background(), a[0], refAmount, refReason, refKey)
			if rf != nil {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%d\n", rf.Id, rf.GetString("status"), rf.GetInt("amount"))
			}
			return err
		},
	}
	refund.Flags().Int64Var(&refAmount, "amount", 0, "minor unit amount (0 = everything left)")
	refund.Flags().StringVar(&refReason, "reason", "", "reason")
	refund.Flags().StringVar(&refKey, "idempotency-key", "", "idempotency key")
	show := &cobra.Command{
		Use: "show <id>", Short: "Show one intent with its events and refunds (no credentials are ever stored)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			r, err := app.FindRecordById(IntentsCollection, a[0])
			if err != nil {
				return ErrNotFound
			}
			evs, _ := app.FindRecordsByFilter(EventsCollection, "intent={:i}", "created", 100, 0, dbx.Params{"i": r.Id})
			rfs, _ := app.FindRecordsByFilter(RefundsCollection, "intent={:i}", "created", 100, 0, dbx.Params{"i": r.Id})
			out := map[string]any{"intent": r, "events": evs, "refunds": rfs}
			return printJSON(c.OutOrStdout(), out)
		},
	}
	intents.AddCommand(list, show, resolve)

	reconcile := &cobra.Command{
		Use: "reconcile", Short: "Check pending intents with their providers and sweep lapsed entitlements", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			rep, err := m.Reconcile(context.Background())
			if perr := printJSON(c.OutOrStdout(), rep); perr != nil {
				return perr
			}
			return err
		},
	}

	// entitlements
	ents := &cobra.Command{Use: "entitlements", Short: "Grant, revoke and list entitlements"}
	var subCol, entStatus string
	var days, graceDays int
	var quota, balance int64
	grant := &cobra.Command{
		Use: "grant <subject-id> <key>", Short: "Grant or extend an entitlement", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			e, err := m.GrantEntitlement(a[0], subCol, Grant{Key: a[1], Days: days, GraceDays: graceDays, Quota: quota, Balance: balance}, entStatus)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tuntil=%s\n", e.Id, e.GetString("status"), e.GetString("until"))
			return nil
		},
	}
	grant.Flags().StringVar(&subCol, "collection", "users", "auth collection of the subject")
	grant.Flags().StringVar(&entStatus, "status", "active", "active or trial (a trial needs a subject without that entitlement)")
	grant.Flags().IntVar(&days, "days", 0, "duration in days (0 = no end)")
	grant.Flags().IntVar(&graceDays, "grace-days", 0, "grace period after the end")
	grant.Flags().Int64Var(&quota, "quota", 0, "set the quota")
	grant.Flags().Int64Var(&balance, "balance", 0, "add to the balance")
	var rvCol string
	revoke := &cobra.Command{
		Use: "revoke <subject-id> <key>", Short: "Lapse an entitlement immediately", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			ok, err := m.RevokeEntitlement(a[0], rvCol, a[1])
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("no such entitlement")
			}
			fmt.Fprintln(c.OutOrStdout(), "revoked")
			return nil
		},
	}
	revoke.Flags().StringVar(&rvCol, "collection", "users", "auth collection of the subject")
	var lsSubject, lsKey string
	lsEnt := &cobra.Command{
		Use: "list", Short: "List entitlements", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			filter, params := "id != ''", dbx.Params{}
			if lsSubject != "" {
				filter += " && subject={:s}"
				params["s"] = lsSubject
			}
			if lsKey != "" {
				filter += " && key={:k}"
				params["k"] = lsKey
			}
			rs, err := app.FindRecordsByFilter(EntitlementsCollection, filter, "-updated", 200, 0, params)
			if err != nil {
				return err
			}
			for _, r := range rs {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s/%s\t%s\t%s\tuntil=%s\tquota=%d\tbalance=%d\n", r.Id, r.GetString("subject_collection"),
					r.GetString("subject"), r.GetString("key"), r.GetString("status"), r.GetString("until"), r.GetInt("quota"), r.GetInt("balance"))
			}
			return nil
		},
	}
	lsEnt.Flags().StringVar(&lsSubject, "subject", "", "filter by subject id")
	lsEnt.Flags().StringVar(&lsKey, "key", "", "filter by key")
	ents.AddCommand(grant, revoke, lsEnt)

	// webhook replay
	wh := &cobra.Command{Use: "webhook", Short: "Webhook events"}
	var evStatus string
	var evLimit int
	whList := &cobra.Command{
		Use: "list", Short: "List stored webhook events (use --status rejected|dead|failed|pending_order to find the ones that need a decision)", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			filter, params := "id != ''", dbx.Params{}
			if evStatus != "" {
				filter, params = "status={:s}", dbx.Params{"s": evStatus}
			}
			rs, err := app.FindRecordsByFilter(EventsCollection, filter, "-created", evLimit, 0, params)
			if err != nil {
				return err
			}
			for _, r := range rs {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\tintent=%s\t%s\n", r.Id, r.GetString("status"), r.GetString("provider"), r.GetString("type"), r.GetString("intent"), r.GetString("error"))
			}
			return nil
		},
	}
	whList.Flags().StringVar(&evStatus, "status", "", "filter by event status")
	whList.Flags().IntVar(&evLimit, "limit", 50, "max rows")
	wh.AddCommand(whList)
	wh.AddCommand(&cobra.Command{
		Use: "replay <eventId>", Short: "Process a stored verified event again (idempotent)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, a []string) error {
			m, err := mod()
			if err != nil {
				return err
			}
			if err := m.ReplayEvent(context.Background(), a[0]); err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), "replayed")
			return nil
		},
	})

	root.AddCommand(intents, reconcile, refund, ents, wh)
	return root
}

// attentionIntents lists the intents a human has to look at: paid_late ones
// and the intents of webhook events that were rejected or dead-lettered.
func attentionIntents(app core.App, limit int) ([]*core.Record, error) {
	out, err := app.FindRecordsByFilter(IntentsCollection, "status='paid_late'", "-updated", limit, 0)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, r := range out {
		seen[r.Id] = true
	}
	evs, err := app.FindRecordsByFilter(EventsCollection, "(status='rejected' || status='dead') && intent!=''", "-created", 200, 0)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		id := e.GetString("intent")
		if seen[id] || len(out) >= limit {
			continue
		}
		if r, err := app.FindRecordById(IntentsCollection, id); err == nil {
			seen[id] = true
			out = append(out, r)
		}
	}
	return out, nil
}
