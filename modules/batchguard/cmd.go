//go:build !no_batchguard

package batchguard

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewCommand returns the `batch` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "batch",
		Short: "Cross-record validation of atomic /api/batch calls",
		Long: "Rules in _batch_rules see every sub-request of a batch. `assert` runs before the\n" +
			"sub-requests, `assert_post` after them on the stored records, both inside the batch\n" +
			"transaction (a failure rolls everything back and answers 400).",
	}
	rules := &cobra.Command{Use: "rules", Short: "Manage batch rules"}
	root.AddCommand(rules)

	boot := func() error {
		if !app.IsBootstrapped() {
			if err := app.Bootstrap(); err != nil {
				return err
			}
		}
		return EnsureCollection(app)
	}

	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List rules", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := boot(); err != nil {
				return err
			}
			rs, err := List(app)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(rs, "", "  ")
				fmt.Fprintln(c.OutOrStdout(), string(b))
				return nil
			}
			for _, r := range rs {
				state := "on"
				if !r.Enabled {
					state = "off"
				}
				var ms []string
				for _, m := range r.Match {
					ms = append(ms, m.Collection+":"+m.Method)
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\tmatch=%s\tassert=%q\tassert_post=%q\n", r.Name, state, strings.Join(ms, ","), r.Assert, r.AssertPost)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var (
		matchSpec           []string
		asrt, asrtPost, msg string
		disabled            bool
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create or replace a rule (expressions are validated)",
		Example: `batch rules add checkout --match orders:POST --match order_items:POST \
    --assert 'sum(order_items, qty) == req(0).body.total_qty' --message 'Order total does not match its items'`,
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := boot(); err != nil {
				return err
			}
			r := Rule{Name: args[0], Enabled: !disabled, Assert: asrt, AssertPost: asrtPost, Message: msg}
			for _, s := range matchSpec {
				coll, method, _ := strings.Cut(s, ":")
				r.Match = append(r.Match, Match{Collection: coll, Method: strings.ToUpper(method)})
			}
			out, err := Save(app, r)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "ok %s (%d match entries)\n", out.Name, len(out.Match))
			return nil
		},
	}
	add.Flags().StringArrayVar(&matchSpec, "match", nil, "collection[:METHOD] that must be in the batch (repeatable)")
	add.Flags().StringVar(&asrt, "assert", "", "expression evaluated before the sub-requests")
	add.Flags().StringVar(&asrtPost, "assert-post", "", "expression evaluated after the sub-requests, on the stored records")
	add.Flags().StringVar(&msg, "message", "", "error message returned to the client")
	add.Flags().BoolVar(&disabled, "disabled", false, "create the rule disabled")

	rm := &cobra.Command{
		Use: "rm <name>", Short: "Delete a rule", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := boot(); err != nil {
				return err
			}
			ok, err := Remove(app, args[0])
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("no such rule")
			}
			fmt.Fprintln(c.OutOrStdout(), "removed")
			return nil
		},
	}

	var file, only string
	test := &cobra.Command{
		Use:   "test --file batch.json",
		Short: "Dry run: evaluate the `assert` of every applicable rule against a batch body (nothing is executed)",
		Long: "The file is a /api/batch body: {\"requests\":[{\"method\":\"POST\",\"url\":\"/api/collections/orders/records\",\"body\":{...}}]}.\n" +
			"Only `assert` runs: `assert_post` needs the stored records and is skipped. Exit code 1 when a rule fails.",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("--file is required")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			var body struct {
				Requests []*core.InternalRequest `json:"requests"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				return fmt.Errorf("invalid batch file: %w", err)
			}
			if err := boot(); err != nil {
				return err
			}
			rs, err := List(app)
			if err != nil {
				return err
			}
			reqs := parseRequests(app, body.Requests)
			m := &Module{app: app, timeout: EvalTimeout}
			failed := 0
			for _, r := range rs {
				if only != "" && r.Name != only {
					continue
				}
				if !applies(app, r, reqs) {
					fmt.Fprintf(c.OutOrStdout(), "skip  %s (not applicable)\n", r.Name)
					continue
				}
				if r.Assert == "" {
					fmt.Fprintf(c.OutOrStdout(), "skip  %s (only assert_post)\n", r.Name)
					continue
				}
				start := time.Now()
				if err := m.check(app, r, r.Assert, "assert", reqs, nil); err != nil {
					failed++
					fmt.Fprintf(c.OutOrStdout(), "FAIL  %s: %s\n", r.Name, err)
					continue
				}
				fmt.Fprintf(c.OutOrStdout(), "pass  %s (%s)\n", r.Name, time.Since(start).Round(time.Microsecond))
			}
			if failed > 0 {
				return fmt.Errorf("%d rule(s) failed", failed)
			}
			return nil
		},
	}
	test.Flags().StringVar(&file, "file", "", "batch body JSON file")
	test.Flags().StringVar(&only, "rule", "", "only this rule")

	rules.AddCommand(list, add, rm, test)
	return root
}
