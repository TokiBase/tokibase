//go:build !no_printer

// Package printer drives ESC/POS receipt printers from durable print jobs
// (see docs/modules/printer.md).
package printer

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/internal/escpos"
)

const (
	// PrintersCollection holds the printer config (superuser only).
	PrintersCollection = "_printers"
	// TemplatesCollection holds the versioned receipt templates.
	TemplatesCollection = "_print_templates"
	// JobsCollection holds the business record of every print job.
	JobsCollection = "_print_jobs"
	// JobKind is the kernel job kind of a transmission.
	JobKind = "print.send"

	// Audit actions.
	AuditJob  = "print.job"
	AuditDead = "print.dead"

	// Job states.
	StateQueued   = "queued"
	StatePrinting = "printing"
	StateWaiting  = "waiting_paper"
	StateDone     = "done"
	StateFailed   = "failed"
	StateDead     = "dead"

	timeLayout = "2006-01-02 15:04:05.000Z"
)

var jobStates = []string{StateQueued, StatePrinting, StateWaiting, StateDone, StateFailed, StateDead}

var (
	errNotFound = errors.New("not found")
	ensureMu    sync.Mutex
)

func floatPtr(f float64) *float64 { return &f }

// ensureCollections creates the three system collections when missing. All
// rules stay nil: superuser only (service actors use the /api/print routes).
func ensureCollections(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if !app.HasTable("_collections") {
		return errors.New("printer: _collections table is not ready")
	}
	if _, err := app.FindCachedCollectionByNameOrId(PrintersCollection); err != nil {
		c := core.NewBaseCollection(PrintersCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 100},
			&core.SelectField{Name: "transport", Required: true, MaxSelect: 1, Values: []string{"tcp", "serial", "file"}},
			&core.TextField{Name: "address", Required: true, Max: 300},
			&core.NumberField{Name: "baud", OnlyInt: true, Min: floatPtr(0)},
			&core.NumberField{Name: "cols", OnlyInt: true, Min: floatPtr(0)},
			&core.TextField{Name: "codepage", Max: 30},
			&core.BoolField{Name: "cut"},
			&core.BoolField{Name: "drawer"},
			&core.BoolField{Name: "qr_native"},
			&core.BoolField{Name: "enabled"},
			&core.BoolField{Name: "default"},
			&core.NumberField{Name: "timeout_ms", OnlyInt: true, Min: floatPtr(0)},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_printers_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCachedCollectionByNameOrId(TemplatesCollection); err != nil {
		c := core.NewBaseCollection(TemplatesCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 100},
			&core.NumberField{Name: "version", OnlyInt: true, Min: floatPtr(0)},
			&core.TextField{Name: "body", Required: true, Max: escpos.DefaultLimits.MaxTemplate},
			&core.TextField{Name: "printer", Max: 100},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_print_templates_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCachedCollectionByNameOrId(JobsCollection); err != nil {
		c := core.NewBaseCollection(JobsCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "printer", Required: true, Max: 100},
			&core.TextField{Name: "template", Max: 100},
			&core.NumberField{Name: "template_version", OnlyInt: true},
			&core.TextField{Name: "payload", Max: 4 << 20}, // base64 of the ESC/POS bytes
			&core.SelectField{Name: "state", Required: true, MaxSelect: 1, Values: jobStates},
			&core.NumberField{Name: "attempts", OnlyInt: true},
			&core.NumberField{Name: "waits", OnlyInt: true},
			&core.TextField{Name: "last_error", Max: 2000},
			&core.TextField{Name: "job_id", Max: 100},
			&core.TextField{Name: "idempotency_key", Max: 200},
			&core.TextField{Name: "actor", Max: 200},
			&core.NumberField{Name: "copies", OnlyInt: true},
			&core.DateField{Name: "printed_at"},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_print_jobs_idem", true, "idempotency_key", "idempotency_key != ''")
		c.AddIndex("idx_toki_print_jobs_state", false, "state, updated", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}

// Printer is a row of _printers.
type Printer struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Address   string `json:"address,omitempty"`
	Baud      int    `json:"baud,omitempty"`
	Cols      int    `json:"cols"`
	Codepage  string `json:"codepage"`
	Cut       bool   `json:"cut"`
	Drawer    bool   `json:"drawer"`
	QRNative  bool   `json:"qr_native"`
	Enabled   bool   `json:"enabled"`
	Default   bool   `json:"default"`
	TimeoutMs int    `json:"timeout_ms"`
}

func printerOf(r *core.Record) *Printer {
	p := &Printer{
		ID: r.Id, Name: r.GetString("name"), Transport: r.GetString("transport"), Address: r.GetString("address"),
		Baud: r.GetInt("baud"), Cols: r.GetInt("cols"), Codepage: r.GetString("codepage"),
		Cut: r.GetBool("cut"), Drawer: r.GetBool("drawer"), QRNative: r.GetBool("qr_native"),
		Enabled: r.GetBool("enabled"), Default: r.GetBool("default"), TimeoutMs: r.GetInt("timeout_ms"),
	}
	if p.Cols <= 0 {
		p.Cols = 42
	}
	if p.Transport == "serial" && p.Baud <= 0 {
		p.Baud = 9600
	}
	if p.TimeoutMs <= 0 {
		p.TimeoutMs = 5000
	}
	p.TimeoutMs = max(200, min(p.TimeoutMs, 30000))
	return p
}

func (p *Printer) timeout() time.Duration { return time.Duration(p.TimeoutMs) * time.Millisecond }

func (p *Printer) codepage() escpos.Codepage {
	cp, _ := escpos.ParseCodepage(p.Codepage)
	return cp
}

// target is the string the devio dialer understands.
func (p *Printer) target() string {
	if p.Transport == "file" {
		return "file://" + p.Address
	}
	return "tcp://" + p.Address
}

// validatePrinter checks a printer config on save.
func validatePrinter(p *Printer) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("printer: name is required")
	}
	if p.Cols != 0 && (p.Cols < 16 || p.Cols > 96) {
		return errors.New("printer: cols must be between 16 and 96 (32, 42 or 48 are typical)")
	}
	if p.Codepage != "" {
		if _, ok := escpos.ParseCodepage(p.Codepage); !ok {
			return fmt.Errorf("printer: unknown codepage %q (cp437, cp858, wpc1252)", p.Codepage)
		}
	}
	pol, err := policy(0)
	if err != nil {
		return fmt.Errorf("printer: TOKI_PRINT_ALLOW_CIDRS: %w", err)
	}
	switch p.Transport {
	case "tcp":
		host, port, err := net.SplitHostPort(p.Address)
		if err != nil || host == "" {
			return fmt.Errorf("printer: tcp address must be host:port, got %q", p.Address)
		}
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("printer: bad tcp port %q", port)
		}
		if a, err := netip.ParseAddr(host); err == nil {
			if err := pol.CheckIP(a); err != nil {
				return fmt.Errorf("printer: %w", err)
			}
		}
	case "serial", "file":
		if !strings.HasPrefix(p.Address, "/") {
			return fmt.Errorf("printer: %s address must be an absolute device path", p.Transport)
		}
		if err := pol.CheckFile(p.Address); err != nil {
			return fmt.Errorf("printer: %w", err)
		}
		if p.Transport == "serial" && p.Baud != 0 && !devio.ValidBaud(p.Baud) {
			return fmt.Errorf("printer: unsupported baud %d", p.Baud)
		}
	default:
		return fmt.Errorf("printer: transport must be tcp, serial or file, got %q", p.Transport)
	}
	return nil
}

// Template is a row of _print_templates.
type Template struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version int    `json:"version"`
	Body    string `json:"body"`
	Printer string `json:"printer"`
}

func templateOf(r *core.Record) *Template {
	return &Template{ID: r.Id, Name: r.GetString("name"), Version: r.GetInt("version"), Body: r.GetString("body"), Printer: r.GetString("printer")}
}

// Job is the public view of a _print_jobs row (the payload is not included).
type Job struct {
	ID              string `json:"id"`
	Printer         string `json:"printer"`
	Template        string `json:"template,omitempty"`
	TemplateVersion int    `json:"template_version,omitempty"`
	State           string `json:"state"`
	Attempts        int    `json:"attempts"`
	LastError       string `json:"last_error,omitempty"`
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	Actor           string `json:"actor,omitempty"`
	Copies          int    `json:"copies"`
	PrintedAt       string `json:"printed_at,omitempty"`
	Created         string `json:"created"`
}

func jobOf(r *core.Record) *Job {
	return &Job{
		ID: r.Id, Printer: r.GetString("printer"), Template: r.GetString("template"),
		TemplateVersion: r.GetInt("template_version"), State: r.GetString("state"),
		Attempts: r.GetInt("attempts"), LastError: r.GetString("last_error"),
		IdempotencyKey: r.GetString("idempotency_key"), Actor: r.GetString("actor"),
		Copies: max(1, r.GetInt("copies")), PrintedAt: r.GetString("printed_at"),
		Created: r.GetString("created"),
	}
}

func findPrinter(app core.App, name string) (*Printer, error) {
	r, err := app.FindFirstRecordByData(PrintersCollection, "name", name)
	if err != nil {
		return nil, fmt.Errorf("%w: printer %q", errNotFound, name)
	}
	return printerOf(r), nil
}

// ListPrinters returns every configured printer ordered by name.
func ListPrinters(app core.App) ([]*Printer, error) {
	if err := ensureCollections(app); err != nil {
		return nil, err
	}
	recs, err := app.FindRecordsByFilter(PrintersCollection, "", "name", 0, 0)
	if err != nil {
		return nil, err
	}
	out := make([]*Printer, 0, len(recs))
	for _, r := range recs {
		out = append(out, printerOf(r))
	}
	return out, nil
}

func findTemplate(app core.App, name string) (*Template, error) {
	r, err := app.FindFirstRecordByData(TemplatesCollection, "name", name)
	if err != nil {
		return nil, fmt.Errorf("%w: template %q", errNotFound, name)
	}
	return templateOf(r), nil
}

// ListJobs returns jobs newest first, optionally filtered by state.
func ListJobs(app core.App, state string, limit int) ([]*Job, error) {
	if err := ensureCollections(app); err != nil {
		return nil, err
	}
	filter, params := "", dbx.Params{}
	if state != "" {
		filter, params = "state = {:s}", dbx.Params{"s": state}
	}
	if limit <= 0 {
		limit = 50
	}
	recs, err := app.FindRecordsByFilter(JobsCollection, filter, "-created", limit, 0, params)
	if err != nil {
		return nil, err
	}
	out := make([]*Job, 0, len(recs))
	for _, r := range recs {
		out = append(out, jobOf(r))
	}
	return out, nil
}

// queueCounts returns the number of jobs per state.
func queueCounts(app core.App) map[string]int64 {
	out := map[string]int64{}
	if !app.HasTable(JobsCollection) {
		return out
	}
	var rows []struct {
		State string `db:"state"`
		N     int64  `db:"n"`
	}
	if err := app.DB().NewQuery(`SELECT [[state]] AS state, COUNT(*) AS n FROM {{_print_jobs}} GROUP BY [[state]]`).All(&rows); err != nil {
		return out
	}
	for _, r := range rows {
		out[r.State] = r.N
	}
	return out
}

// Prune deletes done jobs last updated before now-retention. It returns the
// number of deleted rows.
func Prune(app core.App, now time.Time, retention time.Duration) (int64, error) {
	if !app.HasTable(JobsCollection) {
		return 0, nil
	}
	res, err := app.DB().NewQuery(`DELETE FROM {{_print_jobs}} WHERE [[state]]='done' AND [[updated]] < {:t}`).
		Bind(dbx.Params{"t": now.Add(-retention).UTC().Format(timeLayout)}).Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// queueExpr matches the jobs of printer that are not finished.
func queueExpr(printer string) dbx.Expression {
	return dbx.NewExp("[[printer]] = {:p} AND [[state]] IN ('queued','printing','waiting_paper','failed')", dbx.Params{"p": printer})
}
