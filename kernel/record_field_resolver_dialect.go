package kernel

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/kernel/rule"
	rulesqlite "github.com/tokibase/tokibase/kernel/rule/sqlite"
	"github.com/tokibase/tokibase/tools/search"
)

// Dialect returns the SQL dialect the resolver emits (SQLite by default).
//
// Implements `search.DialectResolver`.
func (r *RecordFieldResolver) Dialect() rule.Dialect {
	if r.dialect == nil {
		return rulesqlite.Dialect
	}

	return r.dialect
}

// SetDialect changes the SQL dialect of the fragments the resolver emits
// (nil restores SQLite). It must match the dialect of the emitter the
// resolver is used with, see docs/RULE_ENGINE.md.
func (r *RecordFieldResolver) SetDialect(d rule.Dialect) {
	r.dialect = d
}

// joinOn returns the ON condition of a registered join; joins without
// a condition get the dialect's replacement (PostgreSQL requires an ON clause).
func (r *RecordFieldResolver) joinOn(j *search.Join) dbx.Expression {
	if j.On == nil && r.Dialect().OptionalOn() != "" {
		return dbx.NewExp("TRUE")
	}

	return j.On
}
