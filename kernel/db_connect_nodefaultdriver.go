//go:build no_default_driver

package kernel

import "github.com/pocketbase/dbx"

func DefaultDBConnect(dbPath string) (*dbx.DB, error) {
	panic("DBConnect config option must be set when the no_default_driver tag is used!")
}
