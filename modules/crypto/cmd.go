package crypto

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// NewCommand creates the `crypto` command (status, enable, disable, rotate,
// retire, verify).
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "crypto",
		Short: "Per-field encryption at rest (AES-256-GCM, envelope keys)",
		Long: "Encrypts selected text/editor/json/email/url fields in the database; backups and replicas hold ciphertext.\n" +
			"Needs a 32 byte master key (base64) in " + EnvMasterKey + " or the file named by " + EnvMasterKeyFile + ".\n" +
			"Keep the master key OUTSIDE pb_data and back it up separately: without it the data is unrecoverable.\n" +
			"Modes: random (non-deterministic, no filtering or sorting) and blind-index (random + exact-match lookup via\n" +
			"/api/crypto/lookup/{collection}/{field}?value=). Filters and sorts on encrypted fields are rejected with 400 " + ErrCode + ".",
	}
	out := func(c *cobra.Command) func(string) {
		return func(s string) { fmt.Fprintln(c.OutOrStdout(), s) }
	}
	ensure := func() error {
		if From(app) == nil {
			return errors.New("crypto module is not registered")
		}
		return nil
	}

	var asJSON bool
	status := &cobra.Command{
		Use: "status", Short: "Show master key state, encrypted fields and key versions", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			r, err := Status(app)
			if err != nil {
				return err
			}
			if asJSON {
				fmt.Fprintln(c.OutOrStdout(), r.JSON())
				return nil
			}
			p := out(c)
			if r.MasterKey {
				p("master key: present")
			} else if r.MasterKeyError != "" {
				p("master key: INVALID (" + r.MasterKeyError + ")")
			} else {
				p("master key: missing (module inactive; set " + EnvMasterKey + " or " + EnvMasterKeyFile + ")")
			}
			if r.MasterKeyInDir {
				p("WARNING: the master key file is inside pb_data; a backup of pb_data would contain the key")
			}
			p(fmt.Sprintf("superuser API shows plaintext: %v (%s)", r.AdminPlaintext, EnvAdminPlaintext))
			if len(r.Collections) == 0 {
				p("no encrypted fields")
			}
			for _, cs := range r.Collections {
				names := make([]string, 0, len(cs.Fields))
				for f := range cs.Fields {
					names = append(names, f)
				}
				sort.Strings(names)
				for _, f := range names {
					p(fmt.Sprintf("%s.%s\tmode=%s", cs.Collection, f, cs.Fields[f]))
				}
				for _, k := range cs.Keys {
					st := "readable"
					if k.Active {
						st = "active"
					}
					if k.RetiredAt != "" {
						st = "retired " + k.RetiredAt
					}
					p(fmt.Sprintf("  key %s v%d\t%s", cs.Collection, k.Version, st))
				}
			}
			for _, w := range r.Warnings {
				p(fmt.Sprintf("WARNING %s.%s (%s): %s", w.Collection, w.Field, w.Where, w.Message))
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var mode string
	var background bool
	progress := func(c *cobra.Command) Progress { return func(s string) { fmt.Fprintln(c.ErrOrStderr(), s) } }
	enable := &cobra.Command{
		Use: "enable <collection> <field>", Short: "Encrypt a field (encrypts existing rows in batches of 500)",
		Example: "crypto enable patients diagnosis\ncrypto enable users phone --mode blind-index",
		Args:    cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			if background {
				if !From(app).Active() {
					return ErrNoMasterKey
				}
				id, err := Enqueue(app, "enable", args[0], args[1], mode)
				if err != nil {
					return err
				}
				fmt.Fprintln(c.OutOrStdout(), "enqueued job", id, "(needs a running worker with the master key)")
				return nil
			}
			n, err := Enable(app, args[0], args[1], mode, progress(c))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "enabled %s.%s (mode %s): %d values encrypted\n", args[0], args[1], mode, n)
			return nil
		},
	}
	enable.Flags().StringVar(&mode, "mode", ModeRandom, "random|blind-index")
	enable.Flags().BoolVar(&background, "background", false, "enqueue as a kernel job instead of running now")

	var understand bool
	disable := &cobra.Command{
		Use: "disable <collection> <field>", Short: "Decrypt a field and stop encrypting it (needs --i-understand)",
		Args: cobra.ExactArgs(2), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			if !understand {
				return errors.New("this writes the plaintext of " + args[0] + "." + args[1] + " back to the database (and into every backup from now on); pass --i-understand to continue")
			}
			n, err := Disable(app, args[0], args[1], progress(c))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "disabled %s.%s: %d values decrypted\n", args[0], args[1], n)
			return nil
		},
	}
	disable.Flags().BoolVar(&understand, "i-understand", false, "confirm that plaintext will be stored again")

	rotate := &cobra.Command{
		Use: "rotate <collection>", Short: "New data key version, re-encrypt rows (old versions stay readable until retire)",
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			if background {
				if !From(app).Active() {
					return ErrNoMasterKey
				}
				id, err := Enqueue(app, "rotate", args[0], "", "")
				if err != nil {
					return err
				}
				fmt.Fprintln(c.OutOrStdout(), "enqueued job", id)
				return nil
			}
			v, n, err := Rotate(app, args[0], progress(c))
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "rotated %s to key v%d: %d values re-encrypted\n", args[0], v, n)
			return nil
		},
	}
	rotate.Flags().BoolVar(&background, "background", false, "enqueue as a kernel job instead of running now")

	retire := &cobra.Command{
		Use: "retire <collection>", Short: "Destroy old key versions that no row uses any more",
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			r, err := Retire(app, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "retired versions: %v\n", r.Retired)
			for v, why := range r.InUse {
				fmt.Fprintf(c.OutOrStdout(), "kept v%d: %s\n", v, why)
			}
			if len(r.InUse) > 0 {
				return errors.New("some versions are still in use (run `crypto rotate` again)")
			}
			return nil
		},
	}

	var sample int
	verify := &cobra.Command{
		Use: "verify <collection>", Short: "Decrypt a random sample of rows and report failures",
		Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensure(); err != nil {
				return err
			}
			r, err := Verify(app, args[0], sample)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(r, "", "  ")
				fmt.Fprintln(c.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(c.OutOrStdout(), "%s: %d rows sampled, %d values decrypted, %d plaintext, %d failures\n",
					r.Collection, r.Sampled, r.Decrypted, r.Plaintext, len(r.Failures))
				for _, f := range r.Failures {
					fmt.Fprintln(c.OutOrStdout(), "  "+f)
				}
			}
			if len(r.Failures) > 0 {
				return errors.New("verification failed")
			}
			return nil
		},
	}
	verify.Flags().IntVar(&sample, "sample", 100, "rows to sample")
	verify.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	root.AddCommand(status, enable, disable, rotate, retire, verify)
	return root
}
