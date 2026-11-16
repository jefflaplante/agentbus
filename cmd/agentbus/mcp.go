package main

import (
	"runtime/debug"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/mcpbus"
)

func newMCPCmd(a *app) *cobra.Command {
	var capabilities []string
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the bus to an MCP client (Claude Code, OpenCode) over stdio",
		Long: `Run an MCP server on stdin/stdout that connects to the bus as one agent.
MCP clients such as Claude Code, OpenCode and OpenChamber start it as a
local (stdio) server and get these tools:

  agents   who is on the bus and what they serve
  send     one-way event to an agent's inbox
  request  ask an agent and wait for its reply
  task     hand work to whoever serves a capability, and wait for the reply
  emit     broadcast an event on this agent's event topic
  inbox    read messages that arrived for this agent (optionally wait)
  reply    answer a request read from the inbox

The server holds one connection for the whole session, so tool calls can
run in parallel. The session is persistent: messages sent while it wasn't
running are delivered when it starts. A request waits in the inbox until
it is answered with reply or its TTL runs out.

With --capability, tasks for that capability arrive in the inbox too and
are answered the same way.

Connection settings come from the usual flags and AGENTBUS_* variables.
Logs go to stderr; stdout carries only MCP traffic.`,
		Example: `  claude mcp add agentbus --env AGENTBUS_AGENT=claude-code.cli -- agentbus mcp
  agentbus mcp --agent opencode.cli --capability code-review`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ib := mcpbus.NewInbox()
			c, err := a.connect(cmd.Context(), false, func(o *client.Options) {
				o.CleanSession = false
				o.Inbox = ib.Handle
				if len(capabilities) > 0 {
					o.Tasks = map[string]client.Handler{}
					for _, capName := range capabilities {
						o.Tasks[capName] = ib.Handle
					}
				}
				o.Description = "MCP session (agentbus mcp)"
			})
			if err != nil {
				return err
			}
			defer c.Close()
			srv := mcpbus.NewServer(c, ib, a.agent, buildVersion(), nil)
			return srv.Run(cmd.Context(), &mcp.StdioTransport{})
		},
	}
	c.Flags().StringArrayVarP(&capabilities, "capability", "c", nil, "also take tasks for this capability (repeatable)")
	return c
}

// buildVersion is the module version this binary was built from, or
// "dev" for a local build.
func buildVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
