//go:build !no_sessions

package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sessions"
)

// NewSessionsCommand returns the `sessions` cobra command (list, revoke, revoke-all, purge).
func NewSessionsCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "sessions", Short: "Inspect and revoke server-side auth sessions"}

	var asJSON bool
	list := &cobra.Command{
		Use: "list <collection> <user>", Short: "List the sessions of one auth record", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			rows, err := sessions.List(app, args[0], args[1])
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []sessions.Session{}
				}
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, r := range rows {
				state := "active"
				if r.Revoked != "" {
					state = "revoked " + r.Revoked + " (" + r.RevokedReason + ")"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tcreated=%s\tlast_seen=%s\texpires=%s\tip=%s\tdevice=%s\t%s\n",
					r.Id, r.Kind, r.Created, r.LastSeen, r.Expires, r.IP, r.Device, state)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	revoke := &cobra.Command{
		Use: "revoke <id>", Short: "Revoke one session (row id or token id)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			ok, err := sessions.Revoke(app, args[0], "cli")
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(c.OutOrStdout(), "no active session %s\n", args[0])
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}

	revokeAll := &cobra.Command{
		Use: "revoke-all <collection> <user>", Short: "Revoke every active session of one auth record", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			n, err := sessions.RevokeUser(app, args[0], args[1], "cli")
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %d session(s)\n", n)
			return nil
		},
	}

	var expired bool
	purge := &cobra.Command{
		Use: "purge --expired", Short: "Delete sessions whose token already expired", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if !expired {
				return fmt.Errorf("nothing to purge: pass --expired")
			}
			n, err := sessions.PurgeExpired(app)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "purged %d session(s)\n", n)
			return nil
		},
	}
	purge.Flags().BoolVar(&expired, "expired", false, "delete expired sessions")

	root.AddCommand(list, revoke, revokeAll, purge)
	return root
}
