package sqlite

import (
	"database/sql"
	"errors"
	"strings"

	"github.com/tokibase/tokibase/kernel"
)

// isLockedError reports whether err is a SQLITE_BUSY/SQLITE_LOCKED error.
//
// The error is checked against the plain error texts since the codes could vary between drivers.
func isLockedError(err error) bool {
	errStr := err.Error()

	return strings.Contains(errStr, "database is locked") ||
		strings.Contains(errStr, "table is locked")
}

// IsUniqueError reports whether err is a "UNIQUE constraint failed" error.
func IsUniqueError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

// classifyError implements [kernel.DBConn.ErrorKind].
func classifyError(err error) kernel.ErrKind {
	switch {
	case err == nil:
		return kernel.ErrKindOther
	case errors.Is(err, sql.ErrNoRows):
		return kernel.ErrKindNotFound
	case isLockedError(err):
		return kernel.ErrKindLocked
	case IsUniqueError(err):
		return kernel.ErrKindUnique
	case strings.Contains(strings.ToLower(err.Error()), "constraint failed"):
		return kernel.ErrKindConstraint
	default:
		return kernel.ErrKindOther
	}
}
