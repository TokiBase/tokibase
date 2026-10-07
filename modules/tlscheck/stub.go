//go:build no_tlscheck

// Package tlscheck is compiled out of this binary by the no_tlscheck build tag.
package tlscheck

import "github.com/tokibase/tokibase/core"

// Register is a no-op in builds with the no_tlscheck tag.
func Register(app core.App) {}
