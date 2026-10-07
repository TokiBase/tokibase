//go:build no_timelint

// Package timelint is compiled out of this binary by the no_timelint build tag.
package timelint

import "github.com/tokibase/tokibase/core"

// Policy is the date-without-zone handling policy.
type Policy string

// Register is a no-op in builds with the no_timelint tag.
func Register(app core.App) {}

// PolicyFromEnv always reports "off" in builds with the no_timelint tag.
func PolicyFromEnv() Policy { return "off" }
