//go:build !no_timelint

// Package timelint detects date values submitted through the API without a
// time zone (a classic source of "+N hours per sync" bugs) and offers a scan
// of stored values.
package timelint

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	validation "github.com/pocketbase/ozzo-validation/v4"
	"github.com/tokibase/tokibase/apis"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/hook"
	"github.com/tokibase/tokibase/tools/router"
	"github.com/tokibase/tokibase/tools/types"
)

const (
	serveHookId  = "__tokiTimelintServe__"
	middlewareId = "__tokiTimelintSnapshot__"
	rawKey       = "__tokiTimelintRaw__"
	createHookId = "__tokiTimelintCreate__"
	updateHookId = "__tokiTimelintUpdate__"

	// ErrCode is the validation error code returned in strict mode.
	ErrCode = "validation_invalid_timezone"

	warnEvery = time.Hour
)

// Policy is the module policy (env TOKI_TIMELINT).
type Policy string

const (
	PolicyOff    Policy = "off"
	PolicyWarn   Policy = "warn"
	PolicyStrict Policy = "strict"
)

// PolicyFromEnv parses TOKI_TIMELINT. Unknown values fall back to warn.
func PolicyFromEnv() Policy {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TOKI_TIMELINT"))) {
	case "strict":
		return PolicyStrict
	case "off", "false", "0", "disabled":
		return PolicyOff
	}
	return PolicyWarn
}

var (
	dateOnly = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	zoneTail = regexp.MustCompile(`(?i)(z|utc|gmt|[+-]\d{2}(:?\d{2})?)$`)
)

// Zoneless reports whether s is a date-time string that the app parses
// successfully today but that carries no zone designator. Date-only values
// (YYYY-MM-DD), empty strings and unparsable values are not zoneless.
func Zoneless(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || dateOnly.MatchString(s) {
		return false
	}
	i := strings.Index(s, ":")
	if i < 0 {
		return false // no time component
	}
	if zoneTail.MatchString(s[i:]) {
		return false
	}
	d, err := types.ParseDateTime(s)
	return err == nil && !d.IsZero()
}

var recordsPath = regexp.MustCompile(`^/api/collections/[^/]+/records(/[^/]+)?$`)

// snapshot stores the raw submitted strings of the collection date fields in
// the request store. The record create/update hooks run after the body was
// already normalized into types.DateTime (and the zone information is gone),
// so the raw value has to be captured before the upstream handler runs.
// Only records create/update requests are covered (not /api/batch).
func snapshot(e *core.RequestEvent) error {
	if (e.Request.Method != http.MethodPost && e.Request.Method != http.MethodPatch) ||
		!recordsPath.MatchString(e.Request.URL.Path) {
		return e.Next()
	}
	collection, err := e.App.FindCachedCollectionByNameOrId(e.Request.PathValue("collection"))
	if err != nil || collection == nil || collection.IsView() {
		return e.Next()
	}
	var names []string
	for _, f := range collection.Fields {
		if f.Type() == kernel.FieldTypeDate {
			names = append(names, f.GetName())
		}
	}
	if len(names) == 0 {
		return e.Next()
	}
	info, err := e.RequestInfo()
	if err != nil {
		// leave the error for the upstream handler (rewind the body so it parses it again)
		if body, ok := e.Request.Body.(router.Rereader); ok {
			body.Reread()
		}
		return e.Next()
	}
	raw := map[string]string{}
	for _, n := range names {
		if v, ok := info.Body[n].(string); ok {
			raw[n] = v
		}
	}
	e.Set(rawKey, raw)
	return e.Next()
}

type warnKey struct{ collection, field string }

type module struct {
	policy Policy
	now    func() time.Time

	mu     sync.Mutex
	warned map[warnKey]time.Time
}

// Register binds the request checks according to TOKI_TIMELINT.
func Register(app core.App) {
	policy := PolicyFromEnv()
	if policy == PolicyOff {
		return
	}
	m := &module{policy: policy, now: time.Now, warned: map[warnKey]time.Time{}}
	app.OnServe().Bind(&hook.Handler[*core.ServeEvent]{
		Id: serveHookId,
		Func: func(e *core.ServeEvent) error {
			e.Router.Bind(&hook.Handler[*core.RequestEvent]{
				Id:       middlewareId,
				Priority: apis.DefaultBodyLimitMiddlewarePriority + 1,
				Func:     snapshot,
			})
			return e.Next()
		},
	})
	app.OnRecordCreateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id:       createHookId,
		Priority: -1 << 20,
		Func:     func(e *core.RecordRequestEvent) error { return m.check(e, "Failed to create record.") },
	})
	app.OnRecordUpdateRequest().Bind(&hook.Handler[*core.RecordRequestEvent]{
		Id:       updateHookId,
		Priority: -1 << 20,
		Func:     func(e *core.RecordRequestEvent) error { return m.check(e, "Failed to update record.") },
	})
}

func (m *module) check(e *core.RecordRequestEvent, failMsg string) error {
	var dateFields []string
	for _, f := range e.Record.Collection().Fields {
		if f.Type() == kernel.FieldTypeDate {
			dateFields = append(dateFields, f.GetName())
		}
	}
	if len(dateFields) == 0 {
		return e.Next()
	}

	raw, _ := e.Get(rawKey).(map[string]string)

	var errs validation.Errors
	for _, name := range dateFields {
		sample, ok := raw[name]
		if !ok || !Zoneless(sample) {
			continue
		}
		if m.policy == PolicyStrict {
			if errs == nil {
				errs = validation.Errors{}
			}
			errs[name] = validation.NewError(ErrCode, "Must include a time zone (Z or +hh:mm) or be a date only (YYYY-MM-DD).")
			continue
		}
		m.warn(e.App, e.Record.Collection().Name, name, sample)
	}
	if len(errs) > 0 {
		return e.BadRequestError(failMsg, errs)
	}
	return e.Next()
}

// warn logs at most once per collection+field per hour.
func (m *module) warn(app kernel.App, collection, field, sample string) {
	k := warnKey{collection, field}
	now := m.now()
	m.mu.Lock()
	if last, ok := m.warned[k]; ok && now.Sub(last) < warnEvery {
		m.mu.Unlock()
		return
	}
	m.warned[k] = now
	m.mu.Unlock()
	app.Logger().Warn("timelint: date value submitted without a time zone (parsed as UTC)",
		"collection", collection, "field", field, "sample", sample)
}

// Finding is one row of the stored value scan.
type Finding struct {
	Collection string `json:"collection"`
	Field      string `json:"field"`
	Total      int    `json:"total"`
	Midnight   int    `json:"midnight"`
}

// Scan counts, per date field, the stored values and the ones whose
// time of day is exactly 00:00:00 (a hint at date-only inputs).
// View collections are skipped.
func Scan(app kernel.App) ([]Finding, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, c := range collections {
		if c.IsView() {
			continue
		}
		for _, f := range c.Fields {
			if f.Type() != kernel.FieldTypeDate {
				continue
			}
			var row struct {
				Total    int `db:"total"`
				Midnight int `db:"midnight"`
			}
			q := fmt.Sprintf("SELECT COUNT(*) AS total, COALESCE(SUM(CASE WHEN substr([[%[1]s]],12) LIKE '00:00:00%%' THEN 1 ELSE 0 END),0) AS midnight FROM {{%[2]s}} WHERE [[%[1]s]] != '' AND [[%[1]s]] IS NOT NULL", f.GetName(), c.Name)
			if err := app.DB().NewQuery(q).One(&row); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", c.Name, f.GetName(), err)
			}
			out = append(out, Finding{Collection: c.Name, Field: f.GetName(), Total: row.Total, Midnight: row.Midnight})
		}
	}
	return out, nil
}
