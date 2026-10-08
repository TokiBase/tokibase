//go:build !no_sync

package sync

import (
	"encoding/json"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/hlc"
	"github.com/tokibase/tokibase/modules/sync/proto"
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

	// Hub fields.
	HubID string `json:"hub_id,omitempty"`
	Epoch string `json:"epoch,omitempty"`
	Nodes int64  `json:"nodes,omitempty"`

	// Spoke fields (from `_sync_cursors`).
	HubURL        string `json:"hub_url,omitempty"`
	CertExpires   string `json:"cert_expires,omitempty"`
	ClockOffsetMs int64  `json:"clock_offset_ms"`
	LastHandshake string `json:"last_handshake,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// State is the spoke loop state kept in `_sync_cursors` ("rebootstrap_required"
	// when the hub compacted the changes this node is missing).
	State string `json:"state,omitempty"`
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
	if id, ok, err := st.Get(keyHubID); err == nil && ok && role == RoleHub {
		s.HubID = id
		s.Epoch, _, _ = st.Get(keyEpoch)
	}
	if role == RoleHub && app.HasTable(NodesCollection) {
		if n, err := app.CountRecords(NodesCollection); err == nil {
			s.Nodes = n
		}
	}
	if app.HasTable("_sync_cursors") {
		cur, err := client.LoadCursor(app)
		if err != nil {
			return nil, err
		}
		if cur != nil {
			s.HubID, s.HubURL, s.ClockOffsetMs, s.LastError = cur.HubID, cur.HubURL, cur.ClockOffsetMs, cur.LastError
			s.State = cur.State
			if cur.LastOK.Valid {
				s.LastHandshake = cur.LastOK.String
			}
			if c, _, err := jwt.NewParser().ParseUnverified(cur.Cert, &proto.CertClaims{}); err == nil {
				if cl, ok := c.Claims.(*proto.CertClaims); ok && cl.ExpiresAt != nil {
					s.CertExpires = cl.ExpiresAt.UTC().Format(proto.TimeLayout)
				}
			}
			if s.Epoch == "" {
				s.Epoch = cur.HubEpoch
			}
		}
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
			fmt.Fprintf(out, "hub id:   %s\nepoch:    %s\n", dash(s.HubID), dash(s.Epoch))
			if s.Role == string(RoleHub) {
				fmt.Fprintf(out, "nodes:    %d\n", s.Nodes)
			} else {
				fmt.Fprintf(out, "hub url:  %s\ncert expires: %s\nclock offset: %d ms\nlast handshake: %s\nlast error: %s\nstate: %s\n",
					dash(s.HubURL), dash(s.CertExpires), s.ClockOffsetMs, dash(s.LastHandshake), dash(s.LastError), dash(s.State))
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	root.AddCommand(status)
	root.AddCommand(enrollCommand(app), joinCommand(app), revokeCommand(app), peersCommand(app), verifyCommand(app), conflictsCommand(app))
	root.AddCommand(policiesCommand(app), purgeCommand(app), compactCommand(app))
	return root
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
