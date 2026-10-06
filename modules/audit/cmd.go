package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// ParseSince accepts a duration ("90m", "1h", "7d") or a date/RFC3339 time
// and returns the lower bound as a time (zero when s is empty).
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil {
			return now.Add(-time.Duration(n * 24 * float64(time.Hour))), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --since %q (use a duration like 1h/7d or a date)", s)
}

// Query returns entries newest first (tail) or oldest first (export).
func Query(app core.App, since time.Time, limit int, newestFirst bool) ([]Entry, error) {
	if !app.AuxHasTable(TableName) {
		return nil, nil
	}
	q := app.AuxDB().Select("*").From(TableName)
	if !since.IsZero() {
		q.AndWhere(dbxGTE("created", since.UTC().Format("2006-01-02 15:04:05.000Z")))
	}
	if newestFirst {
		q.OrderBy("seq DESC")
	} else {
		q.OrderBy("seq ASC")
	}
	if limit > 0 {
		q.Limit(int64(limit))
	}
	var out []Entry
	return out, q.All(&out)
}

// NewCommand returns the `audit` cobra command (tail, verify, export).
// There is deliberately no prune/delete command: the log is append-only.
func NewCommand(app core.App) *cobra.Command {
	root := &cobra.Command{
		Use:   "audit",
		Short: "Inspect the append-only audit log",
	}
	root.AddCommand(tailCommand(app), verifyCommand(app), exportCommand(app))
	return root
}

func tailCommand(app core.App) *cobra.Command {
	var since string
	var limit int
	var asJSON bool
	c := &cobra.Command{
		Use:          "tail",
		Short:        "Show the most recent audit entries",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			from, err := ParseSince(since, time.Now())
			if err != nil {
				return err
			}
			rows, err := Query(app, from, limit, true)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			for i := len(rows) - 1; i >= 0; i-- { // print oldest to newest
				if asJSON {
					b, _ := json.Marshal(rows[i])
					fmt.Fprintln(out, string(b))
					continue
				}
				r := rows[i]
				fmt.Fprintf(out, "%d  %s  %s:%s  %s  %s/%s\n", r.Seq, r.Created, r.ActorKind, dash(r.ActorID), r.Action, r.Collection, r.Record)
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "", "only entries newer than a duration (1h, 7d) or date")
	c.Flags().IntVar(&limit, "limit", 50, "maximum number of entries (0 = all)")
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per line")
	return c
}

func verifyCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:          "verify",
		Short:        "Verify the hash chain (exit code 1 on the first broken seq)",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			res, err := Verify(app)
			if err != nil {
				return err
			}
			if !res.OK {
				fmt.Fprintf(c.OutOrStdout(), "BROKEN at seq %d: %s\n", res.BrokenSeq, res.Reason)
				return fmt.Errorf("audit chain broken at seq %d", res.BrokenSeq)
			}
			fmt.Fprintf(c.OutOrStdout(), "OK: %d entries verified\n", res.Rows)
			return nil
		},
	}
}

func exportCommand(app core.App) *cobra.Command {
	var since, outFile string
	c := &cobra.Command{
		Use:          "export",
		Short:        "Export audit entries as JSON lines",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			from, err := ParseSince(since, time.Now())
			if err != nil {
				return err
			}
			rows, err := Query(app, from, 0, false)
			if err != nil {
				return err
			}
			var w io.Writer = c.OutOrStdout()
			if outFile != "" {
				f, err := os.OpenFile(outFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return errors.New("cannot create --out file (it must not already exist): " + err.Error())
				}
				defer f.Close()
				w = f
			}
			enc := json.NewEncoder(w)
			for _, r := range rows {
				if err := enc.Encode(r); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&since, "since", "", "only entries newer than a duration (1h, 7d) or date")
	c.Flags().StringVar(&outFile, "out", "", "write to this new file instead of stdout")
	return c
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
