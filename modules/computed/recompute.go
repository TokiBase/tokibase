package computed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/search"
)

type ctxKey struct{}

// markCtx marks writes made by the module, so that its own parent saves never
// trigger another recompute (loop protection).
func markCtx() context.Context { return context.WithValue(context.Background(), ctxKey{}, true) }

func isMarked(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ctxKey{}).(bool)
	return v
}

// entry serializes recomputes of one (collection, field, parent) and
// coalesces bursts: a caller whose request was already covered by a recompute
// that started after it was made returns without querying again.
type entry struct {
	mu      sync.Mutex // held while recomputing
	smu     sync.Mutex // guards the counters
	req     uint64
	covered uint64
	refs    int // guarded by Module.locksMu
}

func (m *Module) acquire(key string) *entry {
	m.locksMu.Lock()
	defer m.locksMu.Unlock()
	e := m.locks[key]
	if e == nil {
		e = &entry{}
		m.locks[key] = e
	}
	e.refs++
	return e
}

func (m *Module) release(key string, e *entry) {
	m.locksMu.Lock()
	e.refs--
	if e.refs == 0 {
		delete(m.locks, key)
	}
	m.locksMu.Unlock()
}

// Recompute recomputes the field of one parent and saves it when it changed.
func (m *Module) Recompute(d *Def, parentId string) (bool, error) {
	key := d.key() + "#" + parentId
	e := m.acquire(key)
	defer m.release(key, e)

	e.smu.Lock()
	e.req++
	ticket := e.req
	e.smu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()

	e.smu.Lock()
	if e.covered >= ticket {
		e.smu.Unlock()
		return false, nil
	}
	snap := e.req // every request up to snap committed before this recompute reads
	e.smu.Unlock()

	changed, err := m.apply(d, parentId)
	if err == nil {
		e.smu.Lock()
		e.covered = snap
		e.smu.Unlock()
	}
	return changed, err
}

// apply reads the aggregate and writes the parent in one transaction, so a
// concurrent client edit of the parent cannot be overwritten with stale data.
func (m *Module) apply(d *Def, parentId string) (changed bool, err error) {
	err = m.app.RunInTransaction(func(tx kernel.App) error {
		rec, err := tx.FindRecordById(d.Collection, parentId)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // parent gone (cascade delete)
			}
			return err
		}
		vals, err := aggregate(tx, d, []string{parentId})
		if err != nil {
			return err
		}
		want := normalize(rec.Collection(), d, vals[parentId])
		if !differs(rec.GetFloat(d.Field), want) {
			return nil
		}
		rec.Set(d.Field, want)
		if err := tx.SaveNoValidateWithContext(markCtx(), rec); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if changed && err == nil {
		m.parentWrites.add()
	}
	return changed, err
}

func normalize(parent *core.Collection, d *Def, v float64) float64 {
	if f, ok := parent.Fields.GetByName(d.Field).(*core.NumberField); ok && f.OnlyInt {
		return math.Round(v)
	}
	return v
}

// buildQuery returns the grouped aggregate query on the child. ids may be nil
// (validation only).
func buildQuery(app kernel.App, child *core.Collection, d *Def, ids []string) (*dbx.SelectQuery, error) {
	db := app.ConcurrentDB()
	tbl := db.QuoteSimpleTableName(child.Name)
	rel := tbl + "." + db.QuoteSimpleColumnName(d.SourceRelation)
	var val string
	if d.SourceField != "" {
		val = tbl + "." + db.QuoteSimpleColumnName(d.SourceField)
	}
	var agg string
	switch d.Kind {
	case KindCount:
		agg = "COUNT(DISTINCT " + tbl + ".`id`)"
	case KindSum:
		agg = "COALESCE(SUM(" + val + "), 0)"
	case KindAvg:
		agg = "COALESCE(AVG(" + val + "), 0)"
	case KindMin:
		agg = "COALESCE(MIN(" + val + "), 0)"
	case KindMax:
		agg = "COALESCE(MAX(" + val + "), 0)"
	case KindLast:
		agg = "COALESCE(" + val + ", 0)"
	default:
		return nil, fmt.Errorf("unknown kind %q", d.Kind)
	}
	q := db.Select(rel+" AS pid", agg+" AS v").From(child.Name)
	if d.Kind == KindLast {
		q.OrderBy(tbl+".`created` ASC", tbl+".`id` ASC")
	} else {
		q.GroupBy(rel)
	}
	if len(ids) > 0 {
		params := dbx.Params{}
		ph := make([]string, len(ids))
		for i, id := range ids {
			k := "p" + strconv.Itoa(i)
			ph[i] = "{:" + k + "}"
			params[k] = id
		}
		q.AndWhere(dbx.NewExp(rel+" IN ("+strings.Join(ph, ",")+")", params))
	}
	resolver := core.NewRecordFieldResolver(app, child, nil, true)
	if strings.TrimSpace(d.Filter) != "" {
		expr, err := search.FilterData(d.Filter).BuildExpr(resolver)
		if err != nil {
			return nil, err
		}
		q.AndWhere(expr)
	}
	if err := resolver.UpdateQuery(q); err != nil {
		return nil, err
	}
	return q, nil
}

type aggRow struct {
	Pid string  `db:"pid"`
	V   float64 `db:"v"`
}

// aggregate returns the value per parent id; parents without matching
// children are absent (the value is 0).
func aggregate(app kernel.App, d *Def, ids []string) (map[string]float64, error) {
	child, err := app.FindCollectionByNameOrId(d.SourceCollection)
	if err != nil {
		return nil, err
	}
	q, err := buildQuery(app, child, d, ids)
	if err != nil {
		return nil, err
	}
	var rows []aggRow
	if err := q.All(&rows); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Pid] = r.V // for kind last the rows are ordered, the last one wins
	}
	return out, nil
}
