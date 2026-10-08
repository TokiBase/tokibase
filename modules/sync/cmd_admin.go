//go:build !no_sync

package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// readyModule returns the registered module, initialized.
func readyModule(app core.App) (*Module, error) {
	m := moduleOf(app)
	if m == nil {
		return nil, errors.New("sync is off (TOKI_SYNC_ROLE=hub|spoke)")
	}
	if !m.ready.Load() {
		if err := m.Init(); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func purgeCommand(app core.App) *cobra.Command {
	var legal bool
	var reason string
	c := &cobra.Command{
		Use:   "purge <collection> <id> --legal --reason \"...\"",
		Short: "Hub: erase a record for good (legal tombstone, change patches blanked, spokes erase it too)",
		Long: "Deletes the record, writes a LEGAL tombstone (immutable and never pruned: the id can never be created again " +
			"on any node), erases the patch of every change row of the record and sends the erasure to all nodes.\n" +
			"This cannot be undone.",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			if !legal {
				return errors.New("--legal is required: a purge is irreversible")
			}
			m, err := readyModule(app)
			if err != nil {
				return err
			}
			res, err := m.Purge(args[0], args[1], reason, "cli", true)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(res)
			fmt.Fprintln(c.OutOrStdout(), string(b))
			return nil
		},
	}
	c.Flags().BoolVar(&legal, "legal", false, "required: write a legal tombstone")
	c.Flags().StringVar(&reason, "reason", "", "why the record is erased (required)")
	return c
}

func compactCommand(app core.App) *cobra.Command {
	var vacuum, asJSON bool
	c := &cobra.Command{
		Use:          "compact [--vacuum] [--json]",
		Short:        "Run the compaction now (stale nodes, old changes, tombstones, resolved conflicts, acked spoke rows)",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := readyModule(app)
			if err != nil {
				return err
			}
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			rep, err := m.Compact(ctx)
			if err != nil {
				return err
			}
			if vacuum {
				if err := m.Vacuum(); err != nil {
					return err
				}
				rep.Vacuumed = true
			}
			out := c.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(rep)
				fmt.Fprintln(out, string(b))
				return nil
			}
			fmt.Fprintf(out, "role:        %s\nstale nodes: %d\nsafe seq:    %d\nchanges deleted:    %d\nlow water:   %d\ntombstones deleted: %d\nconflicts deleted:  %d\nspoke rows deleted: %d\nvacuumed:    %t\n",
				rep.Role, rep.StaleNodes, rep.Safe, rep.ChangesDeleted, rep.LowWater, rep.TombstonesDeleted, rep.ConflictsDeleted, rep.SpokeRowsDeleted, rep.Vacuumed)
			return nil
		},
	}
	c.Flags().BoolVar(&vacuum, "vacuum", false, "VACUUM data.db afterwards (needs free disk space and blocks writers)")
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return c
}
