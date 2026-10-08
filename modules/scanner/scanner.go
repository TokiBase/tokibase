//go:build !no_scanner

// Package scanner turns barcode and QR reads from serial ports, Linux evdev
// keyboards and browsers into de-duplicated scan events delivered over the
// realtime topic "@scan" and REST (see docs/modules/scanner.md).
package scanner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/devio"
	"github.com/tokibase/tokibase/tools/hook"
)

const (
	hookId = "__tokiScanner__"

	// ConfigCollection is the superuser-only scanner config collection.
	ConfigCollection = "_scanners"
	// EventsCollection holds the scan events. It is local to the node and never synced.
	EventsCollection = "_scan_events"
	// Topic is the realtime topic scan events are published on.
	Topic = "@scan"

	KindSerial = "serial"
	KindEvdev  = "evdev"
	KindWeb    = "web"

	defaultDedupeMs = 1500
	defaultMaxLen   = 512
	defaultBaud     = 9600
	defaultBaseName = "web"

	timeLayout = "2006-01-02 15:04:05.000Z"

	defaultRetentionHours = 168
	pruneCron             = "*/10 * * * *"
)

// Enabled reports whether the module is switched on (TOKI_SCANNER=on).
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_SCANNER"))) {
	case "on", "1", "true", "yes":
		return true
	}
	return false
}

func retention() time.Duration {
	h := defaultRetentionHours
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TOKI_SCAN_RETENTION_HOURS"))); err == nil && n > 0 {
		h = n
	}
	return time.Duration(h) * time.Hour
}

// topicSuperuserOnly reports TOKI_SCAN_TOPIC_AUTH=superuser; any other value
// (default "auth") lets every authenticated client read scans.
func topicSuperuserOnly() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TOKI_SCAN_TOPIC_AUTH")), "superuser")
}

// Scanner is a row of the _scanners collection with defaults applied.
type Scanner struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Device     string `json:"device"`
	Baud       int    `json:"baud"`
	Terminator string `json:"terminator"`
	Prefix     string `json:"prefix"`
	Suffix     string `json:"suffix"`
	MinLen     int    `json:"min_len"`
	MaxLen     int    `json:"max_len"`
	Charset    string `json:"charset"`
	DedupeMs   int    `json:"dedupe_ms"`
	Grab       bool   `json:"grab"`
	Enabled    bool   `json:"enabled"`
	Layout     string `json:"layout"`

	re *regexp.Regexp
}

func scannerOf(r *core.Record) *Scanner {
	s := &Scanner{
		ID: r.Id, Name: r.GetString("name"), Kind: r.GetString("kind"), Device: r.GetString("device"),
		Baud: r.GetInt("baud"), Terminator: r.GetString("terminator"),
		Prefix: r.GetString("prefix"), Suffix: r.GetString("suffix"),
		MinLen: r.GetInt("min_len"), MaxLen: r.GetInt("max_len"), Charset: r.GetString("charset"),
		DedupeMs: r.GetInt("dedupe_ms"), Grab: r.GetBool("grab"), Enabled: r.GetBool("enabled"),
		Layout: r.GetString("layout"),
	}
	s.applyDefaults()
	return s
}

func (s *Scanner) applyDefaults() {
	if s.Baud == 0 {
		s.Baud = defaultBaud
	}
	if s.MinLen <= 0 {
		s.MinLen = 1
	}
	if s.MaxLen <= 0 {
		s.MaxLen = defaultMaxLen
	}
	if s.DedupeMs == 0 {
		s.DedupeMs = defaultDedupeMs // a negative value disables de-duplication
	}
	if s.Terminator == "" {
		s.Terminator = "auto"
	}
	if s.Layout == "" {
		s.Layout = "us"
	}
	if s.Charset != "" && s.re == nil {
		s.re, _ = regexp.Compile(s.Charset)
	}
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Validate checks a scanner config.
func (s *Scanner) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return errors.New("scanner: name must be 1-64 characters of A-Z a-z 0-9 _ . -")
	}
	switch s.Kind {
	case KindSerial, KindEvdev:
		if s.Device == "" {
			return fmt.Errorf("scanner: device is required for kind %s", s.Kind)
		}
		if filepath.Clean(s.Device) != s.Device || !strings.HasPrefix(s.Device, "/dev/") {
			return errors.New("scanner: device must be a clean path under /dev/")
		}
		if s.Kind == KindSerial && !devio.ValidBaud(s.Baud) {
			return fmt.Errorf("scanner: unsupported baud %d", s.Baud)
		}
	case KindWeb:
	default:
		return errors.New("scanner: kind must be serial, evdev or web")
	}
	switch s.Terminator {
	case "auto", "cr", "lf", "crlf":
	default:
		return errors.New("scanner: terminator must be auto, cr, lf or crlf")
	}
	if s.Layout != "us" {
		return errors.New("scanner: only layout us is supported")
	}
	if s.MaxLen < s.MinLen {
		return errors.New("scanner: max_len is below min_len")
	}
	if len(s.Charset) > 256 {
		return errors.New("scanner: charset is too long")
	}
	if s.Charset != "" {
		if _, err := regexp.Compile(s.Charset); err != nil {
			return fmt.Errorf("scanner: charset is not a valid regular expression: %w", err)
		}
	}
	return nil
}

// Clean strips the configured prefix and suffix and applies the length and
// charset filters. When the code is rejected the reason is returned.
func (s *Scanner) Clean(raw string) (code, reason string) {
	code = strings.Trim(raw, "\r\n\t ")
	if s.Prefix != "" {
		code = strings.TrimPrefix(code, s.Prefix)
	}
	if s.Suffix != "" {
		code = strings.TrimSuffix(code, s.Suffix)
	}
	if len(code) == 0 {
		return "", "empty"
	}
	if len(code) < s.MinLen {
		return "", "too_short"
	}
	if len(code) > s.MaxLen {
		return "", "too_long"
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 0x20 || code[i] == 0x7f {
			return "", "control_char"
		}
	}
	if s.Charset != "" {
		if s.re == nil {
			s.re, _ = regexp.Compile(s.Charset)
		}
		if s.re != nil && !s.re.MatchString(code) {
			return "", "charset"
		}
	}
	return code, ""
}

// defaultWeb is the scanner used by POST /api/scan when no web scanner is configured.
func defaultWeb() *Scanner {
	s := &Scanner{Name: defaultBaseName, Kind: KindWeb, Enabled: true}
	s.applyDefaults()
	return s
}

// symbologyOf guesses the symbology of a code. A scanner-reported hint wins;
// without one only unambiguous shapes are named.
func symbologyOf(code, hint string) string {
	switch h := strings.ToLower(strings.TrimSpace(hint)); h {
	case "qr", "code128", "ean13", "unknown":
		return h
	}
	digits := true
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			digits = false
			break
		}
	}
	switch {
	case digits && len(code) == 13:
		return "ean13"
	case strings.Contains(code, "://") || len(code) > 64:
		return "qr"
	}
	return "unknown"
}

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

var ensureMu sync.Mutex

func floatPtr(f float64) *float64 { return &f }

// ensureCollections creates _scanners and _scan_events when missing.
func ensureCollections(app core.App) error {
	ensureMu.Lock()
	defer ensureMu.Unlock()
	if !app.HasTable("_collections") {
		return errors.New("scanner: _collections table is not ready")
	}
	if _, err := app.FindCachedCollectionByNameOrId(ConfigCollection); err != nil {
		c := core.NewBaseCollection(ConfigCollection)
		c.System = true // rules stay nil: superuser only
		c.Fields.Add(
			&core.TextField{Name: "name", Required: true, Max: 64},
			&core.SelectField{Name: "kind", Required: true, MaxSelect: 1, Values: []string{KindSerial, KindEvdev, KindWeb}},
			&core.TextField{Name: "device", Max: 300},
			&core.NumberField{Name: "baud", OnlyInt: true, Min: floatPtr(0)},
			&core.TextField{Name: "terminator", Max: 10},
			&core.TextField{Name: "prefix", Max: 32},
			&core.TextField{Name: "suffix", Max: 32},
			&core.NumberField{Name: "min_len", OnlyInt: true, Min: floatPtr(0)},
			&core.NumberField{Name: "max_len", OnlyInt: true, Min: floatPtr(0)},
			&core.TextField{Name: "charset", Max: 256},
			&core.NumberField{Name: "dedupe_ms", OnlyInt: true},
			&core.BoolField{Name: "grab"},
			&core.BoolField{Name: "enabled"},
			&core.TextField{Name: "layout", Max: 10},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_scanners_name", true, "name", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	if _, err := app.FindCachedCollectionByNameOrId(EventsCollection); err != nil {
		c := core.NewBaseCollection(EventsCollection)
		c.System = true
		c.Fields.Add(
			&core.TextField{Name: "scanner", Required: true, Max: 64},
			&core.TextField{Name: "code", Required: true, Max: 1024},
			&core.TextField{Name: "symbology", Max: 16},
			&core.TextField{Name: "source", Max: 16},
			&core.TextField{Name: "actor", Max: 64},
			&core.NumberField{Name: "dup_count", OnlyInt: true},
			&core.AutodateField{Name: "created", OnCreate: true},
			&core.AutodateField{Name: "updated", OnCreate: true, OnUpdate: true},
		)
		c.AddIndex("idx_toki_scan_events_created", false, "created", "")
		if err := app.Save(c); err != nil {
			return err
		}
	}
	return nil
}

// loadAll returns every row of _scanners.
func loadAll(app core.App) ([]*Scanner, error) {
	if _, err := app.FindCachedCollectionByNameOrId(ConfigCollection); err != nil {
		return nil, nil
	}
	recs, err := app.FindAllRecords(ConfigCollection)
	if err != nil {
		return nil, err
	}
	out := make([]*Scanner, 0, len(recs))
	for _, r := range recs {
		out = append(out, scannerOf(r))
	}
	return out, nil
}

// Register binds the module to app: collections on bootstrap, hooks, routes,
// readers on serve and the retention cron.
func Register(app core.App) *Module {
	m := newModule(app)
	init := func() {
		if err := ensureCollections(app); err != nil {
			app.Logger().Error("scanner: failed to initialize the collections", "error", err)
		}
	}
	if app.IsBootstrapped() {
		init()
	}
	app.OnBootstrap().Bind(&hook.Handler[*core.BootstrapEvent]{
		Id: hookId, Priority: 1 << 20,
		Func: func(e *core.BootstrapEvent) error {
			if err := e.Next(); err != nil {
				return err
			}
			init()
			return nil
		},
	})
	app.OnRecordValidate(ConfigCollection).Bind(&hook.Handler[*core.RecordEvent]{
		Id: hookId + "val",
		Func: func(e *core.RecordEvent) error {
			if err := scannerOf(e.Record).Validate(); err != nil {
				return err
			}
			return e.Next()
		},
	})
	for _, h := range []func(...string) *hook.TaggedHook[*core.RecordEvent]{
		app.OnRecordAfterCreateSuccess, app.OnRecordAfterUpdateSuccess, app.OnRecordAfterDeleteSuccess,
	} {
		h(ConfigCollection).Bind(&hook.Handler[*core.RecordEvent]{
			Id: hookId + "cfg", Priority: 1 << 20,
			Func: func(e *core.RecordEvent) error {
				m.wakeSupervisor()
				return e.Next()
			},
		})
	}
	m.bindRealtime()
	m.bindRoutes()
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: hookId, Priority: 1 << 20,
		Func: func(e *core.ServeEvent) error {
			m.Start()
			return e.Next()
		},
	})
	app.OnTerminate().Bind(&hook.Handler[*core.TerminateEvent]{
		Id: hookId,
		Func: func(e *core.TerminateEvent) error {
			m.Stop()
			return e.Next()
		},
	})
	return m
}
