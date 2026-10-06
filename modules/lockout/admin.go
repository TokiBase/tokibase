package lockout

import (
	"github.com/pocketbase/dbx"
	"github.com/tokibase/tokibase/core"
)

// Row is a persisted lockout record.
type Row struct {
	Key          string `db:"key" json:"key"`
	Failures     int    `db:"failures" json:"failures"`
	FirstFailure string `db:"first_failure" json:"firstFailure"`
	LockedUntil  string `db:"locked_until" json:"lockedUntil"`
	Updated      string `db:"updated" json:"updated"`
}

// List returns the persisted records, most recently updated first.
func List(app core.App) ([]Row, error) {
	if !app.AuxHasTable(TableName) {
		return nil, nil
	}
	var rows []Row
	err := app.AuxDB().NewQuery("SELECT [[key]], [[failures]], [[first_failure]], COALESCE([[locked_until]], '') AS [[locked_until]], [[updated]] FROM {{_lockout}} ORDER BY [[updated]] DESC").All(&rows)
	return rows, err
}

// Unlock removes the record of one identity and reports whether it existed.
func Unlock(app core.App, collection, identity string) (bool, error) {
	if !app.AuxHasTable(TableName) {
		return false, nil
	}
	res, err := app.AuxDB().NewQuery("DELETE FROM {{_lockout}} WHERE [[key]]={:k}").Bind(dbx.Params{"k": Key(collection, identity)}).Execute()
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Clear removes every record and returns how many were removed.
func Clear(app core.App) (int64, error) {
	if !app.AuxHasTable(TableName) {
		return 0, nil
	}
	res, err := app.AuxDB().NewQuery("DELETE FROM {{_lockout}}").Execute()
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
