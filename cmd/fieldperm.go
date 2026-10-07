//go:build !no_fieldperm

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/fieldperm"
)

// NewFieldPermCommand creates the `fieldperm` command (list, set, rm, lint)
// backed by modules/fieldperm.
func NewFieldPermCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "fieldperm",
		Short: "Manage per-field read and write rules (_field_rules)",
		Long: "Per-field rules use the same language as collection rules. They can only narrow access:\n" +
			"a field rule never grants more than the collection rule already does.\n" +
			"Read:  inherit = no field rule, public = always readable, <rule> = hidden from the response when false.\n" +
			"Write: inherit = no field rule, locked = superusers only, <rule> = checked when the field is in the request body.\n" +
			"A running server picks up CLI changes within 30 seconds.",
	}

	var asJSON bool
	list := &cobra.Command{
		Use: "list [collection]", Short: "List field rules", Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			coll := ""
			if len(args) == 1 {
				coll = args[0]
			}
			rules, err := fieldperm.List(app, coll)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rules)
			}
			for _, r := range rules {
				fmt.Fprintf(c.OutOrStdout(), "%s.%s\tread=%s\twrite=%s", r.Collection, r.Field, showRead(r.Read), showWrite(r.Write))
				if r.Note != "" {
					fmt.Fprintf(c.OutOrStdout(), "\t# %s", r.Note)
				}
				fmt.Fprintln(c.OutOrStdout())
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var read, write, note string
	set := &cobra.Command{
		Use:          "set <collection> <field>",
		Short:        "Create or update the rule of one field",
		Example:      "fieldperm set clans leader --write '@request.auth.id = leader' --read public",
		Args:         cobra.ExactArgs(2),
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			o := fieldperm.SetOptions{}
			if c.Flags().Changed("read") {
				o.ReadSet = true
				switch read {
				case "inherit":
				case "public":
					o.Read = ptr("")
				default:
					o.Read = ptr(read)
				}
			}
			if c.Flags().Changed("write") {
				o.WriteSet = true
				switch write {
				case "inherit":
				case "locked":
					o.Write = ptr("")
				default:
					o.Write = ptr(write)
				}
			}
			if c.Flags().Changed("note") {
				o.Note = &note
			}
			if !o.ReadSet && !o.WriteSet && o.Note == nil {
				return errors.New("nothing to set: pass --read, --write or --note")
			}
			r, err := fieldperm.Set(app, args[0], args[1], o)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "%s.%s\tread=%s\twrite=%s\n", r.Collection, r.Field, showRead(r.Read), showWrite(r.Write))
			return nil
		},
	}
	set.Flags().StringVar(&read, "read", "", "read rule: inherit | public | <rule>")
	set.Flags().StringVar(&write, "write", "", "write rule: inherit | locked | <rule>")
	set.Flags().StringVar(&note, "note", "", "free text note")

	rm := &cobra.Command{
		Use: "rm <collection> <field>", Short: "Delete the rule of one field", Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			ok, err := fieldperm.Remove(app, args[0], args[1])
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(c.OutOrStdout(), "no rule for %s.%s\n", args[0], args[1])
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "removed %s.%s\n", args[0], args[1])
			return nil
		},
	}

	var lintJSON bool
	lint := &cobra.Command{
		Use:          "lint",
		Short:        "Check that rules parse against their collection and name existing fields",
		Long:         "Exit code 1 when there is any finding.",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			findings, err := fieldperm.Lint(app)
			if err != nil {
				return err
			}
			if lintJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(findings); err != nil {
					return err
				}
			} else {
				for _, f := range findings {
					fmt.Fprintf(c.OutOrStdout(), "%s\t%s.%s\t%s\t%s\n", f.Severity, f.Collection, f.Field, f.Kind, f.Message)
				}
				if len(findings) == 0 {
					fmt.Fprintln(c.OutOrStdout(), "ok")
				}
			}
			if len(findings) > 0 {
				return fmt.Errorf("%d finding(s)", len(findings))
			}
			return nil
		},
	}
	lint.Flags().BoolVar(&lintJSON, "json", false, "output JSON")

	root.AddCommand(list, set, rm, lint)
	return root
}

func ptr(s string) *string { return &s }

func showRead(r *string) string {
	switch {
	case r == nil:
		return "inherit"
	case *r == "":
		return "public"
	}
	return strings.TrimSpace(*r)
}

func showWrite(r *string) string {
	switch {
	case r == nil:
		return "inherit"
	case *r == "":
		return "locked"
	}
	return strings.TrimSpace(*r)
}
