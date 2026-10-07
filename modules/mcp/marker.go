//go:build !no_mcp

package mcp

import "github.com/tokibase/tokibase/kernel"

func init() {
	kernel.RegisterModuleMarker("mcp", []string{"_agents"}, []string{"TOKI_MCP", "TOKI_AGENT_KEY", "TOKI_MCP_REQUIRE_HUMAN_CONFIRM"}, false)
}
