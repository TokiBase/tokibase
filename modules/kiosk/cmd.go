//go:build !no_kiosk

package kiosk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func pairURL(base, code string) string {
	return strings.TrimRight(base, "/") + "/kiosk/pair#" + code
}

// NewCommand returns the `kiosk` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "kiosk", Short: "Provision and manage kiosk devices"}
	load := func(name string) (*core.Record, error) {
		if err := ensureCollections(app); err != nil {
			return nil, err
		}
		r, err := findByName(app, name)
		if err != nil {
			return nil, fmt.Errorf("unknown kiosk device %q", name)
		}
		return r, nil
	}

	var name, actor, pin, bindIP, nodeID, base string
	var ttlHours, lockAfter int
	var expires time.Duration
	provision := &cobra.Command{
		Use: "provision", Short: "Create a device and print its one-time pairing URL", SilenceUsage: true,
		Example: `  toki kiosk provision --name gate-1 --actor gate_devices/abc --pin 1234`,
		Long: "Creates the device and prints http://127.0.0.1:8090/kiosk/pair#<code>. Open it once in the kiosk\n" +
			"browser on the device itself; the code is single use and expires (--expires). The secret is in\n" +
			"the URL fragment, so it never reaches a log.",
		RunE: func(c *cobra.Command, _ []string) error {
			col, id, ok := strings.Cut(actor, "/")
			if !ok || col == "" || id == "" {
				return errors.New("--actor must be <auth collection>/<record id>")
			}
			d := &Device{Name: name, AuthCollection: col, AuthRecord: id, NodeID: nodeID, BindIP: bindIP, TTLHours: ttlHours, LockAfterS: lockAfter}
			if err := d.Validate(); err != nil {
				return err
			}
			_, code, err := Provision(app, d, pin, time.Now().UTC(), expires)
			if err != nil {
				return err
			}
			fmt.Fprintln(c.OutOrStdout(), pairURL(base, code))
			return nil
		},
	}
	provision.Flags().StringVar(&name, "name", "", "device name (required)")
	provision.Flags().StringVar(&actor, "actor", "", "service actor, <auth collection>/<record id> (required)")
	provision.Flags().StringVar(&pin, "pin", "", "PIN for the lock overlay (4-64 characters)")
	provision.Flags().StringVar(&bindIP, "bind-ip", "", "only accept this client IP or CIDR")
	provision.Flags().StringVar(&nodeID, "node-id", "", "bind to this sync node id")
	provision.Flags().StringVar(&base, "url", "http://127.0.0.1:8090", "base URL used in the pairing URL")
	provision.Flags().IntVar(&ttlHours, "ttl-hours", 0, "session lifetime in hours (default TOKI_KIOSK_SESSION_HOURS or 12)")
	provision.Flags().IntVar(&lockAfter, "lock-after", 0, "idle seconds before kiosk.js locks the screen (0 = never)")
	provision.Flags().DurationVar(&expires, "expires", pairingTTL, "lifetime of the pairing code")
	_ = provision.MarkFlagRequired("name")
	_ = provision.MarkFlagRequired("actor")

	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List kiosk devices", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			all, err := listDevices(app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), all)
			}
			for _, d := range all {
				state := "unpaired"
				switch {
				case d.Locked:
					state = "locked"
				case d.Paired:
					state = "paired"
				case d.PairingPending:
					state = "pairing"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s/%s\tttl=%dh\tlast_seen=%s\n", d.Name, state, d.AuthCollection, d.AuthRecord, d.TTLHours, d.LastSeen)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var rExpires time.Duration
	var rBase string
	rotate := &cobra.Command{
		Use: "rotate <name>", Short: "Drop the device token and print a new pairing URL", SilenceUsage: true, Args: cobra.ExactArgs(1),
		Long: "The paired browser stops working at once (its cookie no longer matches) and every token the\n" +
			"device already holds is revoked (device generation + recorded sessions).",
		RunE: func(c *cobra.Command, args []string) error {
			r, err := load(args[0])
			if err != nil {
				return err
			}
			code := newPairing(r, time.Now().UTC(), rExpires)
			if err := app.Save(r); err != nil {
				return err
			}
			if _, err := RevokeDevice(app, r.Id, "kiosk rotate"); err != nil {
				return fmt.Errorf("the device was rotated, but its tokens could not be revoked: %w", err)
			}
			fmt.Fprintln(c.OutOrStdout(), pairURL(rBase, code))
			return nil
		},
	}
	rotate.Flags().DurationVar(&rExpires, "expires", pairingTTL, "lifetime of the pairing code")
	rotate.Flags().StringVar(&rBase, "url", "http://127.0.0.1:8090", "base URL used in the pairing URL")

	var sessions bool
	revoke := &cobra.Command{
		Use: "revoke <name>", Short: "Unpair a device and revoke its tokens (the row stays, nothing can pair it until `rotate`)", SilenceUsage: true, Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := load(args[0])
			if err != nil {
				return err
			}
			r.Set("token_hash", "")
			r.Set("pairing_hash", "")
			r.Set("pairing_expires", "")
			if err := app.Save(r); err != nil {
				return err
			}
			n, err := RevokeDevice(app, r.Id, "kiosk revoke")
			if err != nil {
				return fmt.Errorf("the device is unpaired, but its tokens could not be revoked: %w", err)
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %d session(s) of the device\n", n)
			if sessions {
				if kernel.RevokeUserSessions == nil {
					return errors.New("the device is unpaired, but sessions are not available to revoke its tokens (the device tokens are dead through the device generation)")
				}
				n, err := kernel.RevokeUserSessions(app, r.GetString("auth_collection"), r.GetString("auth_record"), "kiosk revoke")
				if err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "revoked %d session(s) of %s/%s (every device that shares this actor)\n", n, r.GetString("auth_collection"), r.GetString("auth_record"))
			}
			fmt.Fprintf(c.OutOrStdout(), "revoked %s\n", args[0])
			return nil
		},
	}
	revoke.Flags().BoolVar(&sessions, "sessions", false, "also revoke every active session of the service actor (every device sharing it, and the actor's other logins)")

	var newPin string
	var clearPin bool
	setPin := &cobra.Command{
		Use: "set-pin <name>", Short: "Set or clear the PIN of a device", SilenceUsage: true, Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := load(args[0])
			if err != nil {
				return err
			}
			switch {
			case clearPin:
				r.Set("pin_hash", "")
			case newPin != "":
				h, err := HashPin(newPin)
				if err != nil {
					return err
				}
				r.Set("pin_hash", h)
			default:
				return errors.New("give --pin <PIN> or --clear")
			}
			return app.Save(r)
		},
	}
	setPin.Flags().StringVar(&newPin, "pin", "", "the new PIN (4-64 characters)")
	setPin.Flags().BoolVar(&clearPin, "clear", false, "remove the PIN")

	root.AddCommand(provision, list, rotate, revoke, setPin)
	return root
}
