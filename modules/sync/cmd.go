//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/hlc"
)

// Status is the output of `toki sync status`.
type Status struct {
	Role    string `json:"role"`
	NodeID  string `json:"node_id"`
	Pending int64  `json:"pending"`
	// LastHLC is the highest hlc in _changes ("" when empty).
	LastHLC string `json:"last_hlc"`
	// Floor is the persisted clock floor ("" when none).
	Floor string `json:"hlc_floor"`
}

// GetStatus reads the status from the database. role is the configured role
// (the module may be nil when the role is off).
func GetStatus(app core.App, role Role) (*Status, error) {
	s := &Status{Role: string(role)}
	if !app.HasTable("_changes") || !app.HasTable("_sync_state") {
		return s, nil
	}
	st := dbState{db: app.NonconcurrentDB()}
	if id, ok, err := st.Get(keyNodeID); err != nil {
		return nil, err
	} else if ok {
		s.NodeID = id
	}
	if err := app.DB().NewQuery("SELECT COUNT(*) FROM _changes WHERE status='local'").Row(&s.Pending); err != nil {
		return nil, err
	}
	var maxHLC int64
	if err := app.DB().NewQuery("SELECT COALESCE(MAX(hlc),0) FROM _changes").Row(&maxHLC); err != nil {
		return nil, err
	}
	if maxHLC > 0 {
		s.LastHLC = hlc.HLC(maxHLC).String()
	}
	if f, err := hlc.LoadFloor(st); err != nil {
		return nil, err
	} else if f > 0 {
		s.Floor = f.String()
	}
	return s, nil
}

// NewCommand returns the `sync` cobra command (status).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "sync",
		Short: "Inspect hub/spoke sync (TOKI_SYNC_ROLE=off|hub|spoke)",
	}
	var asJSON bool
	status := &cobra.Command{
		Use:          "status",
		Short:        "Show role, node id, pending changes, last hlc and clock floor",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			s, err := GetStatus(app, RoleFromEnv())
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(s)
				fmt.Fprintln(out, string(b))
				return nil
			}
			fmt.Fprintf(out, "role:     %s\nnode id:  %s\npending:  %d\nlast hlc: %s\nhlc floor: %s\n",
				s.Role, dash(s.NodeID), s.Pending, dash(s.LastHLC), dash(s.Floor))
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	root.AddCommand(status)
	return root
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
