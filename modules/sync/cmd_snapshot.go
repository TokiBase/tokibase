//go:build !no_sync

package sync

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
)

// rebootstrapCommand is `toki sync rebootstrap [<node>]` (docs/SYNC_DESIGN.md §8.4).
// On the hub it marks a node: its next handshake answers rebootstrap=true. On a
// spoke (no argument) it schedules a snapshot bootstrap that the running loop
// starts at its next cycle; --now runs it in this process instead.
func rebootstrapCommand(app core.App) *cobra.Command {
	var now bool
	c := &cobra.Command{
		Use:   "rebootstrap [<node>] [--now]",
		Short: "Hub: mark a node for re-bootstrap. Spoke: replace the local data with a snapshot of the hub",
		Long: "Hub: `toki sync rebootstrap <node id|name>` makes the next handshake of the node answer rebootstrap=true; " +
			"the node then fetches a snapshot and becomes active again.\n" +
			"Spoke: `toki sync rebootstrap` sets the state to rebootstrap_required; the running node starts the snapshot at its next cycle " +
			"(--now runs it here, for a node that is not serving). Unpushed local changes are kept and rebased.",
		SilenceUsage: true,
		Args:         cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			switch RoleFromEnv() {
			case RoleHub:
				if len(args) != 1 {
					return fmt.Errorf("on a hub the command needs a node: toki sync rebootstrap <node id|name>")
				}
				rec, err := MarkRebootstrap(app, args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "marked %s (%s) for re-bootstrap\n", rec.GetString("name"), rec.Id)
				return nil
			case RoleSpoke:
				if len(args) != 0 {
					return fmt.Errorf("on a spoke the command takes no node")
				}
				if err := ensureSchema(app); err != nil {
					return err
				}
				if err := client.ScheduleRebootstrap(app, "re-bootstrap requested with toki sync rebootstrap"); err != nil {
					return err
				}
				if !now {
					fmt.Fprintln(out, "scheduled: the running node starts the snapshot at its next cycle (state rebootstrap_required)")
					return nil
				}
				m, err := readyModule(app)
				if err != nil {
					return err
				}
				cl, err := m.NewClient(func(o *client.Options) { o.Backend = backend{m} })
				if err != nil {
					return err
				}
				ctx := c.Context()
				if ctx == nil {
					ctx = context.Background()
				}
				if err := cl.Bootstrap(ctx); err != nil {
					return err
				}
				fmt.Fprintln(out, "bootstrap finished")
				return nil
			}
			return requireRole(RoleSpoke)
		},
	}
	c.Flags().BoolVar(&now, "now", false, "spoke: run the bootstrap in this process")
	return c
}
