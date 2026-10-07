//go:build no_backupcheck

package cmd

import (
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// addBackupVerifyCommands adds nothing: list/verify/verify-all need modules/backupcheck.
func addBackupVerifyCommands(command *cobra.Command, app core.App) {}
