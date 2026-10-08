//go:build !no_printer

package printer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/internal/escpos"
	"github.com/tokibase/tokibase/kernel"
)

// ErrSyncReplica is returned when a print would be created by a sync apply.
var ErrSyncReplica = errors.New("printer: print jobs are never created from sync applies")

// RequestError is an error caused by the request; Status is the HTTP status.
type RequestError struct {
	Status int
	Msg    string
}

func (e *RequestError) Error() string { return e.Msg }

func badReq(format string, a ...any) error { return &RequestError{400, fmt.Sprintf(format, a...)} }

// Request asks for one print.
type Request struct {
	Printer        string
	Template       string
	Data           any
	Copies         int
	IdempotencyKey string
	Raw            []byte // superuser callers only: sent unchanged
	Actor          string
}

// Result is the answer to [Module.Enqueue].
type Result struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

func (m *Module) resolvePrinter(name string) (*Printer, error) {
	if name != "" {
		p, err := findPrinter(m.app, name)
		if err != nil {
			return nil, &RequestError{404, err.Error()}
		}
		if !p.Enabled {
			return nil, badReq("printer %q is disabled", name)
		}
		return p, nil
	}
	all, err := ListPrinters(m.app)
	if err != nil {
		return nil, err
	}
	var enabled []*Printer
	for _, p := range all {
		if !p.Enabled {
			continue
		}
		if p.Default {
			return p, nil
		}
		enabled = append(enabled, p)
	}
	if len(enabled) == 1 {
		return enabled[0], nil
	}
	return nil, badReq("printer is required (no default printer is configured)")
}

// findByKey finds the job of actor with the idempotency key: keys are scoped
// per actor, so one user can neither suppress nor discover another one's job.
func (m *Module) findByKey(actor, key string) *core.Record {
	r, err := m.app.FindFirstRecordByFilter(JobsCollection, "actor = {:a} && idempotency_key = {:k}",
		dbx.Params{"a": actor, "k": key})
	if err != nil {
		return nil
	}
	return r
}

// uniqueKey is the kernel job key of an actor scoped idempotency key.
func uniqueKey(actor, key string) string {
	h := sha256.Sum256([]byte(actor + "\x00" + key))
	return "print:k:" + hex.EncodeToString(h[:16])
}

// overQuota reports whether actor has too many unfinished jobs. Superusers and
// non collection actors (the CLI) are exempt.
func (m *Module) overQuota(actor string) bool {
	limit := maxQueuedPerActor()
	if limit <= 0 || !strings.Contains(actor, "/") || strings.HasPrefix(actor, core.CollectionNameSuperusers+"/") {
		return false
	}
	n, err := m.app.CountRecords(JobsCollection, dbx.NewExp(
		"[[actor]] = {:a} AND [[state]] IN ('queued','printing','waiting_paper','failed')", dbx.Params{"a": actor}))
	return err == nil && int(n) >= limit
}

// Enqueue renders the print, stores the job and queues the transmission.
// It never runs for sync applies: ctx carrying a replica origin is refused.
func (m *Module) Enqueue(ctx context.Context, rq Request) (*Result, error) {
	if kernel.IsSyncReplica(ctx) {
		return nil, ErrSyncReplica
	}
	if err := ensureCollections(m.app); err != nil {
		return nil, err
	}
	copies := rq.Copies
	if copies == 0 {
		copies = 1
	}
	if copies < 1 || copies > maxCopies {
		return nil, badReq("copies must be between 1 and %d", maxCopies)
	}
	if len(rq.IdempotencyKey) > 200 {
		return nil, badReq("idempotency_key is too long")
	}
	if rq.IdempotencyKey != "" {
		if r := m.findByKey(rq.Actor, rq.IdempotencyKey); r != nil {
			return &Result{ID: r.Id, State: r.GetString("state"), Duplicate: true}, nil
		}
	}
	if m.overQuota(rq.Actor) {
		return nil, &RequestError{429, fmt.Sprintf("too many unfinished prints (limit %d per client)", maxQueuedPerActor())}
	}

	var tpl *Template
	if len(rq.Raw) == 0 {
		if rq.Template == "" {
			return nil, badReq("template is required")
		}
		t, err := findTemplate(m.app, rq.Template)
		if err != nil {
			return nil, &RequestError{404, err.Error()}
		}
		tpl = t
	}
	pname := rq.Printer
	if pname == "" && tpl != nil {
		pname = tpl.Printer
	}
	prn, err := m.resolvePrinter(pname)
	if err != nil {
		return nil, err
	}

	max := MaxBytes()
	var payload []byte
	if tpl == nil {
		if len(rq.Raw) > max {
			return nil, badReq("raw payload is %d bytes, the limit is %d", len(rq.Raw), max)
		}
		payload = rq.Raw
	} else {
		lim := escpos.DefaultLimits
		lim.MaxBytes = max
		lim.MaxOutput = max * 2
		var info escpos.Info
		payload, info, err = escpos.RenderInfo(tpl.Body, withCols(rq.Data, prn.Cols), escpos.Options{
			Codepage: prn.codepage(), QRRaster: !prn.QRNative, Location: time.Local, Limits: lim,
		})
		if err != nil {
			// the detail can quote the template: keep it in the log
			m.app.Logger().Warn("printer: render failed", "template", tpl.Name, "actor", rq.Actor, "error", err)
			if errors.Is(err, escpos.ErrLimit) {
				return nil, badReq("the print data is too large for template %q", tpl.Name)
			}
			return nil, badReq("template %q could not be rendered with this data", tpl.Name)
		}
		payload = finish(prn, payload, info)
		if len(payload) > max {
			return nil, badReq("rendered payload is %d bytes, the limit is %d", len(payload), max)
		}
	}

	col, err := m.app.FindCachedCollectionByNameOrId(JobsCollection)
	if err != nil {
		return nil, err
	}
	rec := core.NewRecord(col)
	rec.Set("printer", prn.Name)
	if tpl != nil {
		rec.Set("template", tpl.Name)
		rec.Set("template_version", tpl.Version)
	}
	rec.Set("payload", base64.StdEncoding.EncodeToString(payload))
	rec.Set("state", StateQueued)
	rec.Set("copies", copies)
	rec.Set("idempotency_key", rq.IdempotencyKey)
	rec.Set("actor", rq.Actor)
	if err := m.app.SaveWithContext(ctx, rec); err != nil {
		if rq.IdempotencyKey != "" { // lost the race for the key
			if r := m.findByKey(rq.Actor, rq.IdempotencyKey); r != nil {
				return &Result{ID: r.Id, State: r.GetString("state"), Duplicate: true}, nil
			}
		}
		return nil, err
	}

	uniq := "print:" + rec.Id
	if rq.IdempotencyKey != "" {
		uniq = uniqueKey(rq.Actor, rq.IdempotencyKey)
	}
	jobID, err := kernel.Jobs(m.app).Enqueue(ctx, JobKind, map[string]string{"id": rec.Id},
		kernel.Unique(uniq), kernel.MaxAttempts(MaxAttempts))
	if err != nil {
		_ = m.app.Delete(rec)
		return nil, fmt.Errorf("printer: queue the job: %w", err)
	}
	rec.Set("job_id", jobID)
	if err := m.app.Save(rec); err != nil {
		m.app.Logger().Warn("printer: failed to store the job id", "job", rec.Id, "error", err)
	}
	audit(AuditJob, rec.Id, map[string]any{
		"printer": prn.Name, "template": rq.Template, "actor": rq.Actor, "copies": copies, "bytes": len(payload),
	})
	return &Result{ID: rec.Id, State: StateQueued}, nil
}

// withCols exposes the paper width to templates as `_cols` (maps only).
func withCols(data any, cols int) any {
	switch d := data.(type) {
	case nil:
		return map[string]any{"_cols": cols}
	case map[string]any:
		out := make(map[string]any, len(d)+1)
		for k, v := range d {
			out[k] = v
		}
		out["_cols"] = cols // never caller controlled
		return out
	}
	return data
}

// finish applies the printer defaults: drawer kick and cut when the template
// did not do it itself.
func finish(prn *Printer, payload []byte, info escpos.Info) []byte {
	b := escpos.New(prn.codepage())
	if prn.Drawer && !info.HasDrawer {
		b.Drawer()
	}
	if prn.Cut && !info.HasCut {
		b.Feed(3).Cut()
	}
	if b.Len() == 0 {
		return payload
	}
	return append(append([]byte{}, payload...), b.Bytes()...)
}

// Retry queues a failed, dead or unconfirmed job again (an explicit reprint).
func (m *Module) Retry(ctx context.Context, id string) (*Result, error) {
	if kernel.IsSyncReplica(ctx) {
		return nil, ErrSyncReplica
	}
	var rec *core.Record
	err := m.app.RunInTransaction(func(tx kernel.App) error {
		r, err := tx.FindRecordById(JobsCollection, id)
		if err != nil {
			return &RequestError{404, "print job not found"}
		}
		st := r.GetString("state")
		if st != StateFailed && st != StateDead && st != StateUnconfirmed {
			return &RequestError{409, fmt.Sprintf("only failed, dead or unconfirmed jobs can be retried, this one is %s", st)}
		}
		r.Set("state", StateQueued)
		r.Set("waits", 0)
		r.Set("last_error", "")
		rec = r
		return tx.Save(r)
	})
	if err != nil {
		return nil, err
	}
	// a new unique key: the old job may still wait for its backoff
	key := fmt.Sprintf("print:%s:r%d", rec.Id, time.Now().UnixNano())
	jobID, err := kernel.Jobs(m.app).Enqueue(ctx, JobKind, map[string]string{"id": rec.Id},
		kernel.Unique(key), kernel.MaxAttempts(MaxAttempts))
	if err != nil {
		return nil, fmt.Errorf("printer: queue the job: %w", err)
	}
	rec.Set("job_id", jobID)
	_ = m.app.Save(rec)
	return &Result{ID: rec.Id, State: StateQueued}, nil
}
