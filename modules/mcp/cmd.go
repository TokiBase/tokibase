//go:build !no_mcp

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/tokibase/tokibase/core"
)

// EnvAgentKey is the env variable carrying the agent API key for stdio.
const EnvAgentKey = "TOKI_AGENT_KEY"

// NewCommands returns the `mcp`, `agent` and `gen` commands.
func NewCommands(app core.App) []*cobra.Command {
	return []*cobra.Command{mcpCommand(app), agentCommand(app), genCommand(app)}
}

func mcpCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "mcp", Short: "Model Context Protocol server for AI agents"}
	root.AddCommand(&cobra.Command{
		Use:   "serve",
		Short: "Serve MCP over stdio as the agent identified by TOKI_AGENT_KEY",
		Long: "Serve the Model Context Protocol over stdio (stdin/stdout) so Claude Code, Cursor and other agents can inspect and manage this instance.\n" +
			"The agent is authenticated with env " + EnvAgentKey + " (create one with `agent create`). stdout carries the protocol only; diagnostics go to stderr.\n" +
			"Run it against the data dir of the instance (--dir).",
		Example:      "claude mcp add tokibase -e " + EnvAgentKey + "=tka_... -- toki mcp serve --dir pb_data",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			agent, err := Authenticate(app, os.Getenv(EnvAgentKey))
			if err != nil {
				return fmt.Errorf("%s: %w (set %s to a key from `agent create`)", "mcp", err, EnvAgentKey)
			}
			srv := NewServer(app, agent, "")
			if !auditEnabled() {
				if unauditedAllowed() {
					fmt.Fprintf(os.Stderr, "toki mcp: WARNING audit is disabled and %s=1: agent writes are NOT recorded\n", EnvUnaudited)
				} else {
					fmt.Fprintf(os.Stderr, "toki mcp: audit is disabled: write tools are refused (set %s=1 to accept unaudited writes)\n", EnvUnaudited)
				}
			}
			fmt.Fprintf(os.Stderr, "toki mcp: serving agent %q (role %s) session %s\n", agent.Name, agent.Role, srv.Session())
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = srv.Run(ctx, &sdk.StdioTransport{})
			if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	})
	return root
}

func agentCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "agent", Short: "Manage AI agent identities (MCP)"}

	var (
		role string
		cols []string
		rate int

		sandbox bool
		expires string
	)
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an agent and print its API key once",
		Long: "Create an agent identity in the _agents collection and print its API key ONCE (only a sha256 hash is stored).\n" +
			"Roles: reader (schema + reads), writer (+ create/update/delete), operator (+ audit/backup/replica/deny/lockout, bypasses rules).\n" +
			"--collections limits the collections the agent may touch (empty = all).\n" +
			"--sandbox makes every write a dry run (executed in a transaction that is rolled back). --expires (RFC 3339, 2006-01-02 or Nd, e.g. 90d) limits the key's lifetime.",
		Example:      "agent create reviewer --role writer --collections posts,comments",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			exp, err := parseExpires(expires, time.Now())
			if err != nil {
				return err
			}
			a, key, err := CreateAgentOpts(app, args[0], Role(role), cols, rate, AgentOptions{Sandbox: sandbox, Expires: exp})
			if err != nil {
				return err
			}
			emit("agent.created", "", a.ID, map[string]any{
				"cli": true, "agent": a.Name, "agent_id": a.ID, "role": string(a.Role),
				"collections": a.Collections, "rate_per_min": a.RatePerMin, "sandbox": a.Sandbox, "expires": a.Expires,
			})
			out := c.OutOrStdout()
			fmt.Fprintf(out, "Agent %q created (role %s, %d calls/min, collections: %s, sandbox: %v).\n", a.Name, a.Role, a.RatePerMin, listOrAll(a.Collections), a.Sandbox)
			fmt.Fprintf(out, "API key (shown once, store it now):\n\n  %s\n\n", key)
			fmt.Fprintf(out, "Connect:\n  claude mcp add tokibase -e %s=%s -- toki mcp serve --dir %s\n", EnvAgentKey, key, app.DataDir())
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", string(RoleReader), "reader, writer or operator")
	create.Flags().StringSliceVar(&cols, "collections", nil, "comma separated collections the agent may touch (default all)")
	create.Flags().BoolVar(&sandbox, "sandbox", false, "writes are dry runs: executed in a transaction that is always rolled back")
	create.Flags().StringVar(&expires, "expires", "", "key expiry: RFC 3339, YYYY-MM-DD or Nd (days)")
	create.Flags().IntVar(&rate, "rate", DefaultRatePerMin, "calls per minute")

	var asJSON bool
	list := &cobra.Command{
		Use:          "list",
		Short:        "List agents",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			agents, err := ListAgents(app)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(c.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(agents)
			}
			tw := tabwriter.NewWriter(c.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tROLE\tENABLED\tSANDBOX\tRATE/MIN\tCOLLECTIONS\tEXPIRES\tCREATED")
			for _, a := range agents {
				fmt.Fprintf(tw, "%s\t%s\t%v\t%v\t%d\t%s\t%s\t%s\n", a.Name, a.Role, a.Enabled, a.Sandbox, a.RatePerMin, listOrAll(a.Collections), a.Expires, a.Created)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "output JSON")

	revoke := &cobra.Command{
		Use:          "revoke <name>",
		Short:        "Disable an agent (running MCP sessions stop on their next call)",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			a, err := RevokeAgent(app, args[0])
			if err != nil {
				return err
			}
			emit("agent.revoked", "", a.ID, map[string]any{"cli": true, "agent": a.Name, "agent_id": a.ID})
			fmt.Fprintf(c.OutOrStdout(), "Agent %q revoked.\n", a.Name)
			return nil
		},
	}
	confirm := &cobra.Command{
		Use:   "confirm <token>",
		Short: "Approve a pending destructive plan (needs " + EnvRequireHumanConfirm + "=1 on the MCP server)",
		Long: "With " + EnvRequireHumanConfirm + "=1 the confirm_token of records.delete / records.batch is only valid after a human approved it here.\n" +
			"Review the printed agent, tool and summary before approving. An agent that can run shell commands can run this too: do not give it shell access to the instance.",
		SilenceUsage: true,
		Args:         cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := ApproveConfirm(app.DataDir(), args[0], time.Now())
			if err != nil {
				return err
			}
			emit("agent.confirmed", "", p.AgentID, map[string]any{"cli": true, "agent": p.Agent, "agent_id": p.AgentID, "tool": p.Tool})
			fmt.Fprintf(c.OutOrStdout(), "Approved %s for agent %q (valid until %s).\nSummary: %s\n", p.Tool, p.Agent, p.Expires.Format(time.RFC3339), p.Summary)
			return nil
		},
	}
	root.AddCommand(create, list, revoke, confirm)
	return root
}

// parseExpires reads --expires: empty = never, Nd = N days from now,
// YYYY-MM-DD (end of that day UTC) or RFC 3339.
func parseExpires(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(v, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(v, "d")); err == nil && n > 0 {
			return now.Add(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	var t time.Time
	if tt, err := time.Parse(time.RFC3339, v); err == nil {
		t = tt
	} else if tt, err := time.Parse("2006-01-02", v); err == nil {
		t = tt.Add(24*time.Hour - time.Second)
	}
	if !t.IsZero() {
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("--expires %q is in the past", v)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --expires %q (use RFC 3339, YYYY-MM-DD or Nd)", v)
}

func listOrAll(l []string) string {
	if len(l) == 0 {
		return "all"
	}
	return strings.Join(l, ",")
}

func genCommand(app core.App) *cobra.Command {
	root := &cobra.Command{Use: "gen", Short: "Generate documentation files for AI agents"}
	var (
		out   string
		force bool
	)
	agentsMD := &cobra.Command{
		Use:   "agents-md",
		Short: "Write AGENTS.md and llms.txt describing this instance",
		Long: "Write AGENTS.md (--out) and llms.txt (same directory) with the collections, a rules summary, the enabled modules and how to connect an agent over MCP.\n" +
			"Existing files are not overwritten unless --force is given.",
		Example:      "gen agents-md --out AGENTS.md",
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			md, llms, err := GenerateDocs(app)
			if err != nil {
				return err
			}
			llmsPath := filepath.Join(filepath.Dir(out), "llms.txt")
			for _, f := range []struct{ path, body string }{{out, md}, {llmsPath, llms}} {
				if !force {
					if _, err := os.Stat(f.path); err == nil {
						return fmt.Errorf("%s already exists (use --force to overwrite)", f.path)
					}
				}
			}
			for _, f := range []struct{ path, body string }{{out, md}, {llmsPath, llms}} {
				if err := os.WriteFile(f.path, []byte(f.body), 0o644); err != nil {
					return err
				}
				fmt.Fprintf(c.OutOrStdout(), "wrote %s\n", f.path)
			}
			return nil
		},
	}
	agentsMD.Flags().StringVar(&out, "out", "AGENTS.md", "output path of AGENTS.md (llms.txt goes next to it)")
	agentsMD.Flags().BoolVar(&force, "force", false, "overwrite existing files")
	root.AddCommand(agentsMD)
	return root
}
