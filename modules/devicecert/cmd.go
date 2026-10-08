//go:build !no_devicecert

package devicecert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tokibase/tokibase/kernel"
	"io"
	"os"
	"os/exec"
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
	if !Enabled() {
		return nil, errors.New("devicecert is off (set TOKI_DEVICECERT=on); nothing is created while it is off")
	}
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
			if !Enabled() { // read-only: do not create tables while the module is off
				h := New(app).Health()
				if asJSON {
					return printJSON(c.OutOrStdout(), h)
				}
				fmt.Fprintln(c.OutOrStdout(), "enabled:   false (set TOKI_DEVICECERT=on)")
				return nil
			}
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
			fmt.Fprintf(w, "enabled:   %v\nrole:      %s\nlisten:    %s\nmtls:      %s\nleaf_days: %d\ncas:       %d\ndeny_list: %d\n", h.Enabled, h.Role, orDash(h.Listen), h.MTLS, h.LeafDays, h.CAs, h.DenyList)
			if h.HubKeyInDB {
				fmt.Fprintln(w, "warning:   the hub key is in data.db next to the wrapped CA key (set TOKI_SYNC_HUB_KEY_FILE)")
			}
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

	root.AddCommand(ca, status, list, issueCommand(app), revokeCommand(app), rotateCommand(app))
	return root
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func issueCommand(app core.App) *cobra.Command {
	var name, scope, out, p12Pass string
	var days int
	var p12 bool
	cmd := &cobra.Command{
		Use: "issue", Short: "Hub: issue a client certificate for a LAN peer (gate controller, scanner box)", SilenceUsage: true,
		Long: "Issues a clientAuth certificate and writes <name>.key.pem (0600), <name>.crt.pem and ca.pem to --out.\n" +
			"--scope lists the route prefixes the certificate may call without a token (for example\n" +
			"/api/scan,/api/print,/api/kiosk/status). Without --scope the certificate only proves identity and grants no route.\n" +
			"--p12 also writes <name>.p12 (needs the openssl binary; the password is --p12-pass or a random one printed once).",
		RunE: func(c *cobra.Command, _ []string) error {
			if name == "" {
				return errors.New("--name is required")
			}
			if !validDNS(name) && !validLabel(name) {
				return errors.New("--name may contain letters, digits, '-', '_' and '.'")
			}
			m, err := cliModule(app)
			if err != nil {
				return err
			}
			cert, err := m.Issue(c.Context(), kernel.DeviceCertRequest{Name: name, Kind: kernel.DeviceCertClient, Days: days, RouteScope: scope})
			if err != nil {
				return err
			}
			if err := os.MkdirAll(out, 0o700); err != nil {
				return err
			}
			keyPath, crtPath, caPath := filepath.Join(out, name+".key.pem"), filepath.Join(out, name+".crt.pem"), filepath.Join(out, "ca.pem")
			if err := writeFileAtomic(keyPath, cert.KeyPEM, 0o600); err != nil {
				return err
			}
			if err := writeFileAtomic(crtPath, cert.CertPEM, 0o644); err != nil {
				return err
			}
			if err := writeFileAtomic(caPath, cert.CAPEM, 0o644); err != nil {
				return err
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "serial:     %s\nname:       %s\nnot_after:  %s\nroute_scope: %s\nkey:        %s\ncert:       %s\nca:         %s\n",
				cert.Serial, cert.Name, cert.NotAfter.UTC().Format(time.RFC3339), orDash(cert.RouteScope), keyPath, crtPath, caPath)
			if p12 {
				p12Path := filepath.Join(out, name+".p12")
				pass := p12Pass
				if pass == "" {
					pass = randomPass()
					fmt.Fprintf(os.Stderr, "p12 password: %s\n", pass)
				}
				if err := makeP12(keyPath, crtPath, caPath, p12Path, pass); err != nil {
					return err
				}
				fmt.Fprintf(w, "p12:        %s\n", p12Path)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the peer (CN), for example gate-ctrl-1")
	f.IntVar(&days, "days", 90, "validity in days (at most 365)")
	f.StringVar(&scope, "scope", "", "comma separated route prefixes the certificate may call without a token")
	f.StringVar(&out, "out", ".", "directory for the PEM files")
	f.BoolVar(&p12, "p12", false, "also write a PKCS#12 bundle (uses openssl)")
	f.StringVar(&p12Pass, "p12-pass", "", "password of the .p12 (default: random, printed to stderr)")
	return cmd
}

func validLabel(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func randomPass() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// makeP12 builds a PKCS#12 file with the openssl binary (the Go standard
// library has no encoder). The password goes through the environment, never
// the command line.
func makeP12(key, crt, ca, out, pass string) error {
	bin, err := exec.LookPath("openssl")
	if err != nil {
		return errors.New("--p12 needs the openssl binary in PATH")
	}
	cmd := exec.Command(bin, "pkcs12", "-export", "-inkey", key, "-in", crt, "-certfile", ca, "-out", out, "-passout", "env:TOKI_P12_PASS")
	cmd.Env = append(os.Environ(), "TOKI_P12_PASS="+pass)
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("openssl pkcs12 failed: %v: %s", err, b)
	}
	return os.Chmod(out, 0o600)
}

func revokeCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "revoke <serial|name>", Short: "Hub: revoke a certificate (a name revokes all its unrevoked certificates)", SilenceUsage: true,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			m, err := cliModule(app)
			if err != nil {
				return err
			}
			if err := m.Revoke(c.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %s (nodes learn it with the next sync pull, within a few seconds when online)\n", args[0])
			return nil
		},
	}
}

func rotateCommand(app core.App) *cobra.Command {
	var overlap int
	cmd := &cobra.Command{
		Use: "rotate-ca", Short: "Hub: add a new CA; the old one stays trusted for the overlap window", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			m, err := cliModule(app)
			if err != nil {
				return err
			}
			if overlap < 0 {
				overlap = CAOverlapDays()
			}
			ca, retire, err := m.RotateCA(overlap)
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			fmt.Fprintf(w, "new root:    %s\nold root retires: %s\n", ca.Fingerprint(), retire.UTC().Format(time.RFC3339))
			fmt.Fprintln(w, "Install the new root (toki devicecert ca) on browsers and peers before the old one retires; nodes receive it with their next leaf.")
			return nil
		},
	}
	cmd.Flags().IntVar(&overlap, "overlap-days", -1, "days the old root stays trusted (default TOKI_DEVICECERT_CA_OVERLAP_DAYS, 30)")
	return cmd
}
