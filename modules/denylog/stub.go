//go:build no_denylog

// Package denylog is compiled out of this binary by the no_denylog build tag.
package denylog

import (
	"time"

	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/kernel"
)

// Entry is the denied-request record shape used by the MCP provider wiring.
type Entry struct{}

// Enabled reports false in builds with the no_denylog tag.
func Enabled() bool { return false }

// Register is a no-op in builds with the no_denylog tag.
func Register(app core.App) {}

// Tail returns no entries in builds with the no_denylog tag.
func Tail(app kernel.App, since time.Duration, limit int) ([]Entry, error) { return nil, nil }
