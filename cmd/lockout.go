package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/lockout"
)

// NewLockoutCommand returns the `lockout` cobra command (list, unlock, clear).
func NewLockoutCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "lockout", Short: "Inspect and clear per-identity authentication lockouts"}

	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List recorded lockout state", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows, err := lockout.List(app)
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []lockout.Row{}
				}
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			now := time.Now().UTC()
			for _, r := range rows {
				state := "counting"
				if t := lockout.ParseTime(r.LockedUntil); t.After(now) {
					state = "LOCKED until " + r.LockedUntil
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\tfailures=%d\t%s\n", r.Key, r.Failures, state)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	unlock := &cobra.Command{
		Use: "unlock <collection> <identity>", Short: "Unlock one identity", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			ok, err := lockout.Unlock(app, args[0], args[1])
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(c.OutOrStdout(), "no lockout record for %s\n", lockout.Key(args[0], strings.TrimSpace(args[1])))
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "unlocked %s\n", lockout.Key(args[0], args[1]))
			return nil
		},
	}

	clear := &cobra.Command{
		Use: "clear", Short: "Remove all lockout records", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			n, err := lockout.Clear(app)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "cleared %d record(s)\n", n)
			return nil
		},
	}
	root.AddCommand(list, unlock, clear)
	return root
}
