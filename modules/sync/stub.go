//go:build no_sync

// Package sync is compiled out of this binary by the no_sync build tag.
// Enabled() reports false and Register is a no-op.
package sync

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// Role is the sync role of the process.
type Role string

// Roles.
const (
	RoleOff   Role = "off"
	RoleHub   Role = "hub"
	RoleSpoke Role = "spoke"
)

// Module is never instantiated in builds with the no_sync tag.
type Module struct{}

var errCompiledOut = errors.New("sync is not available in this build (no_sync)")

// RoleFromEnv reports RoleOff in builds with the no_sync tag.
func RoleFromEnv() Role { return RoleOff }

// Enabled reports false in builds with the no_sync tag.
func Enabled() bool { return false }

// Register is a no-op returning nil in builds with the no_sync tag.
func Register(app core.App) *Module { return nil }

// RegisterRole is a no-op returning nil in builds with the no_sync tag.
func RegisterRole(app core.App, role Role) *Module { return nil }

// NewCommand returns a hidden command that reports the module is compiled out.
func NewCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use: "sync", Hidden: true, SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error { return errCompiledOut },
	}
}
