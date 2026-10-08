//go:build !no_default_driver

package sqlite

import (
	"github.com/pocketbase/dbx"
	_ "modernc.org/sqlite"
)

// DefaultConnect opens the SQLite database at dbPath using the default
// modernc.org/sqlite driver and the default pragmas.
func DefaultConnect(dbPath string) (*dbx.DB, error) {
	pragmas := TuningFromEnv().Query()

	db, err := dbx.Open("sqlite", dbPath+pragmas)
	if err != nil {
		return nil, err
	}

	return db, nil
}
