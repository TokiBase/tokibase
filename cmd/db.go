package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/tools/dbadvise"
)

// NewDBCommand creates the `db` command (advise).
func NewDBCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "db",
		Short: "Database diagnostics",
	}
	command.AddCommand(dbAdviseCommand(app))
	return command
}

func dbAdviseCommand(app core.App) *cobra.Command {
	var (
		asJSON  bool
		minRecs int64
		slowMs  float64
		logs    int
		noLogs  bool
	)

	command := &cobra.Command{
		Use:   "advise",
		Short: "List collections whose common sort/filter fields have no index",
		Long: "List the fields that lists are sorted or filtered by without an index. A sort on an unindexed field scans the whole\n" +
			"collection and sorts it in a temp b-tree on every request (for example `sort=-created`), which is what limits the read\n" +
			"throughput of large collections. Sources: the schema (autodate created/updated and single relation fields) and the slow\n" +
			"`GET /api/collections/<name>/records` request logs. Nothing is changed: add the suggested indexes to the collection.",
		Example:      "db advise --min-records 5000 --json",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if noLogs {
				logs = -1
			}

			findings, err := dbadvise.Run(app, dbadvise.Options{MinRecords: minRecs, SlowMs: slowMs, LogLimit: logs})
			if err != nil {
				return err
			}

			if asJSON {
				raw, err := json.MarshalIndent(findings, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(raw))
				return nil
			}

			for _, f := range findings {
				detail := fmt.Sprintf("%d records", f.Records)
				if f.Source == "logs" {
					detail += fmt.Sprintf(", %d slow requests, avg %.0f ms, max %.0f ms", f.Requests, f.AvgMs, f.MaxMs)
				}
				fmt.Printf("%s %s.%s (%s, %s)\n    %s\n", color.YellowString("no index"), f.Collection, f.Field, f.Kind, detail, f.Suggestion)
			}
			fmt.Printf("%d finding(s)\n", len(findings))

			return nil
		},
	}

	command.Flags().BoolVar(&asJSON, "json", false, "print the findings as a JSON array")
	command.Flags().Int64Var(&minRecs, "min-records", 1000, "ignore the collections with fewer records")
	command.Flags().Float64Var(&slowMs, "slow-ms", 50, "consider the logged list requests slower than this (ms)")
	command.Flags().IntVar(&logs, "log-rows", 50000, "number of the most recent list request logs to scan")
	command.Flags().BoolVar(&noLogs, "no-logs", false, "use only the schema heuristics")

	return command
}
