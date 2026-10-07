package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// ParseAge accepts "7d", "36h", "90m" and returns the duration.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil && n >= 0 {
			return time.Duration(n * 24 * float64(time.Hour)), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, nil
	}
	return 0, fmt.Errorf("invalid age %q (use e.g. 7d, 36h, 90m)", s)
}

// NewCommand returns the `jobs` cobra command (list, retry, purge, stats).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "jobs", Short: "Inspect and manage the durable job queue"}

	var state string
	var limit int
	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List jobs, newest first", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			rows, err := List(app, state, limit)
			if err != nil {
				return err
			}
			if asJSON {
				if rows == nil {
					rows = []Row{}
				}
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(rows)
			}
			for _, r := range rows {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\tattempt=%d/%d\trun_at=%s\t%s\n",
					r.ID, r.State, r.Kind, r.Attempt, r.MaxAttempts, r.RunAt, r.LastError)
			}
			return nil
		},
	}
	list.Flags().StringVar(&state, "state", "", "filter by state (queued|running|done|failed|dead)")
	list.Flags().IntVar(&limit, "limit", 100, "maximum rows (0 = all)")
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var dead bool
	retry := &cobra.Command{
		Use: "retry <id|--dead>", Short: "Requeue a job, or every dead job with --dead", SilenceUsage: true,
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if dead == (len(args) == 1) {
				return errors.New("pass exactly one of <id> or --dead")
			}
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			n, err := Retry(app, id, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "requeued %d job(s)\n", n)
			return nil
		},
	}
	retry.Flags().BoolVar(&dead, "dead", false, "requeue all dead jobs")

	var doneBefore string
	purge := &cobra.Command{
		Use: "purge", Short: "Delete finished jobs", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if doneBefore == "" {
				return errors.New("--done-before is required (e.g. 7d)")
			}
			age, err := ParseAge(doneBefore)
			if err != nil {
				return err
			}
			n, err := PurgeDone(app, time.Now().Add(-age))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "purged %d done job(s)\n", n)
			return nil
		},
	}
	purge.Flags().StringVar(&doneBefore, "done-before", "", "delete done jobs older than this age (7d, 36h, 90m)")

	var statsJSON bool
	stats := &cobra.Command{
		Use: "stats", Short: "Show job counts by state", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			m := New(app)
			s, err := m.Stats(context.Background())
			if err != nil {
				return err
			}
			if statsJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(s)
			}
			fmt.Fprintf(c.OutOrStdout(), "queued=%d running=%d failed=%d done=%d dead=%d\n", s.Queued, s.Running, s.Failed, s.Done, s.Dead)
			return nil
		},
	}
	stats.Flags().BoolVar(&statsJSON, "json", false, "output JSON")

	root.AddCommand(list, retry, purge, stats)
	return root
}
