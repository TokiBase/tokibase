//go:build no_default_driver

package sqlite

import "github.com/pocketbase/dbx"

// DefaultConnect panics because the default driver is excluded from the build.
func DefaultConnect(dbPath string) (*dbx.DB, error) {
	panic("DBConnect config option must be set when the no_default_driver tag is used!")
}
