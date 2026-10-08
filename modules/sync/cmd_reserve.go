//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// SequenceRow is a sequence as `toki sync reserve list` prints it.
type SequenceRow struct {
	Name     string `json:"name"`
	Next     int64  `json:"next"`
	Block    int64  `json:"block"`
	MaxOpen  int64  `json:"max_open_per_node"`
	MaxBlock int64  `json:"max_block"`
	Format   string `json:"format"`
}

// RangeRow is a range as `toki sync reserve list` prints it.
type RangeRow struct {
	ID        string `json:"id"`
	Sequence  string `json:"sequence"`
	Node      string `json:"node"`
	NodeName  string `json:"node_name"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
	Status    string `json:"status"`
	HighWater int64  `json:"high_water"`
	Expires   string `json:"expires"`
}

// ListReservations returns the sequences and the ranges of the hub.
func ListReservations(app core.App) ([]SequenceRow, []RangeRow, error) {
	seqs, err := app.FindAllRecords(SequencesCollection)
	if err != nil {
		return nil, nil, errf("%s not found (is TOKI_SYNC_ROLE=hub?)", SequencesCollection)
	}
	var sr []SequenceRow
	for _, r := range seqs {
		sr = append(sr, SequenceRow{
			Name: r.GetString("name"), Next: max(int64(r.GetFloat("next")), 1), Block: recInt(r, "block", defaultBlock),
			MaxOpen: recInt(r, "max_open_per_node", defaultMaxOpen), MaxBlock: recInt(r, "max_block", defaultMaxBlock), Format: r.GetString("format"),
		})
	}
	rngs, err := app.FindAllRecords(ReservationsCollection)
	if err != nil {
		return nil, nil, err
	}
	names := map[string]string{}
	var rr []RangeRow
	for _, r := range rngs {
		node := r.GetString("node")
		if _, ok := names[node]; !ok {
			if n, err := app.FindRecordById(NodesCollection, node); err == nil {
				names[node] = n.GetString("name")
			} else {
				names[node] = ""
			}
		}
		rr = append(rr, RangeRow{
			ID: r.Id, Sequence: r.GetString("sequence"), Node: node, NodeName: names[node], Start: int64(r.GetFloat("start")),
			End: int64(r.GetFloat("end")), Status: r.GetString("status"), HighWater: int64(r.GetFloat("high_water")),
			Expires: r.GetDateTime("expires").String(),
		})
	}
	return sr, rr, nil
}

func reserveCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "reserve",
		Short: "Hub: sequences and the ranges reserved by the nodes (list|create-seq|release)",
	}
	var asJSON bool
	list := &cobra.Command{
		Use:          "list [--json]",
		Short:        "List the sequences and the ranges issued to the nodes",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			seqs, rngs, err := ListReservations(app)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(map[string]any{"sequences": nonNil(seqs), "ranges": nonNil(rngs)})
				fmt.Fprintln(out, string(b))
				return nil
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "SEQUENCE\tNEXT\tBLOCK\tMAX OPEN\tMAX BLOCK\tFORMAT")
			for _, s := range seqs {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\n", s.Name, s.Next, s.Block, s.MaxOpen, s.MaxBlock, dash(s.Format))
			}
			_ = tw.Flush()
			fmt.Fprintln(out)
			tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSEQUENCE\tNODE\tSTART\tEND\tSTATUS\tHIGH WATER\tEXPIRES")
			for _, r := range rngs {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%d\t%s\n", r.ID, r.Sequence, dash(r.NodeName), r.Start, r.End, r.Status, r.HighWater, dash(r.Expires))
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var o SequenceOptions
	create := &cobra.Command{
		Use:          "create-seq <name> [--start N] [--block N] [--max-open N] [--max-block N] [--format F]",
		Short:        "Create a sequence (the numbers are issued to nodes in ranges, never reissued)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			o.Name, o.CLI = args[0], true
			r, err := CreateSequence(app, o)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "sequence %s created (next %d, block %d)\n", r.GetString("name"), int64(r.GetFloat("next")), int64(r.GetFloat("block")))
			return nil
		},
	}
	create.Flags().Int64Var(&o.Start, "start", 1, "first number")
	create.Flags().Int64Var(&o.Block, "block", defaultBlock, "default range size")
	create.Flags().Int64Var(&o.MaxOpen, "max-open", defaultMaxOpen, "active ranges one node may hold")
	create.Flags().Int64Var(&o.MaxBlock, "max-block", defaultMaxBlock, "largest range a node may ask for")
	create.Flags().StringVar(&o.Format, "format", "", "display format, for example G{node.code}-{n:06} (documentation only)")

	release := &cobra.Command{
		Use:          "release <range id>",
		Short:        "Retire a range (its numbers are never reissued; the node stops using it)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			if err := ReleaseRange(app, args[0], "", 0, true); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "range %s retired\n", args[0])
			return nil
		},
	}
	root.AddCommand(list, create, release)
	return root
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
