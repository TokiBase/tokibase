//go:build !no_sync

package sync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/sync/client"
	"github.com/tokibase/tokibase/modules/sync/proto"
)

func requireRole(want Role) error {
	if got := RoleFromEnv(); got != want {
		return fmt.Errorf("this command needs %s=%s (current: %s)", EnvRole, want, got)
	}
	return nil
}

func enrollCommand(app core.App) *cobra.Command {
	var name, profile, actor string
	var allowSU bool
	var params []string
	c := &cobra.Command{
		Use:   "enroll --name N --profile P [--param k=v] [--actor col/id [--allow-superuser-actor]]",
		Short: "Hub: create a pending node and print its one-time enrollment code",
		Long: "Creates a pending node on the hub and prints a one-time code (valid 24 h). " +
			"The code is shown only once; the hub keeps its hash. Use it on the device: toki sync join <hub-url> <code>.",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			pm := map[string]string{}
			for _, kv := range params {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return fmt.Errorf("--param %q: expected k=v", kv)
				}
				pm[k] = v
			}
			rec, code, err := CreateEnrollment(app, EnrollOptions{Name: name, Profile: profile, Params: pm, Actor: actor, AllowSuperuserActor: allowSU, CLI: true})
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			fmt.Fprintf(out, "node:    %s (%s)\nprofile: %s\nexpires: %s\ncode:    %s\n\nThis code is shown once and valid for %s.\nOn the device: toki sync join <hub-url> %s\n",
				rec.GetString("name"), rec.Id, rec.GetString("profile"), rec.GetDateTime("enroll_expires").Time().UTC().Format(time.RFC3339), code, EnrollTTL, code)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "node name (required)")
	c.Flags().StringVar(&profile, "profile", "", "profile: "+strings.Join(nodeProfiles, "|")+" (required)")
	c.Flags().StringArrayVar(&params, "param", nil, "partition param k=v (repeatable)")
	c.Flags().StringVar(&actor, "actor", "", "service actor <collection>/<record id>")
	c.Flags().BoolVar(&allowSU, "allow-superuser-actor", false, "allow a superuser as service actor (bypasses all rules for what the node pushes as itself)")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("profile")
	return c
}

func joinCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:          "join <hub-url> [<code>|-]",
		Short:        "Spoke: enroll this node on a hub with a one-time code (read from stdin with '-', or from TOKI_SYNC_ENROLL_CODE)",
		SilenceUsage: true,
		Args:         cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleSpoke); err != nil {
				return err
			}
			if err := ensureSchema(app); err != nil {
				return err
			}
			code, err := joinCode(c, args)
			if err != nil {
				return err
			}
			id, err := proto.LoadOrCreateIdentity(filepath.Join(app.DataDir(), NodeKeyFile), os.Getenv(EnvNodeKey))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(c.Context(), 2*client.RequestTimeout)
			defer cancel()
			res, err := client.Join(ctx, app, client.EnrollParams{
				HubURL: args[0], Code: code, Identity: id, Profile: profileFromEnv(), AppVersion: appVersion(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "enrolled\nnode id: %s\nhub id:  %s\nhub url: %s\n", res.NodeID, res.HubID, strings.TrimRight(args[0], "/"))
			return nil
		},
	}
}

// EnvEnrollCode lets `toki sync join` take the code from the environment, so
// it does not show up in the process list or the shell history.
const EnvEnrollCode = "TOKI_SYNC_ENROLL_CODE"

// joinCode returns the one-time code: argument, "-" (first line of stdin) or
// the environment.
func joinCode(c *cobra.Command, args []string) (string, error) {
	code := ""
	switch {
	case len(args) == 2 && args[1] == "-":
		line, err := bufio.NewReader(io.LimitReader(c.InOrStdin(), 1024)).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("sync: no code on stdin")
		}
		code = line
	case len(args) == 2:
		code = args[1]
	default:
		code = os.Getenv(EnvEnrollCode)
	}
	if code = strings.TrimSpace(code); code == "" {
		return "", errors.New("sync: the enrollment code is missing (argument, '-' for stdin or " + EnvEnrollCode + ")")
	}
	return code, nil
}

func profileFromEnv() string {
	if p := strings.TrimSpace(os.Getenv("TOKI_PROFILE")); validProfile(p) {
		return p
	}
	return "edge"
}

func appVersion() string { return strings.TrimSpace(os.Getenv("TOKI_APP_VERSION")) }

func revokeCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:          "revoke <node id|name>",
		Short:        "Hub: revoke a node (its session tokens stop working at once)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			rec, err := RevokeNode(app, args[0], true)
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %s (%s)\n", rec.GetString("name"), rec.Id)
			return nil
		},
	}
}

// Peer is one row of `toki sync peers`.
type Peer struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Profile       string `json:"profile"`
	Status        string `json:"status"`
	PulledSeq     int64  `json:"pulled_seq"`
	LagSeq        int64  `json:"lag_seq"`
	LastSeen      string `json:"last_seen"`
	SchemaVersion int64  `json:"schema_version"`
	ClockOffsetMs int64  `json:"clock_offset_ms"`
	CertExpires   string `json:"cert_expires"`
}

// ListPeers returns the nodes of the hub.
func ListPeers(app core.App) ([]Peer, error) {
	recs, err := app.FindRecordsByFilter(NodesCollection, "", "name", 0, 0)
	if err != nil {
		return nil, err
	}
	var head int64
	if app.HasTable("_changes") {
		_ = app.DB().NewQuery("SELECT COALESCE(MAX(seq),0) FROM _changes").Row(&head)
	}
	out := make([]Peer, 0, len(recs))
	for _, r := range recs {
		p := Peer{
			ID: r.Id, Name: r.GetString("name"), Profile: r.GetString("profile"), Status: r.GetString("status"),
			PulledSeq: int64(r.GetFloat("pulled_seq")), SchemaVersion: int64(r.GetFloat("schema_version")),
			ClockOffsetMs: int64(r.GetFloat("clock_offset_ms")),
		}
		if p.Status == NodeActive || p.Status == NodeStale {
			p.LagSeq = max(head-p.PulledSeq, 0)
		}
		if t := r.GetDateTime("last_seen"); !t.IsZero() {
			p.LastSeen = t.Time().UTC().Format(proto.TimeLayout)
		}
		if t := r.GetDateTime("cert_expires"); !t.IsZero() {
			p.CertExpires = t.Time().UTC().Format(proto.TimeLayout)
		}
		out = append(out, p)
	}
	return out, nil
}

func peersCommand(app core.App) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:          "peers [--json]",
		Short:        "Hub: list nodes with status, lag, last seen, schema and clock offset",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := requireRole(RoleHub); err != nil {
				return err
			}
			peers, err := ListPeers(app)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				b, _ := json.Marshal(peers)
				fmt.Fprintln(out, string(b))
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tPROFILE\tSTATUS\tLAG\tLAST SEEN\tSCHEMA\tOFFSET(ms)")
			for _, p := range peers {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\n", p.ID, p.Name, p.Profile, p.Status, p.LagSeq, dash(p.LastSeen), p.SchemaVersion, p.ClockOffsetMs)
			}
			return w.Flush()
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return c
}
