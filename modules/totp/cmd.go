package totp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

func resolveUser(app core.App, collection, user string) (*core.Record, error) {
	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil || !col.IsAuth() {
		return nil, errors.New("unknown auth collection " + collection)
	}
	if r, err := app.FindRecordById(col, user); err == nil {
		return r, nil
	}
	if r, err := app.FindAuthRecordByEmail(col, strings.TrimSpace(user)); err == nil {
		return r, nil
	}
	return nil, errors.New("record not found: " + user)
}

// NewCommand returns the `totp` cobra command (status, disable, enforce).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "totp", Short: "Inspect and manage TOTP two-factor authentication"}

	status := &cobra.Command{
		Use: "status <collection> <user>", Short: "Show the TOTP state of one auth record (id or email)", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := resolveUser(app, args[0], args[1])
			if err != nil {
				return err
			}
			s := StatusOf(app, rec)
			w := c.OutOrStdout()
			fmt.Fprintf(w, "record\t%s\nconfigured\t%v\nenabled\t%v\nrecovery_codes_left\t%d\nlast_used\t%s\ncreated\t%s\n",
				rec.Id, s.Exists, s.Enabled, s.RecoveryLeft, s.LastUsed, s.Created)
			cfg := loadEnforcement(app)
			fmt.Fprintf(w, "enforced_for_user\t%v\n", cfg.active() && cfg.matches(rec))
			return nil
		},
	}

	disable := &cobra.Command{
		Use: "disable <collection> <user>", Short: "Break-glass: remove TOTP and recovery codes of one auth record (audited)", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			rec, err := resolveUser(app, args[0], args[1])
			if err != nil {
				return err
			}
			ok, err := Disable(app, rec)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintln(c.OutOrStdout(), "TOTP was not set up for this record")
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "TOTP disabled for %s\n", rec.Id)
			return nil
		},
	}

	var roles string
	var supers, off, regrace bool
	enforce := &cobra.Command{
		Use: "enforce", Short: "Require TOTP for roles and/or superusers (stored in _params; env vars add to it)", Args: cobra.NoArgs, SilenceUsage: true,
		Long: "Writes the enforcement set. The grace window (" + EnvGraceDays + " days) starts when enforcement turns on;\n" +
			"--restart-grace restarts it. Use --off to clear the stored set (env settings stay in effect).",
		RunE: func(c *cobra.Command, _ []string) error {
			w := c.OutOrStdout()
			if !off && roles == "" && !supers && !regrace {
				cfg := loadEnforcement(app)
				at, ok := EnforcedAt(app)
				fmt.Fprintf(w, "roles\t%s\nsuperusers\t%v\nenforced_at\t%v (%v)\ngrace_days\t%d\n", strings.Join(cfg.Roles, ","), cfg.Superusers, at, ok, graceDays())
				return nil
			}
			cfg := Enforcement{}
			if !off {
				cfg = Enforcement{Roles: splitRoles(roles), Superusers: supers}
			}
			SetEnforcement(app, cfg, regrace)
			fmt.Fprintf(w, "enforcement set: roles=%s superusers=%v\n", strings.Join(cfg.Roles, ","), cfg.Superusers)
			return nil
		},
	}
	enforce.Flags().StringVar(&roles, "roles", "", "comma list of role values (field `roles` or `role`)")
	enforce.Flags().BoolVar(&supers, "superusers", false, "require TOTP for _superusers")
	enforce.Flags().BoolVar(&off, "off", false, "clear the stored enforcement")
	enforce.Flags().BoolVar(&regrace, "restart-grace", false, "restart the grace window now")

	root.AddCommand(status, disable, enforce)
	return root
}
