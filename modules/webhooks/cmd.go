//go:build !no_webhooks

package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// NewCommand returns the `webhooks` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "webhooks", Short: "Manage outbound webhooks and their deliveries"}

	var listJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List configured webhooks", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			all, err := LoadAll(app)
			if err != nil {
				return err
			}
			if listJSON {
				out := make([]*Webhook, 0, len(all))
				for _, w := range all {
					out = append(out, w.Redacted()) // header values masked, secret never serialized
				}
				return printJSON(c.OutOrStdout(), out)
			}
			for _, w := range all {
				state := "enabled"
				if !w.Enabled {
					state = "disabled"
				}
				cols := "*"
				if len(w.Collections) > 0 {
					cols = strings.Join(w.Collections, ",")
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tevents=%s\tcollections=%s\t%s\n",
					w.Name, state, strings.Join(w.Events, ","), cols, w.URL)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "output JSON (secret never printed, header values masked)")

	var name, url, secret, events, collections string
	add := &cobra.Command{
		Use: "add", Short: "Add a webhook", SilenceUsage: true,
		Example: `  toki webhooks add --name crm --url https://example.com/hook --events record.create,record.update --collections orders`,
		RunE: func(c *cobra.Command, _ []string) error {
			gen := secret == ""
			w, err := Add(app, Webhook{
				Name: name, URL: url, Secret: secret,
				Events: normalizeEvents(events), Collections: normalizeEvents(collections),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "added webhook %q (%s)\n", w.Name, w.ID)
			if gen {
				fmt.Fprintf(c.OutOrStdout(), "secret (shown once): %s\n", w.Secret)
			}
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "unique webhook name (required)")
	add.Flags().StringVar(&url, "url", "", "receiver URL, http(s) (required)")
	add.Flags().StringVar(&secret, "secret", "", "HMAC secret, at least 16 characters (generated and printed when empty; prefer generated, argv shows in shell history)")
	add.Flags().StringVar(&events, "events", "", "comma separated events, e.g. record.create,collection.*,auth.login (required)")
	add.Flags().StringVar(&collections, "collections", "", "comma separated collection names (empty = all)")
	_ = add.MarkFlagRequired("name")
	_ = add.MarkFlagRequired("url")
	_ = add.MarkFlagRequired("events")

	rm := &cobra.Command{
		Use: "rm <name>", Short: "Remove a webhook", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := Remove(app, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "removed webhook %q\n", args[0])
			return nil
		},
	}

	test := &cobra.Command{
		Use: "test <name>", Short: "Send a ping event synchronously and print the result", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			w, err := Find(app, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(w.TimeoutMs)*time.Millisecond+5*time.Second)
			defer cancel()
			d, err := Ping(ctx, app, w)
			if err != nil {
				return err
			}
			if d.State == StateDelivered {
				fmt.Fprintf(c.OutOrStdout(), "ok: HTTP %d in %d ms (delivery %s)\n", d.LastStatus, d.ResponseMs, d.Id)
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "failed: HTTP %d: %s (delivery %s)\n", d.LastStatus, d.LastError, d.Id)
			return errors.New("ping failed")
		},
	}

	var state string
	var limit int
	var delJSON bool
	deliveries := &cobra.Command{
		Use: "deliveries", Short: "List recent deliveries", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows, err := ListDeliveries(app, state, limit)
			if err != nil {
				return err
			}
			if delJSON {
				if rows == nil {
					rows = []Delivery{}
				}
				return printJSON(c.OutOrStdout(), rows)
			}
			for _, d := range rows {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\tattempt=%d\tstatus=%d\t%s\t%s\n",
					d.Id, d.State, d.Event, d.Attempt, d.LastStatus, d.Created, oneLine(d.LastError))
			}
			return nil
		},
	}
	deliveries.Flags().StringVar(&state, "state", "", "filter: queued|delivered|failed|dead")
	deliveries.Flags().IntVar(&limit, "limit", 50, "maximum rows")
	deliveries.Flags().BoolVar(&delJSON, "json", false, "output JSON")

	var dead, now, force bool
	replay := &cobra.Command{
		Use: "replay [delivery-id]", Short: "Re-queue a delivery, or every dead one with --dead", Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if dead == (len(args) == 1) {
				return errors.New("give either a delivery id or --dead")
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			if err := initTable(app); err != nil {
				return err
			}
			replayFn := Replay
			if force {
				replayFn = ReplayForce
			}
			n, err := replayFn(app, id, now && id != "")
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "re-queued %d delivery(ies)\n", n)
			if now && id != "" {
				// claim it so a running server does not deliver it twice
				if err := deliver(context.Background(), app, id); err != nil {
					return err
				}
				d, err := getDelivery(app, id)
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "%s: HTTP %d %s\n", d.State, d.LastStatus, oneLine(d.LastError))
			} else {
				fmt.Fprintln(c.OutOrStdout(), "a running server delivers them within seconds (use --now to deliver one synchronously)")
			}
			return nil
		},
	}
	replay.Flags().BoolVar(&dead, "dead", false, "re-queue all dead deliveries")
	replay.Flags().BoolVar(&force, "force", false, "also replay delivered or in-flight deliveries (may cause a duplicate)")
	replay.Flags().BoolVar(&now, "now", false, "deliver a single delivery synchronously from this process")

	root.AddCommand(list, add, rm, test, deliveries, replay)
	return root
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
