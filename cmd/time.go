//go:build !no_timelint

package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/timelint"
)

// NewTimeCommand creates the `time` command (lint) backed by modules/timelint.
func NewTimeCommand(app core.App) *cobra.Command {
	command := &cobra.Command{Use: "time", Short: "Inspect date field values"}

	var asJSON bool
	lint := &cobra.Command{
		Use:   "lint",
		Short: "Scan stored date values: per collection/field totals and values at exactly 00:00:00",
		Long: "Scan the stored values of all date fields. Upstream stores every value normalized to UTC, so a missing zone can no longer be seen in storage;\n" +
			"values at exactly 00:00:00 are reported as info because they hint at date-only inputs.\n" +
			"Submitted values are checked at request time (env TOKI_TIMELINT=off|warn|strict).",
		Example:      "time lint --json",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows, err := timelint.Scan(app)
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []timelint.Finding{}
				}
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, r := range rows {
				fmt.Fprintf(c.OutOrStdout(), "info %s.%s: %d value(s), %d at exactly 00:00:00\n", r.Collection, r.Field, r.Total, r.Midnight)
			}
			return nil
		},
	}
	lint.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	command.AddCommand(lint)
	return command
}
