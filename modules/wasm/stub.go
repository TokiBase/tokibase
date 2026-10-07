//go:build no_wasm

// Package wasm is a stub in builds with the no_wasm tag (no wazero linked).
package wasm

import (
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Enabled is always false with the no_wasm tag.
func Enabled() bool { return false }

// Register is a no-op in builds with the no_wasm tag.
func Register(app core.App, root *cobra.Command) any { return nil }

// NewCommand returns nil in builds with the no_wasm tag (callers skip nil).
func NewCommand(app core.App) *cobra.Command { return nil }
