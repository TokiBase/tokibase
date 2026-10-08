//go:build !no_sync

package sync

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// PolicyRow is one `_sync_policies` row as `toki sync policies list` prints it.
type PolicyRow struct {
	Collection   string            `json:"collection"`
	Direction    string            `json:"direction"`
	Strategy     string            `json:"strategy"`
	Partition    string            `json:"partition"`
	FieldTypes   map[string]string `json:"field_types"`
	Exclude      []string          `json:"exclude"`
	Hook         string            `json:"hook"`
	PullViewRule bool              `json:"pull_view_rule"`
	Crypto       string            `json:"crypto"`
	Order        int               `json:"order"`
	Enabled      bool              `json:"enabled"`
	Review       bool              `json:"review"`
	Trusted      bool              `json:"trusted"`
}

func policyRow(r *core.Record) PolicyRow {
	row := PolicyRow{
		Collection: r.GetString("collection"), Direction: r.GetString("direction"), Strategy: r.GetString("strategy"),
		Partition: r.GetString("partition"), Hook: r.GetString("hook"), PullViewRule: r.GetBool("pull_view_rule"),
		Crypto: r.GetString("crypto"), Order: r.GetInt("order"), Enabled: r.GetBool("enabled"),
		Review: r.GetBool("review"), Trusted: r.GetBool("trusted"), FieldTypes: map[string]string{}, Exclude: []string{},
	}
	if row.Direction == "" {
		row.Direction = DirBoth
	}
	if row.Strategy == "" {
		row.Strategy = "lww"
	}
	if row.Crypto == "" {
		row.Crypto = "ciphertext"
	}
	if raw := rawJSON(r, "field_types"); raw != nil {
		_ = json.Unmarshal(raw, &row.FieldTypes)
	}
	if raw := rawJSON(r, "exclude"); raw != nil {
		_ = json.Unmarshal(raw, &row.Exclude)
	}
	return row
}

// ListPolicies returns the policies ordered by `order`, then collection.
func ListPolicies(app core.App) ([]PolicyRow, error) {
	recs, err := app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return nil, err
	}
	out := make([]PolicyRow, 0, len(recs))
	for _, r := range recs {
		out = append(out, policyRow(r))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Collection < out[j].Collection
	})
	return out, nil
}

// PolicyChange lists the settings of `policies set`; nil / empty leaves a
// setting as it is (a new policy starts with the defaults of the design).
type PolicyChange struct {
	Direction    *string
	Strategy     *string
	Partition    *string
	Hook         *string
	Crypto       *string
	Order        *int
	Enabled      *bool
	Review       *bool
	Trusted      *bool
	PullViewRule *bool
	// FieldTypes replaces the typed fields when non-nil ("f=counter").
	FieldTypes map[string]string
	// Exclude replaces the exclude list when non-nil.
	Exclude []string
}

// findPolicy finds the policy of a collection by the name or id it was written with
// or resolves to.
func findPolicy(app core.App, ref string) (*core.Record, error) {
	recs, err := app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return nil, err
	}
	col, _ := app.FindCachedCollectionByNameOrId(ref)
	for _, r := range recs {
		pref := r.GetString("collection")
		if pref == ref {
			return r, nil
		}
		if col != nil && (pref == col.Id || pref == col.Name) {
			return r, nil
		}
	}
	return nil, nil
}

// SetPolicy creates or updates the policy of a collection. Validation (the
// same checks as `lint`, errors only) runs in the save hook.
func SetPolicy(app core.App, ref string, ch PolicyChange) (*core.Record, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, errors.New("a collection is required")
	}
	pc, err := app.FindCollectionByNameOrId(PoliciesCollection)
	if err != nil {
		return nil, fmt.Errorf("%s not found (is TOKI_SYNC_ROLE=hub?)", PoliciesCollection)
	}
	rec, err := findPolicy(app, ref)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		rec = core.NewRecord(pc)
		rec.Set("collection", ref)
		rec.Set("direction", DirBoth)
		rec.Set("strategy", "lww")
		rec.Set("crypto", "ciphertext")
		rec.Set("pull_view_rule", true)
		rec.Set("enabled", true)
	}
	if ch.Direction != nil {
		rec.Set("direction", *ch.Direction)
	}
	if ch.Strategy != nil {
		rec.Set("strategy", *ch.Strategy)
	}
	if ch.Partition != nil {
		rec.Set("partition", strings.TrimSpace(*ch.Partition))
	}
	if ch.Hook != nil {
		rec.Set("hook", *ch.Hook)
	}
	if ch.Crypto != nil {
		rec.Set("crypto", *ch.Crypto)
	}
	if ch.Order != nil {
		rec.Set("order", *ch.Order)
	}
	if ch.Enabled != nil {
		rec.Set("enabled", *ch.Enabled)
	}
	if ch.Review != nil {
		rec.Set("review", *ch.Review)
	}
	if ch.Trusted != nil {
		rec.Set("trusted", *ch.Trusted)
	}
	if ch.PullViewRule != nil {
		rec.Set("pull_view_rule", *ch.PullViewRule)
	}
	if ch.FieldTypes != nil {
		rec.Set("field_types", ch.FieldTypes)
	}
	if ch.Exclude != nil {
		rec.Set("exclude", ch.Exclude)
	}
	if err := app.Save(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// RemovePolicy deletes the policy of a collection (the collection stops being
// replicated; existing data and change rows stay).
func RemovePolicy(app core.App, ref string) error {
	rec, err := findPolicy(app, ref)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("no policy for %q", ref)
	}
	return app.Delete(rec)
}

// LintPolicies checks every policy (enabled or not) and returns the findings.
func LintPolicies(app core.App) ([]policyIssue, error) {
	recs, err := app.FindAllRecords(PoliciesCollection)
	if err != nil {
		return nil, err
	}
	out := []policyIssue{}
	for _, r := range recs {
		out = append(out, checkPolicy(app, r)...)
	}
	return out, nil
}

func policiesCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "policies",
		Short: "Hub: list, set, remove and lint the sync policies (_sync_policies)",
	}

	var asJSON bool
	list := &cobra.Command{
		Use:          "list [--json]",
		Short:        "List the policies",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			rows, err := ListPolicies(app)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(rows)
				fmt.Fprintln(out, string(b))
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "COLLECTION\tDIRECTION\tSTRATEGY\tPARTITION\tVIEWRULE\tENABLED\tORDER\tTYPES")
			for _, r := range rows {
				var types []string
				for f, t := range r.FieldTypes {
					types = append(types, f+"="+t)
				}
				sort.Strings(types)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\t%d\t%s\n", r.Collection, r.Direction, r.Strategy, dash(r.Partition),
					r.PullViewRule, r.Enabled, r.Order, dash(strings.Join(types, ",")))
			}
			return w.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var (
		direction, strategy, partition, hook, crypto string
		order                                        int
		enabled, review, trusted, pullViewRule       bool
		fieldTypes, exclude                          []string
	)
	set := &cobra.Command{
		Use:   "set <collection> [flags]",
		Short: "Create or update the policy of a collection (only the flags you pass change)",
		Long: "Creates the policy (direction both, strategy lww, enabled, pull-view-rule on) or changes the " +
			"settings that are passed. The policy is validated like a save through the API.\n\n" +
			"  toki sync policies set tickets --direction both --partition \"branch = @node.branch\" --field-type fee=counter\n" +
			"  toki sync policies set audit --direction pull",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			f := c.Flags()
			var ch PolicyChange
			if f.Changed("direction") {
				ch.Direction = &direction
			}
			if f.Changed("strategy") {
				ch.Strategy = &strategy
			}
			if f.Changed("partition") {
				ch.Partition = &partition
			}
			if f.Changed("hook") {
				ch.Hook = &hook
			}
			if f.Changed("crypto") {
				ch.Crypto = &crypto
			}
			if f.Changed("order") {
				ch.Order = &order
			}
			if f.Changed("enabled") {
				ch.Enabled = &enabled
			}
			if f.Changed("review") {
				ch.Review = &review
			}
			if f.Changed("trusted") {
				ch.Trusted = &trusted
			}
			if f.Changed("pull-view-rule") {
				ch.PullViewRule = &pullViewRule
			}
			if f.Changed("field-type") {
				ch.FieldTypes = map[string]string{}
				for _, kv := range fieldTypes {
					k, v, ok := strings.Cut(kv, "=")
					if !ok || k == "" || v == "" {
						return fmt.Errorf("--field-type %q: expected <field>=<counter|set|reserve:<sequence>|include>", kv)
					}
					ch.FieldTypes[k] = v
				}
			}
			if f.Changed("exclude") {
				ch.Exclude = append([]string{}, exclude...)
			}
			rec, err := SetPolicy(app, args[0], ch)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "policy %s saved (%s)\n", rec.GetString("collection"), rec.Id)
			return nil
		},
	}
	set.Flags().StringVar(&direction, "direction", "", "both|push|pull|none")
	set.Flags().StringVar(&strategy, "strategy", "", "lww|hub-wins|field-merge|hook")
	set.Flags().StringVar(&partition, "partition", "", "\"<field> = @node.<param>\" (empty removes the partition)")
	set.Flags().StringVar(&hook, "hook", "", "WASM module of strategy hook")
	set.Flags().StringVar(&crypto, "crypto", "", "ciphertext|strip")
	set.Flags().IntVar(&order, "order", 0, "bootstrap/apply order (parents before children)")
	set.Flags().BoolVar(&enabled, "enabled", true, "enable or disable the policy")
	set.Flags().BoolVar(&review, "review", false, "keep automatic merges open for review")
	set.Flags().BoolVar(&trusted, "trusted", false, "pull the collection although its viewRule is null (superusers only)")
	set.Flags().BoolVar(&pullViewRule, "pull-view-rule", true, "also require the viewRule of the collection for the service actor on pull")
	set.Flags().StringArrayVar(&fieldTypes, "field-type", nil, "typed field <field>=<counter|set|reserve:<sequence>|include> (repeatable, replaces the list)")
	set.Flags().StringArrayVar(&exclude, "exclude", nil, "field that is never synced (repeatable, replaces the list)")

	rm := &cobra.Command{
		Use:          "rm <collection>",
		Short:        "Delete the policy of a collection (it stops being replicated)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			if err := RemovePolicy(app, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "policy %s removed\n", args[0])
			return nil
		},
	}

	var lintJSON bool
	lint := &cobra.Command{
		Use:   "lint [--json]",
		Short: "Check the policies: unknown fields, file fields, direction/strategy combinations, partition field",
		Long: "Prints errors and warnings for every policy. Exit status 1 when there is at least one error " +
			"(warnings alone do not fail).",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			issues, err := LintPolicies(app)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			errs := 0
			for _, i := range issues {
				if i.Level == "error" {
					errs++
				}
			}
			if lintJSON {
				b, _ := json.Marshal(issues)
				fmt.Fprintln(out, string(b))
			} else {
				for _, i := range issues {
					fmt.Fprintln(out, i.String())
				}
				fmt.Fprintf(out, "%d issue(s), %d error(s)\n", len(issues), errs)
			}
			if errs > 0 {
				return errors.New("policies have " + strconv.Itoa(errs) + " error(s)")
			}
			return nil
		},
	}
	lint.Flags().BoolVar(&lintJSON, "json", false, "output JSON")

	root.AddCommand(list, set, rm, lint)
	return root
}
