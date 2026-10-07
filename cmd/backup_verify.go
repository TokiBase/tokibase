//go:build !no_backupcheck

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/backupcheck"
)

func addBackupVerifyCommands(command *cobra.Command, app core.App) {
	command.AddCommand(backupListCommand(app))
	command.AddCommand(backupVerifyCommand(app))
	command.AddCommand(backupVerifyAllCommand(app))
}

func backupListCommand(app core.App) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:          "list",
		Short:        "List stored backups, newest first",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			list, err := backupcheck.List(context.Background(), app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(list)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSIZE\tMODIFIED")
			for _, b := range list {
				fmt.Fprintf(tw, "%s\t%d\t%s\n", b.Name, b.Size, b.Modified.UTC().Format(time.RFC3339))
			}
			return tw.Flush()
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the list as JSON")
	return command
}

func backupVerifyCommand(app core.App) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "verify <name|latest>",
		Short: "Restore a backup into a temp dir and check its integrity",
		Long: "Restore a backup into a temp dir and run PRAGMA integrity_check/quick_check, compare\n" +
			"collections and records with the live app and check sampled storage files.\n" +
			"Exit code 1 when integrity or quick check fail, or the collections count differs (latest backup).",
		Example:      "backup verify latest --json",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			ctx := context.Background()
			name := args[0]
			if name == "latest" {
				var err error
				if name, err = backupcheck.Latest(ctx, app); err != nil {
					return err
				}
			}
			r, _ := backupcheck.Verify(ctx, app, name)
			if asJSON {
				if err := printJSON(r); err != nil {
					return err
				}
			} else {
				printReports([]backupcheck.Report{r})
				if r.Error != "" {
					fmt.Println("error:", r.Error)
				}
			}
			if !r.OK() {
				return errors.New("backupcheck: verification failed")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return command
}

func backupVerifyAllCommand(app core.App) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:          "verify-all",
		Short:        "Verify every stored backup",
		Long:         "Verify every stored backup. Exit code 1 when any of them fails.",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			ctx := context.Background()
			list, err := backupcheck.List(ctx, app)
			if err != nil {
				return err
			}
			reports := make([]backupcheck.Report, 0, len(list))
			failed := 0
			for _, b := range list {
				r, _ := backupcheck.Verify(ctx, app, b.Name)
				if !r.OK() {
					failed++
				}
				reports = append(reports, r)
			}
			if asJSON {
				if err := printJSON(reports); err != nil {
					return err
				}
			} else {
				printReports(reports)
				fmt.Printf("%d backup(s), %d failed\n", len(reports), failed)
			}
			if failed > 0 {
				return errors.New("backupcheck: verification failed")
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the reports as a JSON array")
	return command
}

func printJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

func printReports(reports []backupcheck.Report) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tNAME\tINTEGRITY\tQUICK\tCOLLECTIONS\tRECORDS\tMISSING\tDURATION")
	for _, r := range reports {
		status := "ok"
		if !r.OK() {
			status = "FAIL"
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%t\t%d/%d\t%d/%d\t%d\t%s\n",
			status, r.Name, r.IntegrityOK, r.QuickCheckOK,
			r.Collections, r.LiveCollections, r.Records, r.LiveRecords,
			r.MissingFiles, r.Duration.Round(time.Millisecond))
	}
	tw.Flush()
}
