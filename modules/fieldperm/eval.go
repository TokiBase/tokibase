//go:build !no_fieldperm

package fieldperm

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
	"github.com/tokibase/tokibase/tools/inflector"
	"github.com/tokibase/tokibase/tools/search"
	"github.com/tokibase/tokibase/tools/security"
)

// evalExisting evaluates rule against a stored record exactly like a
// collection rule (kernel resolver: @request.*, @collection.*, relations).
// Unlike app.CanAccessRecord it has no superuser shortcut: the caller decides.
func evalExisting(app kernel.App, record *core.Record, info *core.RequestInfo, rule string) (bool, error) {
	if info == nil {
		return false, errNilInfo
	}
	if strings.TrimSpace(rule) == "" {
		return true, nil
	}
	col := record.Collection()
	query := app.RecordQuery(col).
		Select("(1)").
		AndWhere(dbx.HashExp{col.Name + ".id": record.Id})

	resolver := core.NewRecordFieldResolver(app, col, info, true)
	expr, err := search.FilterData(rule).BuildExpr(resolver)
	if err != nil {
		return false, err
	}
	if err := resolver.UpdateQuery(query); err != nil {
		return false, err
	}
	var exists int
	err = query.AndWhere(expr).Limit(1).Row(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return exists > 0, nil
}

// evalSubmitted evaluates rule against a record that is not stored yet
// (create), the same way the create rule is checked: the submitted data is
// exported into a one-row CTE named after the collection.
func evalSubmitted(app kernel.App, record *core.Record, info *core.RequestInfo, rule string) (bool, error) {
	if info == nil {
		return false, errNilInfo
	}
	if strings.TrimSpace(rule) == "" {
		return true, nil
	}
	collection := record.Collection()
	dummy := record.Clone()
	part := "__pb_create__" + security.PseudorandomString(6)
	if dummy.Id == "" {
		dummy.Id = "__temp_id__" + part
	}
	dummy.SetVerified(false)

	export, err := dummy.DBExport(app)
	if err != nil {
		return false, fmt.Errorf("dummy DBExport: %w", err)
	}
	params := make(dbx.Params, len(export))
	selects := make([]string, 0, len(export))
	for k, v := range export {
		k = inflector.Columnify(k)
		p := "__pb_create__" + k
		params[p] = v
		selects = append(selects, "{:"+p+"} AS [["+k+"]]")
	}
	dc := *collection
	dc.Id += part
	dc.Name += inflector.Columnify(part)

	withFrom := fmt.Sprintf("WITH {{%s}} as (SELECT %s)", dc.Name, strings.Join(selects, ","))
	q := app.ConcurrentDB().Select("(1)").PreFragment(withFrom).From(dc.Name).AndBind(params)

	resolver := core.NewRecordFieldResolver(app, &dc, info, true)
	expr, err := search.FilterData(rule).BuildExpr(resolver)
	if err != nil {
		return false, err
	}
	q.AndWhere(expr)
	if err := resolver.UpdateQuery(q); err != nil {
		return false, err
	}
	var exists int
	err = q.Limit(1).Row(&exists)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return exists > 0, nil
}
