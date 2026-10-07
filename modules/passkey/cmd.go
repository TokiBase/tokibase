package passkey

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewCommand returns the `passkey` cobra command (list, rm, config).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "passkey", Short: "Inspect and manage WebAuthn passkeys"}

	list := &cobra.Command{
		Use: "list <collection> <user>", Short: "List the passkeys of one auth record (id or email)", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			rows, err := List(app, args[0], args[1])
			if err != nil {
				return err
			}
			for _, r := range rows {
				flag := ""
				if r.GetBool("clone_suspected") {
					flag = "\tCLONE_SUSPECTED"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tsign_count=%d\tlast_used=%s\tcreated=%s%s\n",
					r.Id, r.GetString("name"), r.GetInt("sign_count"), r.GetDateTime("last_used").String(), r.GetDateTime("created").String(), flag)
			}
			return nil
		},
	}

	rm := &cobra.Command{
		Use: "rm <id>", Short: "Delete one passkey by id", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			r, err := Remove(app, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "deleted passkey %s (%s)\n", r.Id, r.GetString("name"))
			return nil
		},
	}

	cfg := &cobra.Command{
		Use: "config", Short: "Print the effective relying party configuration", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			cf := LoadConfig()
			w := c.OutOrStdout()
			fmt.Fprintf(w, "active\t%v\n", cf.Active())
			fmt.Fprintf(w, "rp_id\t%s\t(env %s)\n", cf.RPID, EnvRPID)
			fmt.Fprintf(w, "rp_name\t%s\t(env %s)\n", cf.RPName, EnvRPName)
			fmt.Fprintf(w, "origins\t%s\t(env %s)\n", strings.Join(cf.Origins, ","), EnvOrigins)
			fmt.Fprintf(w, "clone_policy\t%s\t(env %s)\n", cf.ClonePolicy, EnvClone)
			if !cf.Active() {
				fmt.Fprintf(w, "module inactive: set %s (and optionally %s) to enable the endpoints\n", EnvRPID, EnvOrigins)
			} else if _, err := New(app, cf); err != nil {
				fmt.Fprintf(w, "INVALID configuration: %v\n", err)
			}
			return nil
		},
	}

	root.AddCommand(list, rm, cfg)
	return root
}
