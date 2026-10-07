//go:build no_adminlock

// Package adminlock is compiled out of this binary by the no_adminlock build tag.
package adminlock

import "github.com/tokibase/tokibase/core"

// ActionBlocked is the audit action of a blocked admin request.
const ActionBlocked = "admin.blocked"

// Mode is the Admin UI mode.
type Mode string

// ModeOn is the only mode of a build with the no_adminlock tag.
const ModeOn Mode = "on"

// Block describes one blocked admin request.
type Block struct {
	Action, Collection, Record  string
	Method, Path, IP, UserAgent string
	ActorID, ActorColl          string
}

// ModeFromEnv returns "" in builds with the no_adminlock tag (module absent).
func ModeFromEnv() Mode { return "" }

// Register is a no-op in builds with the no_adminlock tag.
func Register(app core.App) Mode { return "" }

// SetAuditSink is a no-op in builds with the no_adminlock tag.
func SetAuditSink(fn func(Block)) {}
