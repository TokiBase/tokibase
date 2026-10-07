//go:build !no_replica

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
	"github.com/tokibase/tokibase/modules/walreplica"
)

// NewReplicaCommand creates the `replica` command (status, restore, snapshot)
// backed by modules/walreplica.
func NewReplicaCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "replica",
		Short: "Inspect, restore and snapshot the WAL replica (" + walreplica.EnvURL + ")",
	}
	command.AddCommand(replicaStatusCommand())
	command.AddCommand(replicaRestoreCommand())
	command.AddCommand(replicaSnapshotCommand(app))
	command.AddCommand(replicaPromoteCommand())
	return command
}

func replicaURL(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	cfg, err := walreplica.FromEnv()
	if err != nil {
		return "", err
	}
	if !cfg.Enabled() {
		return "", fmt.Errorf("no replica url: pass --url or set %s", walreplica.EnvURL)
	}
	return cfg.URL, nil
}

func replicaStatusCommand() *cobra.Command {
	var urlFlag string
	var asJSON bool

	command := &cobra.Command{
		Use:   "status",
		Short: "Show what the replica contains (reads the replica, the app does not need to run)",
		Long: "Reads the replica at --url (default: $" + walreplica.EnvURL + ") and prints, per database, the newest replicated\n" +
			"transaction, its age and the snapshots available for restore.\n" +
			"Live lag and errors of a running server are in the superuser /api/health response (data.replica).",
		Example:      "replica status --json",
		SilenceUsage: true,
		Annotations:  map[string]string{AnnotationSkipBootstrap: "true"},
		RunE: func(command *cobra.Command, args []string) error {
			url, err := replicaURL(urlFlag)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(command.Context(), 2*time.Minute)
			defer cancel()

			infos, err := walreplica.Inspect(ctx, url)
			if err != nil {
				return err
			}

			if asJSON {
				raw, err := json.MarshalIndent(infos, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(raw))
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "DB\tLATEST TXID\tLATEST AGE\tSNAPSHOTS\tOLDEST RESTORE POINT\tFILES\tBYTES")
			for _, i := range infos {
				age, oldest := "-", "-"
				if i.LatestAt != nil {
					age = time.Since(*i.LatestAt).Round(time.Second).String()
				}
				if i.OldestRestoreAt != nil {
					oldest = i.OldestRestoreAt.Format(time.RFC3339)
				}
				fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%d\t%d\n", i.Name, i.LatestTXID, age, i.Snapshots, oldest, i.Files, i.Bytes)
			}
			return w.Flush()
		},
	}

	command.Flags().StringVar(&urlFlag, "url", "", "replica url (default $"+walreplica.EnvURL+")")
	command.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return command
}

func replicaRestoreCommand() *cobra.Command {
	var urlFlag, timestamp string
	var overwrite bool

	command := &cobra.Command{
		Use:   "restore --dir <pb_data>",
		Short: "Restore data.db and auxiliary.db from the replica into a pb_data directory",
		Long: "Restores both databases from the replica into the directory given with --dir (required, the global flag).\n" +
			"Refuses to run when <dir>/data.db exists unless --overwrite is set. Stop the server first.\n" +
			"Use --timestamp for a point-in-time restore.",
		Example:      "replica restore --url s3://bucket/prefix --dir ./pb_data --timestamp 2026-10-07T10:00:00Z",
		SilenceUsage: true,
		Annotations:  map[string]string{AnnotationSkipBootstrap: "true"},
		RunE: func(command *cobra.Command, args []string) error {
			dirFlag := command.Flags().Lookup("dir")
			if dirFlag == nil || !dirFlag.Changed || dirFlag.Value.String() == "" {
				return errors.New("--dir <pb_data> is required")
			}

			url, err := replicaURL(urlFlag)
			if err != nil {
				return err
			}

			opts := walreplica.RestoreOptions{Overwrite: overwrite}
			if timestamp != "" {
				if opts.Timestamp, err = time.Parse(time.RFC3339, timestamp); err != nil {
					return fmt.Errorf("invalid --timestamp (want RFC3339, e.g. 2026-10-07T10:00:00Z): %w", err)
				}
			}

			if err := walreplica.Restore(command.Context(), url, dirFlag.Value.String(), opts); err != nil {
				return err
			}
			fmt.Printf("restored into %s\n", dirFlag.Value.String())
			return nil
		},
	}

	command.Flags().StringVar(&urlFlag, "url", "", "replica url (default $"+walreplica.EnvURL+")")
	command.Flags().StringVar(&timestamp, "timestamp", "", "restore the state as of this RFC3339 time (default latest)")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "replace an existing data.db/auxiliary.db in --dir")
	return command
}

func replicaSnapshotCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:   "snapshot",
		Short: "Force a full snapshot of both databases now",
		Long: "Starts replication in this process, syncs pending changes and writes a snapshot of data.db and auxiliary.db\n" +
			"to $" + walreplica.EnvURL + ". Run it only while no server is replicating the same pb_data\n" +
			"(a running server snapshots by itself every $TOKI_REPLICA_SNAPSHOT_INTERVAL).",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(command.Context(), 5*time.Minute)
			defer cancel()

			res, err := walreplica.Snapshot(ctx, app)
			if err != nil {
				return err
			}
			for _, name := range []string{"data", "aux"} {
				if txid, ok := res[name]; ok {
					fmt.Printf("%s: snapshot at txid %d\n", name, txid)
				}
			}
			return nil
		},
	}
}

func replicaPromoteCommand() *cobra.Command {
	var urlFlag, timestamp string
	var force bool

	command := &cobra.Command{
		Use:   "promote --dir <pb_data>",
		Short: "Promote the replica into a standalone pb_data directory (failover)",
		Long: "Restores data.db and auxiliary.db from the replica into --dir (required), runs PRAGMA integrity_check on both,\n" +
			"counts the collections and writes <dir>/" + walreplica.PromotedMarker + ".\n" +
			"Refuses when <dir>/data.db exists unless --force, which MOVES the directory to <dir>.pre-promote-<unixts> (never deletes).\n" +
			"Stop the old primary first. Then start the promoted node with a NEW " + walreplica.EnvURL + ": it must not replicate back into\n" +
			"the url it was restored from.",
		Example:      "replica promote --url s3://bucket/prod --dir ./pb_data --timestamp 2026-10-07T10:00:00Z",
		SilenceUsage: true,
		Annotations:  map[string]string{AnnotationSkipBootstrap: "true"},
		RunE: func(command *cobra.Command, args []string) error {
			dirFlag := command.Flags().Lookup("dir")
			if dirFlag == nil || !dirFlag.Changed || dirFlag.Value.String() == "" {
				return errors.New("--dir <pb_data> is required")
			}
			dir := dirFlag.Value.String()

			url, err := replicaURL(urlFlag)
			if err != nil {
				return err
			}

			opts := walreplica.PromoteOptions{Force: force}
			if timestamp != "" {
				if opts.Timestamp, err = time.Parse(time.RFC3339, timestamp); err != nil {
					return fmt.Errorf("invalid --timestamp (want RFC3339, e.g. 2026-10-07T10:00:00Z): %w", err)
				}
			}

			res, err := walreplica.Promote(command.Context(), url, dir, opts)
			if err != nil {
				return err
			}

			if res.MovedTo != "" {
				fmt.Printf("moved the previous directory to %s\n", res.MovedTo)
			}
			fmt.Printf("promoted %s into %s: integrity ok, %d collections\n", res.FromURL, dir, res.Collections)
			fmt.Printf("\nNext step: start the new primary on a NEW replica url:\n")
			fmt.Printf("  %s=<new url, not %s> toki serve --dir %s\n", walreplica.EnvURL, res.FromURL, dir)
			fmt.Printf("Do not reuse the old url: the old primary may still be writing to it, and two writers corrupt the replica history.\n")
			return nil
		},
	}

	command.Flags().StringVar(&urlFlag, "url", "", "replica url (default $"+walreplica.EnvURL+")")
	command.Flags().StringVar(&timestamp, "timestamp", "", "promote the state as of this RFC3339 time (default latest)")
	command.Flags().BoolVar(&force, "force", false, "move an existing --dir aside (<dir>.pre-promote-<unixts>) instead of refusing")
	return command
}
