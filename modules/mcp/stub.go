//go:build no_mcp

package mcp

import (
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Register is a no-op in builds with the no_mcp tag.
func Register(app core.App) {}

// NewCommands returns no commands in builds with the no_mcp tag.
func NewCommands(app core.App) []*cobra.Command { return nil }

// RegisterHTTP is a no-op in builds with the no_mcp tag (/api/mcp answers 404).
func RegisterHTTP(app core.App) {}
