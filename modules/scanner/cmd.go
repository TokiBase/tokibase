//go:build !no_scanner

package scanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// listDevices returns the stable device links under dir and their targets.
func listDevices(dir string) []string {
	ents, _ := filepath.Glob(filepath.Join(dir, "*"))
	out := make([]string, 0, len(ents))
	for _, p := range ents {
		t, err := filepath.EvalSymlinks(p)
		if err != nil {
			t = "?"
		}
		out = append(out, p+" -> "+t)
	}
	return out
}

// NewCommand returns the `scan` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "scan", Short: "Manage barcode/QR scanners and scan events"}

	var asJSON bool
	list := &cobra.Command{
		Use: "list", Short: "List configured scanners", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			all, err := loadAll(app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), all)
			}
			for _, s := range all {
				state := "enabled"
				if !s.Enabled {
					state = "disabled"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\tdedupe=%dms\n", s.Name, s.Kind, state, s.Device, s.DedupeMs)
			}
			return nil
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var device, kind string
	var baud int
	var grab bool
	listen := &cobra.Command{
		Use: "listen [scanner]", Short: "Print raw codes of a scanner (setup aid)", SilenceUsage: true, Args: cobra.MaximumNArgs(1),
		Long: "Opens the device of a configured scanner, or --device path, and prints every code as it\n" +
			"arrives, before any prefix/suffix/length/charset filter. Stop with Ctrl-C. Stop the server\n" +
			"reader of the same scanner first (a serial port has one reader).",
		RunE: func(c *cobra.Command, args []string) error {
			if (len(args) == 1) == (device != "") {
				return errors.New("give either a scanner name or --device path")
			}
			sc := &Scanner{Name: "listen", Kind: kind, Device: device, Baud: baud, Grab: grab, Enabled: true}
			if len(args) == 1 {
				all, err := loadAll(app)
				if err != nil {
					return err
				}
				sc = nil
				for _, s := range all {
					if s.Name == args[0] {
						sc = s
					}
				}
				if sc == nil {
					return fmt.Errorf("unknown scanner %q", args[0])
				}
				if sc.Kind == KindWeb {
					return errors.New("a web scanner has no device; open the page that uses wedge.js")
				}
			} else if sc.Kind == "" {
				sc.Kind = KindSerial
				if strings.HasPrefix(device, "/dev/input/") {
					sc.Kind = KindEvdev
				}
			}
			sc.applyDefaults()
			if err := sc.Validate(); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			rc, err := openDevice(sc)
			if err != nil {
				return err
			}
			go func() { <-ctx.Done(); rc.Close() }()
			defer rc.Close()
			fmt.Fprintf(c.ErrOrStderr(), "listening on %s (%s), Ctrl-C to stop\n", sc.Device, sc.Kind)
			var next func() (string, error)
			if sc.Kind == KindEvdev {
				ks, err := devio.NewKeyScanner(rc, devio.EventSize, 0)
				if err != nil {
					return err
				}
				next = ks.Next
			} else {
				next = devio.NewLineReader(rc, 0).Next
			}
			for {
				s, err := next()
				if errors.Is(err, devio.ErrLineTooLong) || errors.Is(err, devio.ErrScanTooLong) {
					fmt.Fprintln(c.OutOrStdout(), "(line too long, dropped)")
					continue
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil
					}
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "%q\n", s)
			}
		},
	}
	listen.Flags().StringVar(&device, "device", "", "device path instead of a configured scanner")
	listen.Flags().StringVar(&kind, "kind", "", "serial or evdev (default: guessed from the path)")
	listen.Flags().IntVar(&baud, "baud", 0, "serial baud rate (default 9600)")
	listen.Flags().BoolVar(&grab, "grab", false, "evdev: EVIOCGRAB the device")

	var scannerName, url, token string
	simulate := &cobra.Command{
		Use: "simulate <code>", Short: "Inject a scan (tests, demos)", SilenceUsage: true, Args: cobra.ExactArgs(1),
		Long: "Without --url the scan is written to the local database; a running server stores it but\n" +
			"does not publish it on @scan (another process). With --url and --token it calls POST /api/scan\n" +
			"of a running server, which publishes it.",
		RunE: func(c *cobra.Command, args []string) error {
			if url != "" {
				body, _ := json.Marshal(map[string]any{"scanner": scannerName, "code": args[0]})
				req, _ := http.NewRequestWithContext(c.Context(), "POST", strings.TrimRight(url, "/")+"/api/scan", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", token)
				cl := &http.Client{Timeout: 10 * time.Second}
				res, err := cl.Do(req)
				if err != nil {
					return err
				}
				defer res.Body.Close()
				b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
				fmt.Fprintf(c.OutOrStdout(), "%s\n", strings.TrimSpace(string(b)))
				if res.StatusCode >= 300 {
					return fmt.Errorf("HTTP %d", res.StatusCode)
				}
				return nil
			}
			if !Enabled() {
				return errors.New("the scanner module is off; set TOKI_SCANNER=on")
			}
			if err := ensureCollections(app); err != nil {
				return err
			}
			m := newModule(app)
			sc, status, msg := m.webScanner(scannerName)
			if sc == nil {
				return fmt.Errorf("%s (%d)", msg, status)
			}
			res, err := m.Ingest(context.Background(), sc, args[0], IngestOptions{Source: "cli", Actor: "cli"})
			if err != nil {
				return err
			}
			return printJSON(c.OutOrStdout(), res)
		},
	}
	simulate.Flags().StringVar(&scannerName, "scanner", "", "web scanner name (default: the first enabled web scanner)")
	simulate.Flags().StringVar(&url, "url", "", "base URL of a running server")
	simulate.Flags().StringVar(&token, "token", "", "auth token for --url")

	devices := &cobra.Command{
		Use: "devices", Short: "List scanner device candidates", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			w := c.OutOrStdout()
			for _, dir := range []string{"/dev/input/by-id", "/dev/serial/by-id"} {
				fmt.Fprintln(w, dir+":")
				l := listDevices(dir)
				if len(l) == 0 {
					fmt.Fprintln(w, "  (none)")
				}
				for _, d := range l {
					fmt.Fprintln(w, "  "+d)
				}
			}
			return nil
		},
	}

	root.AddCommand(list, listen, simulate, devices)
	return root
}
