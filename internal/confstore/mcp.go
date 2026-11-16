package confstore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MCP clients `agentbus config mcp` writes setup for.
var MCPClients = []string{"claude", "hermes", "openclaw", "json"}

// MCPSetup is how to connect MCP client to the bus as agent id: over HTTP
// to mcpURL when it is set, or else by running `agentbus mcp` (stdio)
// against the MQTT broker at mqttURL. The token never appears in it:
// HTTP setups read it from $AGENTBUS_TOKEN, which each client expands
// when it connects, and stdio setups point at the token file.
func MCPSetup(mcpClient, mcpURL, mqttURL, id, tokenFile string) (string, error) {
	if mcpURL != "" {
		return mcpHTTPSetup(mcpClient, mcpURL, tokenFile)
	}
	return mcpStdioSetup(mcpClient, mqttURL, id, tokenFile)
}

const tokenRef = "${AGENTBUS_TOKEN}" // expanded by the MCP client, never by the shell

func mcpHTTPSetup(mcpClient, url, tokenFile string) (string, error) {
	export := fmt.Sprintf("export AGENTBUS_TOKEN=\"$(cat %s)\"", shellPath(tokenFile))
	authz := "Bearer " + tokenRef
	var b strings.Builder
	switch mcpClient {
	case "claude":
		fmt.Fprintf(&b, "# Claude Code, in every project. Claude Code expands ${AGENTBUS_TOKEN} when it\n")
		fmt.Fprintf(&b, "# connects, so the token stays out of ~/.claude.json. Put the export in your\n")
		fmt.Fprintf(&b, "# shell profile, so every claude you start has it.\n")
		fmt.Fprintf(&b, "%s\n", export)
		fmt.Fprintf(&b, "claude mcp add --scope user --transport http agentbus %s \\\n", shellQuote(url))
		fmt.Fprintf(&b, "  --header 'Authorization: %s'\n\n", authz)
		fmt.Fprintf(&b, "# Or for one project, in .mcp.json at the repository root:\n")
		b.WriteString(mustIndent(map[string]any{"mcpServers": map[string]any{"agentbus": map[string]any{
			"type": "http", "url": url, "headers": map[string]string{"Authorization": authz}}}}))
	case "hermes":
		fmt.Fprintf(&b, "# Hermes Agent: add to ~/.hermes/config.yaml. Hermes expands ${AGENTBUS_TOKEN}\n")
		fmt.Fprintf(&b, "# from the environment or ~/.hermes/.env when it connects.\n")
		fmt.Fprintf(&b, "mcp_servers:\n  agentbus:\n    url: %q\n    headers:\n      Authorization: %q\n\n", url, authz)
		fmt.Fprintf(&b, "# Then give Hermes the token, in its environment or in ~/.hermes/.env (keep it mode 0600):\n")
		fmt.Fprintf(&b, "%s\n", export)
	case "openclaw":
		fmt.Fprintf(&b, "# OpenClaw: saved to ~/.openclaw/openclaw.json. OpenClaw expands ${AGENTBUS_TOKEN}\n")
		fmt.Fprintf(&b, "# when it connects; transport must be streamable-http (its default is sse).\n")
		fmt.Fprintf(&b, "%s\n", export)
		fmt.Fprintf(&b, "openclaw mcp set agentbus %s\n", shellQuote(mustCompact(map[string]any{
			"url": url, "transport": "streamable-http", "headers": map[string]string{"Authorization": authz}})))
	case "json":
		fmt.Fprintf(&b, "# A generic MCP config. Set AGENTBUS_TOKEN in the client's environment:\n# %s\n", export)
		b.WriteString(mustIndent(map[string]any{"mcpServers": map[string]any{"agentbus": map[string]any{
			"type": "http", "url": url, "headers": map[string]string{"Authorization": authz}}}}))
	default:
		return "", unknownMCPClient(mcpClient)
	}
	return b.String(), nil
}

func mcpStdioSetup(mcpClient, mqttURL, id, tokenFile string) (string, error) {
	env := map[string]string{"AGENTBUS_URL": mqttURL, "AGENTBUS_AGENT": id, "AGENTBUS_TOKEN_FILE": tokenFile}
	server := map[string]any{"command": "agentbus", "args": []string{"mcp"}, "env": env}
	var b strings.Builder
	switch mcpClient {
	case "claude":
		fmt.Fprintf(&b, "# Claude Code, in every project: starts `agentbus mcp`, which needs agentbus on PATH\n")
		fmt.Fprintf(&b, "# and the token file on this machine.\n")
		fmt.Fprintf(&b, "claude mcp add --scope user agentbus \\\n  --env AGENTBUS_URL=%s --env AGENTBUS_AGENT=%s \\\n"+
			"  --env AGENTBUS_TOKEN_FILE=%s -- agentbus mcp\n", shellQuote(mqttURL), shellQuote(id), shellPath(tokenFile))
	case "hermes":
		fmt.Fprintf(&b, "# Hermes Agent: add to ~/.hermes/config.yaml.\n")
		fmt.Fprintf(&b, "mcp_servers:\n  agentbus:\n    command: agentbus\n    args: [mcp]\n    env:\n")
		fmt.Fprintf(&b, "      AGENTBUS_URL: %q\n      AGENTBUS_AGENT: %q\n      AGENTBUS_TOKEN_FILE: %q\n", mqttURL, id, tokenFile)
	case "openclaw":
		fmt.Fprintf(&b, "# OpenClaw: saved to ~/.openclaw/openclaw.json.\n")
		fmt.Fprintf(&b, "openclaw mcp set agentbus %s\n", shellQuote(mustCompact(server)))
	case "json":
		fmt.Fprintf(&b, "# A generic MCP config (stdio).\n")
		b.WriteString(mustIndent(map[string]any{"mcpServers": map[string]any{"agentbus": server}}))
	default:
		return "", unknownMCPClient(mcpClient)
	}
	return b.String(), nil
}

func unknownMCPClient(c string) error {
	return fmt.Errorf("--client must be one of %s, not %q", strings.Join(MCPClients, ", "), c)
}

func mustIndent(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

func mustCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
