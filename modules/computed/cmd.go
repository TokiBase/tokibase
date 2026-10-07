package computed

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// NewCommand returns the `computed` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "computed",
		Short: "Manage server-maintained counters and rollups (_computed_fields)",
		Long: "A computed field is an existing number field on a parent collection that the server keeps\n" +
			"equal to an aggregate (count, sum, avg, min, max, last) of a child collection.\n" +
			"Clients cannot write it. Values are exact right after a backfill and eventually\n" +
			"consistent right after each committed child write.",
	}

	var asJSON bool
	list := &cobra.Command{
		Use: "list [collection]", Short: "List definitions", Args: cobra.MaximumNArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			coll := ""
			if len(args) == 1 {
				coll = args[0]
			}
			defs, err := List(app, coll)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(c, defs)
			}
			for _, d := range defs {
				fmt.Fprintf(c.OutOrStdout(), "%s.%s\t%s\tfrom %s.%s", d.Collection, d.Field, d.Kind, d.SourceCollection, d.SourceRelation)
				if d.SourceField != "" {
					fmt.Fprintf(c.OutOrStdout(), "\tof=%s", d.SourceField)
				}
				if d.Filter != "" {
					fmt.Fprintf(c.OutOrStdout(), "\tfilter=%s", d.Filter)
				}
				fmt.Fprintln(c.OutOrStdout())
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var d Def
	add := &cobra.Command{
		Use:   "add <collection> <field>",
		Short: "Create or update a definition (validated against the schema)",
		Example: "computed add fgr_users total_distance_km --kind sum --source fgr_workout_sessions \\\n" +
			"    --relation user --source-field distance_km --filter 'status = \"done\"'\n" +
			"computed add clans members_count --kind count --source clan_members --relation clan",
		Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			d.Collection, d.Field = args[0], args[1]
			out, err := Add(app, d)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "ok %s.%s (%s). Run `computed backfill %s %s` to compute existing values.\n",
				out.Collection, out.Field, out.Kind, out.Collection, out.Field)
			if w := MissingIndex(app, *out); w != "" {
				fmt.Fprintln(c.OutOrStdout(), "WARNING: "+w)
			}
			return nil
		},
	}
	add.Flags().StringVar(&d.Kind, "kind", "", "count|sum|avg|min|max|last")
	add.Flags().StringVar(&d.SourceCollection, "source", "", "child collection")
	add.Flags().StringVar(&d.SourceRelation, "relation", "", "relation field on the child that points to the parent")
	add.Flags().StringVar(&d.SourceField, "source-field", "", "child number field (not for count)")
	add.Flags().StringVar(&d.Filter, "filter", "", "optional filter on the child (rule language)")
	_ = add.MarkFlagRequired("kind")
	_ = add.MarkFlagRequired("source")
	_ = add.MarkFlagRequired("relation")

	rm := &cobra.Command{
		Use: "rm <collection> <field>", Short: "Delete a definition (stored values stay as they are)",
		Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			ok, err := Remove(app, args[0], args[1])
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("no such definition")
			}
			fmt.Fprintln(c.OutOrStdout(), "removed")
			return nil
		},
	}

	var inline bool
	backfill := &cobra.Command{
		Use:   "backfill <collection> [field]",
		Short: "Recompute all parents and fix drift (as a job when a queue exists, else inline)",
		Args:  cobra.RangeArgs(1, 2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			field := ""
			if len(args) == 2 {
				field = args[1]
			}
			if !inline {
				id, err := EnqueueBackfill(app, args[0], field)
				if err == nil {
					fmt.Fprintf(c.OutOrStdout(), "queued job %s (see `jobs list`; a worker must be running)\n", id)
					return nil
				}
				if !errors.Is(err, kernel.ErrNoJobQueue) {
					return err
				}
			}
			m := New(app)
			reps, err := m.Backfill(args[0], field, func(done int, r *Report) {
				fmt.Fprintf(c.ErrOrStderr(), "\r%s.%s: %d parents, %d fixed", r.Collection, r.Field, done, r.Fixed)
			})
			fmt.Fprintln(c.ErrOrStderr())
			for _, r := range reps {
				fmt.Fprintf(c.OutOrStdout(), "%s.%s\tparents=%d drift=%d fixed=%d\n", r.Collection, r.Field, r.Parents, r.Drift, r.Fixed)
			}
			return err
		},
	}
	backfill.Flags().BoolVar(&inline, "inline", false, "run in this process instead of queueing a job")

	verify := &cobra.Command{
		Use: "verify <collection> [field]", Short: "Report drift without writing (exit 1 when drift is found)",
		Args: cobra.RangeArgs(1, 2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			field := ""
			if len(args) == 2 {
				field = args[1]
			}
			reps, err := New(app).Verify(args[0], field)
			drift := 0
			for _, r := range reps {
				drift += r.Drift
				fmt.Fprintf(c.OutOrStdout(), "%s.%s\tparents=%d drift=%d", r.Collection, r.Field, r.Parents, r.Drift)
				if len(r.Sample) > 0 {
					fmt.Fprintf(c.OutOrStdout(), " sample=%v", r.Sample)
				}
				fmt.Fprintln(c.OutOrStdout())
			}
			if err != nil {
				return err
			}
			if drift > 0 {
				return fmt.Errorf("drift found in %d parent(s)", drift)
			}
			return nil
		},
	}

	drift := &cobra.Command{
		Use: "drift", Short: "Verify every definition now, log and audit drift (what the daily cron does)",
		Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			n, err := New(app).DriftAll()
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "drift=%d\n", n)
			if n > 0 {
				return fmt.Errorf("drift found in %d parent(s)", n)
			}
			return nil
		},
	}

	var with []string
	index := &cobra.Command{
		Use:     "index <collection> <field>",
		Short:   "Create an index on the child's relation column (what every recompute filters on)",
		Example: "computed index kids parent\ncomputed index comments post --with created   # for kind last",
		Args:    cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			name, created, err := EnsureIndex(app, args[0], args[1], with...)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(c.OutOrStdout(), "created index %s\n", name)
			} else {
				fmt.Fprintf(c.OutOrStdout(), "index %s already covers it\n", name)
			}
			return nil
		},
	}
	index.Flags().StringSliceVar(&with, "with", nil, "extra columns after the relation column (e.g. created for kind last)")

	root.AddCommand(list, add, rm, backfill, verify, drift, index)
	return root
}

func printJSON(c *cobra.Command, v any) error {
	enc := json.NewEncoder(c.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// New returns a module bound to app without registering hooks (CLI use).
func New(app core.App) *Module {
	return &Module{app: app, invalid: true, locks: map[string]*entry{}}
}
