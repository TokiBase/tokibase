package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
	"github.com/tokibase/tokibase/modules/ruleguard"
)

// NewRuleCommand creates the `rule` command (lint, allow) backed by modules/ruleguard.
func NewRuleCommand(app core.App) *cobra.Command {
	command := &cobra.Command{
		Use:   "rule",
		Short: "Inspect public (empty string) API rules",
	}
	command.AddCommand(ruleLintCommand(app))
	command.AddCommand(ruleAllowCommand(app))
	return command
}

func ruleLintCommand(app core.App) *cobra.Command {
	var strict, asJSON bool

	command := &cobra.Command{
		Use:   "lint",
		Short: "Report collection rules that are public (empty string) and not allowlisted",
		Long: "Report collection rules that are public (empty string) and not allowlisted in <dataDir>/ruleguard.json.\n" +
			"Exit code 1 when there are error findings (with --strict: when there is any finding).",
		Example:      "rule lint --strict --json",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			pol, err := ruleguard.Load(app.DataDir())
			if err != nil {
				return err
			}
			findings, err := ruleguard.Lint(app, pol)
			if err != nil {
				return err
			}

			if asJSON {
				raw, err := json.MarshalIndent(findings, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(raw))
			} else {
				for _, f := range findings {
					label := color.CyanString("info ")
					if f.Severity == ruleguard.SeverityError {
						label = color.RedString("error")
					}
					fmt.Printf("%s %s.%s: %s\n", label, f.Collection, f.Rule, f.Message)
				}
			}

			errCount := len(ruleguard.Errors(findings))
			if !asJSON {
				fmt.Printf("%d finding(s), %d error(s), policy %q\n", len(findings), errCount, pol.Policy)
			}

			if errCount > 0 || (strict && len(findings) > 0) {
				return errors.New("ruleguard: lint failed")
			}
			return nil
		},
	}

	command.PersistentFlags().BoolVar(&strict, "strict", false, "exit with code 1 on any finding, including allowlisted (info) ones")
	command.PersistentFlags().BoolVar(&asJSON, "json", false, "print the findings as a JSON array")
	return command
}

func ruleAllowCommand(app core.App) *cobra.Command {
	return &cobra.Command{
		Use:          "allow <collection> <kind>...",
		Short:        "Allowlist public rule kinds of a collection in ruleguard.json",
		Long:         "Allowlist public rule kinds of a collection in <dataDir>/ruleguard.json.\nKinds: list, view, create, update, delete, manage, auth.",
		Example:      "rule allow posts list view",
		SilenceUsage: true,
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) < 2 {
				return errors.New("missing collection and rule kind arguments")
			}
			for _, k := range args[1:] {
				if !ruleguard.ValidKind(k) {
					return fmt.Errorf("invalid rule kind %q (expected one of %v)", k, ruleguard.Kinds)
				}
			}
			pol, err := ruleguard.Load(app.DataDir())
			if err != nil {
				return err
			}
			pol.Allow(args[0], args[1:]...)
			if err := ruleguard.Save(app.DataDir(), pol); err != nil {
				return err
			}
			color.Green("Allowlisted %v for %q in %s", args[1:], args[0], ruleguard.Path(app.DataDir()))
			return nil
		},
	}
}
