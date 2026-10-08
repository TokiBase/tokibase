//go:build !no_devicecert

package devicecert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// cliModule is the registered module of app or a fresh one with the schema
// ensured (the commands work with TOKI_DEVICECERT unset).
func cliModule(app core.App) (*Module, error) {
	if err := ensureSchema(app); err != nil {
		return nil, err
	}
	m := New(app)
	if err := m.leaf.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// rootPEM returns the root certificate: the CA on the hub, the stored root of
// the last leaf on a node.
func (m *Module) rootPEM(create bool) ([]byte, error) {
	if m.IsHub() {
		ca, err := m.CA(create)
		if err != nil {
			return nil, err
		}
		if ca == nil {
			return nil, errors.New("devicecert: the hub has no CA yet (run `toki devicecert ca`)")
		}
		return ca.PEM, nil
	}
	b, err := os.ReadFile(filepath.Join(m.app.DataDir(), CAFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("devicecert: this node has no root yet: it is fetched from the hub with the first leaf (TOKI_DEVICECERT=on and a sync handshake)")
		}
		return nil, err
	}
	return b, nil
}

// NewCommand returns the `devicecert` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "devicecert", Short: "Hub CA and edge TLS certificates"}

	var fpOnly bool
	ca := &cobra.Command{
		Use: "ca", Short: "Print the root certificate (PEM) and its SHA-256 fingerprint", SilenceUsage: true,
		Long: "Prints the root certificate to install in the trust store of browsers and tablets that talk\n" +
			"to an edge node. The PEM goes to stdout, the fingerprint to stderr (or alone with --fingerprint).\n" +
			"On the hub the CA is created on first use; on a node the root received with the leaf is printed.",
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := cliModule(app)
			if err != nil {
				return err
			}
			b, err := m.rootPEM(true)
			if err != nil {
				return err
			}
			cert, err := ParseCertPEM(b)
			if err != nil {
				return err
			}
			if fpOnly {
				fmt.Fprintln(c.OutOrStdout(), Fingerprint(cert.Raw))
				return nil
			}
			if _, err := c.OutOrStdout().Write(b); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "SHA256 Fingerprint=%s\nsubject=%s\nnot_after=%s\n", Fingerprint(cert.Raw), cert.Subject.CommonName, cert.NotAfter.UTC().Format(time.RFC3339))
			return nil
		},
	}
	ca.Flags().BoolVar(&fpOnly, "fingerprint", false, "print only the SHA-256 fingerprint")

	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show the edge certificate of this node and the listener settings", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := cliModule(app)
			if err != nil {
				return err
			}
			h := m.Health()
			if h.Role == "hub" && h.CAFingerpr == "" {
				if ca, _ := m.CA(false); ca != nil {
					h.CAFingerpr = ca.Fingerprint()
				}
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), h)
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "enabled:   %v\nrole:      %s\nlisten:    %s\nmtls:      %s\nleaf_days: %d\n", h.Enabled, h.Role, orDash(h.Listen), h.MTLS, h.LeafDays)
			if h.CAFingerpr != "" {
				fmt.Fprintf(w, "ca:        %s\n", h.CAFingerpr)
			}
			if h.Leaf == nil {
				fmt.Fprintln(w, "leaf:      none yet")
				return nil
			}
			fmt.Fprintf(w, "leaf:      %s\nnot_before: %s\nnot_after: %s\nnames:     %v %v\n", h.Leaf.Serial, h.Leaf.NotBefore, h.Leaf.NotAfter, h.Leaf.DNS, h.Leaf.IPs)
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	list := &cobra.Command{
		Use: "list", Short: "List the issued certificates (hub)", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if _, err := cliModule(app); err != nil {
				return err
			}
			recs, err := app.FindAllRecords(CertsCollection)
			if err != nil {
				return err
			}
			type row struct {
				Serial    string `json:"serial"`
				Name      string `json:"name"`
				Kind      string `json:"kind"`
				Node      string `json:"node,omitempty"`
				NotAfter  string `json:"not_after"`
				RevokedAt string `json:"revoked_at,omitempty"`
				Scope     string `json:"route_scope,omitempty"`
			}
			rows := make([]row, 0, len(recs))
			for _, r := range recs {
				rows = append(rows, row{
					Serial: r.GetString("serial"), Name: r.GetString("name"), Kind: r.GetString("kind"), Node: r.GetString("node"),
					NotAfter: r.GetDateTime("not_after").Time().UTC().Format(time.RFC3339), Scope: r.GetString("route_scope"),
				})
				if rv := r.GetDateTime("revoked_at"); !rv.IsZero() {
					rows[len(rows)-1].RevokedAt = rv.Time().UTC().Format(time.RFC3339)
				}
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), rows)
			}
			for _, r := range rows {
				state := "valid"
				if r.RevokedAt != "" {
					state = "revoked"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\tnot_after=%s\t%s\n", r.Serial, r.Name, r.Kind, orDash(r.Node), r.NotAfter, state)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	root.AddCommand(ca, status, list)
	return root
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
