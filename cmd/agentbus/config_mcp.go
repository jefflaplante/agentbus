package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/internal/confstore"
)

func newConfigMCPCmd(a *app) *cobra.Command {
	var mcpClient, transport string
	c := &cobra.Command{
		Use:   "mcp ID",
		Short: "Print MCP client setup for an agent (Claude Code, Hermes, OpenClaw)",
		Long: `Print how to connect an MCP client to the bus as an agent, ready to paste.

With mcp_listen set in the broker config, the setup uses the broker's MCP
endpoint over HTTP: the client needs only the URL and the agent's token,
not the agentbus binary. Otherwise it runs "agentbus mcp" locally (stdio).
--transport picks one explicitly.

The token itself is never printed. HTTP setups read it from
$AGENTBUS_TOKEN, which each client expands when it connects, so it stays
out of the client's config file; stdio setups point at the token file.

Give each client its own agent: two clients sharing an identity share one
inbox. Observer identities can't use the HTTP endpoint.`,
		Example: `  agentbus config mcp claude-code
  agentbus config mcp hermes-home --client hermes
  agentbus config mcp openclaw --client openclaw --transport stdio`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			i := confstore.Find(cfg, args[0])
			if i < 0 {
				return fmt.Errorf("no agent %q in %s", args[0], loc.Path)
			}
			mcpURL := confstore.MCPURL(cfg)
			switch transport {
			case "auto":
			case "stdio":
				mcpURL = ""
			case "http":
				if mcpURL == "" {
					return errors.New("the MCP endpoint is off: set mcp_listen (agentbus config set mcp_listen 127.0.0.1:8765), " +
						"restart the broker, and try again")
				}
			default:
				return errors.New("--transport must be auto, http or stdio")
			}
			if mcpURL != "" && cfg.Agents[i].Observer {
				return fmt.Errorf("%s is an observer identity, which the HTTP endpoint refuses; use --transport stdio, "+
					"or a non-observer agent", args[0])
			}
			out, err := confstore.MCPSetup(mcpClient, mcpURL, confstore.URL(cfg), args[0], loc.TokenFile(args[0]))
			if err != nil {
				return err
			}
			fmt.Fprint(a.stdout, out)
			return nil
		},
	}
	c.Flags().StringVar(&mcpClient, "client", "claude", "MCP client: "+strings.Join(confstore.MCPClients, ", "))
	c.Flags().StringVar(&transport, "transport", "auto", "auto (http when mcp_listen is set), http or stdio")
	_ = c.RegisterFlagCompletionFunc("client", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return confstore.MCPClients, cobra.ShellCompDirectiveNoFileComp
	})
	_ = c.RegisterFlagCompletionFunc("transport", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"auto", "http", "stdio"}, cobra.ShellCompDirectiveNoFileComp
	})
	return c
}
