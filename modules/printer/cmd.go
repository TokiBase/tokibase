//go:build !no_printer

package printer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/escpos"
)

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// testPage is the self-test sheet: name, time, a column ruler and optionally a QR.
func testPage(p *Printer, qr bool) ([]byte, error) {
	b := escpos.New(p.codepage())
	b.QRRaster = !p.QRNative
	b.Init().Align(escpos.AlignCenter).Bold(true).Line("TOKIBASE PRINT TEST").Bold(false)
	b.Align(escpos.AlignLeft).Line("printer: " + p.Name)
	b.Line("time:    " + time.Now().Format("2006-01-02 15:04:05"))
	b.Line(fmt.Sprintf("cols:    %d", p.Cols))
	ruler := strings.Repeat("1234567890", p.Cols/10+1)[:p.Cols]
	b.Line(ruler)
	b.Line("abc ABC 123 éèü €")
	if qr {
		b.Align(escpos.AlignCenter)
		if err := b.QR("tokibase-print-test:"+p.Name, 6, escpos.QRMedium); err != nil {
			return nil, err
		}
		b.Align(escpos.AlignLeft)
	}
	return finish(p, b.Feed(3).Cut().Bytes()), nil
}

// NewCommand returns the `print` cobra command.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "print", Short: "Manage ESC/POS printers and print jobs"}
	ctx := context.Background()
	load := func(name string) (*Module, *Printer, error) {
		if err := ensureCollections(app); err != nil {
			return nil, nil, err
		}
		p, err := findPrinter(app, name)
		return New(app), p, err
	}

	var asJSON bool
	printers := &cobra.Command{
		Use: "printers", Short: "List the configured printers", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			list, err := ListPrinters(app)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(c.OutOrStdout(), list)
			}
			for _, p := range list {
				state := "enabled"
				if !p.Enabled {
					state = "disabled"
				}
				def := ""
				if p.Default {
					def = " default"
				}
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\tcols=%d\t%s%s\n", p.Name, p.Transport, p.Address, p.Cols, state, def)
			}
			return nil
		},
	}
	printers.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	var qr bool
	test := &cobra.Command{
		Use: "test <printer>", Short: "Print a self-test sheet directly (bypasses the queue)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			m, p, err := load(args[0])
			if err != nil {
				return err
			}
			page, err := testPage(p, qr)
			if err != nil {
				return err
			}
			res, reason, err := m.transmit(ctx, p, page, 1)
			if err != nil {
				return err
			}
			if res == outWait {
				return fmt.Errorf("printer needs attention: %s", reason)
			}
			fmt.Fprintf(c.OutOrStdout(), "sent %d bytes to %s\n", len(page), p.Name)
			return nil
		},
	}
	test.Flags().BoolVar(&qr, "qr", false, "include a QR code")

	var data, key string
	var copies int
	send := &cobra.Command{
		Use: "send <printer> <template>", Short: "Queue a print from a template", Args: cobra.ExactArgs(2), SilenceUsage: true,
		Example: `  toki print send counter ticket --data '{"plate":"B 1234 XY","ticket_id":"T-0001"}'`,
		RunE: func(c *cobra.Command, args []string) error {
			var d any
			if data != "" {
				if err := json.Unmarshal([]byte(data), &d); err != nil {
					return fmt.Errorf("--data: %w", err)
				}
			}
			res, err := New(app).Enqueue(ctx, Request{Printer: args[0], Template: args[1], Data: d, Copies: copies, IdempotencyKey: key, Actor: "cli"})
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "queued %s (%s)\n", res.ID, res.State)
			return nil
		},
	}
	send.Flags().StringVar(&data, "data", "", "template data as JSON")
	send.Flags().IntVar(&copies, "copies", 1, "number of copies")
	send.Flags().StringVar(&key, "key", "", "idempotency key")

	var state string
	var limit int
	var jobsJSON bool
	jobs := &cobra.Command{
		Use: "jobs", Short: "List print jobs, newest first", SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			list, err := ListJobs(app, state, limit)
			if err != nil {
				return err
			}
			if jobsJSON {
				return printJSON(c.OutOrStdout(), list)
			}
			for _, j := range list {
				fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\tattempts=%d\t%s\n", j.ID, j.State, j.Printer, j.Template, j.Attempts, j.LastError)
			}
			return nil
		},
	}
	jobs.Flags().StringVar(&state, "state", "", "filter by state (queued, printing, waiting_paper, done, failed, dead)")
	jobs.Flags().IntVar(&limit, "limit", 50, "maximum rows")
	jobs.Flags().BoolVar(&jobsJSON, "json", false, "output JSON")

	retry := &cobra.Command{
		Use: "retry <id>", Short: "Queue a failed or dead job again", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := ensureCollections(app); err != nil {
				return err
			}
			res, err := New(app).Retry(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "queued %s again\n", res.ID)
			return nil
		},
	}

	status := &cobra.Command{
		Use: "status <printer>", Short: "Read the real-time status of a printer (DLE EOT)", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			m, p, err := load(args[0])
			if err != nil {
				return err
			}
			if p.Transport == "file" {
				return fmt.Errorf("%s: a file transport has no status channel", p.Name)
			}
			conn, err := m.Open(ctx, p)
			if err != nil {
				return err
			}
			defer conn.Close()
			st, ok, err := queryStatus(conn)
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(c.OutOrStdout(), "%s: no status answer (the printer may not support DLE EOT)\n", p.Name)
				return nil
			}
			fmt.Fprintf(c.OutOrStdout(), "%s: %s\n", p.Name, describe(st))
			if st.PaperNear {
				fmt.Fprintln(c.OutOrStdout(), "paper is running low")
			}
			return nil
		},
	}

	root.AddCommand(printers, test, send, jobs, retry, status)
	return root
}
