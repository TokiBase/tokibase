//go:build !no_denylog

package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/denylog"
)

// NewDenyCommand creates the `deny` command (tail) backed by modules/denylog.
func NewDenyCommand(app core.App) *cobra.Command {
	command := &cobra.Command{Use: "deny", Short: "Inspect denied (401/403/429) requests"}

	var (
		since  time.Duration
		limit  int
		asJSON bool
	)
	tail := &cobra.Command{
		Use:   "tail",
		Short: "Show the latest 401/403/429 denial log entries (newest first)",
		Long: "Show log entries marked toki.deny=true from the _logs table: status, method, path, ip, auth_kind, auth_id, collection, reason, rule_kind, rate_limited.\n" +
			"Entries are written in batches (about every 3 seconds) and subject to the logs retention settings.",
		Example:      "deny tail --since 1h --limit 20 --json",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows, err := denylog.Tail(app, since, limit)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, r := range rows {
				d := r.Data
				fmt.Fprintf(c.OutOrStdout(), "%s\t%v\t%v %v\tip=%v\tauth=%v:%v\treason=%v", r.Created, d["status"], d["method"], d["path"], d["ip"], d["auth_kind"], d["auth_id"], d["reason"])
				for _, k := range []string{"collection", "rule_kind", "rate_limited"} {
					if v, ok := d[k]; ok {
						fmt.Fprintf(c.OutOrStdout(), "\t%s=%v", k, v)
					}
				}
				fmt.Fprintln(c.OutOrStdout())
			}
			return nil
		},
	}
	tail.Flags().DurationVar(&since, "since", 0, "only entries newer than this duration (e.g. 1h, 30m); default all retained")
	tail.Flags().IntVar(&limit, "limit", 50, "max number of entries")
	tail.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	command.AddCommand(tail)
	return command
}
