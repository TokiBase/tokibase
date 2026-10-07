package computed

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

const batchSize = 500

// Report is the result of a verify or backfill run for one definition.
type Report struct {
	Collection string   `json:"collection"`
	Field      string   `json:"field"`
	Parents    int      `json:"parents"`
	Drift      int      `json:"drift"`
	Fixed      int      `json:"fixed"`
	Sample     []string `json:"sample,omitempty"` // first drifting parent ids
}

const sampleMax = 20

// Progress is called after every batch.
type Progress func(done int, r *Report)

// scan walks all parents of d in id order, batch by batch. fix recomputes the
// drifting parents (serialized and authoritative); without it nothing is written.
func (m *Module) scan(d *Def, fix bool, progress Progress) (*Report, error) {
	rep := &Report{Collection: d.Collection, Field: d.Field}
	parent, err := m.app.FindCollectionByNameOrId(d.Collection)
	if err != nil {
		return nil, err
	}
	last := ""
	for {
		recs, err := m.app.FindRecordsByFilter(parent, "id > {:last}", "id", batchSize, 0, dbx.Params{"last": last})
		if err != nil {
			return rep, err
		}
		if len(recs) == 0 {
			return rep, nil
		}
		ids := make([]string, len(recs))
		for i, r := range recs {
			ids[i] = r.Id
		}
		vals, err := aggregate(m.app, d, ids)
		if err != nil {
			return rep, err
		}
		for _, r := range recs {
			rep.Parents++
			want := normalize(parent, d, vals[r.Id])
			if !differs(r.GetFloat(d.Field), want) {
				continue
			}
			rep.Drift++
			if len(rep.Sample) < sampleMax {
				rep.Sample = append(rep.Sample, r.Id)
			}
			if fix {
				if _, err := m.Recompute(d, r.Id); err != nil {
					return rep, err
				}
				rep.Fixed++
			}
		}
		last = recs[len(recs)-1].Id
		if progress != nil {
			progress(rep.Parents, rep)
		}
	}
}

func (m *Module) pick(collection, field string) ([]Def, error) {
	defs, err := find(m.app, collection, field)
	if err != nil {
		return nil, err
	}
	return defs, nil
}

// Backfill recomputes every parent of collection (and field when not empty)
// and writes the ones that drifted.
func (m *Module) Backfill(collection, field string, progress Progress) ([]*Report, error) {
	defs, err := m.pick(collection, field)
	if err != nil {
		return nil, err
	}
	var out []*Report
	for i := range defs {
		d := &defs[i]
		rep, err := m.scan(d, true, progress)
		if rep != nil {
			out = append(out, rep)
			audit(ActionBackfill, d.Collection, d.Field, map[string]any{
				"parents": rep.Parents, "drift": rep.Drift, "fixed": rep.Fixed, "error": errStr(err),
			})
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// Verify reports drift (stored != recomputed) without writing anything.
// Parents changed by concurrent writes while it runs can show up as transient drift.
func (m *Module) Verify(collection, field string) ([]*Report, error) {
	defs, err := m.pick(collection, field)
	if err != nil {
		return nil, err
	}
	var out []*Report
	for i := range defs {
		rep, err := m.scan(&defs[i], false, nil)
		if rep != nil {
			out = append(out, rep)
		}
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// DriftAll verifies every definition, logs and audits drift. Returns the total drift.
func (m *Module) DriftAll() (int, error) {
	defs, err := List(m.app, "")
	if err != nil {
		return 0, err
	}
	total := 0
	var firstErr error
	for i := range defs {
		d := &defs[i]
		rep, err := m.scan(d, false, nil)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if rep != nil && rep.Drift > 0 {
			total += rep.Drift
			m.app.Logger().Warn("computed: drift found", "field", d.key(), "drift", rep.Drift, "parents", rep.Parents)
			audit(ActionDrift, d.Collection, d.Field, map[string]any{
				"drift": rep.Drift, "parents": rep.Parents, "sample": rep.Sample,
			})
		}
	}
	return total, firstErr
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type backfillPayload struct {
	Collection string `json:"collection"`
	Field      string `json:"field,omitempty"`
}

func (m *Module) registerJobs() {
	q := kernel.Jobs(m.app)
	q.Register(JobBackfill, func(ctx context.Context, _ kernel.App, job *kernel.Job) error {
		var p backfillPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return err
		}
		_, err := m.Backfill(p.Collection, p.Field, nil)
		return err
	})
	q.Register(JobDrift, func(ctx context.Context, _ kernel.App, _ *kernel.Job) error {
		_, err := m.DriftAll()
		return err
	})

	expr := strings.TrimSpace(os.Getenv(EnvDriftCron))
	if expr == "" {
		expr = "23 3 * * *"
	}
	if strings.EqualFold(expr, "off") {
		return
	}
	err := m.app.Cron().Add("__tokiComputedDrift__", expr, func() {
		slot := time.Now().UTC().Format("20060102")
		_, err := kernel.Jobs(m.app).Enqueue(context.Background(), JobDrift, nil, kernel.Unique("computed.drift:"+slot))
		if errors.Is(err, kernel.ErrNoJobQueue) {
			_, err = m.DriftAll()
		}
		if err != nil {
			m.app.Logger().Error("computed: drift check failed", "error", err)
		}
	})
	if err != nil {
		m.app.Logger().Warn("computed: invalid drift cron", "expr", expr, "error", err)
	}
}

// EnqueueBackfill queues a backfill job; kernel.ErrNoJobQueue when there is no queue.
func EnqueueBackfill(app core.App, collection, field string) (string, error) {
	if _, err := find(app, collection, field); err != nil {
		return "", err
	}
	return kernel.Jobs(app).Enqueue(context.Background(), JobBackfill, backfillPayload{collection, field})
}
