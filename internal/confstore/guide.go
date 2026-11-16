package confstore

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/jefflaplante/agentbus/broker"
)

// Guide is what to do after adding an agent or rotating its token: where
// the token is, and the commands that start a client as that agent, on
// this machine and on another host. The CLI prints it after "config agent
// add" and "config agent setup"; the config editor shows it on its client
// setup screen.
func Guide(loc Location, cfg *broker.Config, id string) string {
	i := Find(cfg, id)
	var ag broker.AgentConfig
	if i >= 0 {
		ag = cfg.Agents[i]
	}
	file := loc.TokenFile(id)
	url := URL(cfg)
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("Token file")
	line("  %s", Tilde(file))
	if _, err := os.Stat(file); err != nil {
		line("  ! missing: rotate the token to create it (agentbus config agent token %s)", id)
	}
	line("  Readable only by you (mode 0600). Clients read the token from this file;")
	line("  it is never printed. Keep it out of git and chat.")
	line("")

	// On this machine the CLI finds the token beside the config, so the
	// agent ID is enough. A non-default config has to be named too.
	cli := "agentbus --agent " + shellQuote(id)
	if loc.Source != FromDefault {
		cli = "agentbus --config " + shellQuote(loc.Path) + " --agent " + shellQuote(id)
	}
	line("Start it on this machine")
	cmds := startCommands(ag, id)
	for _, c := range cmds {
		line("  %s %s", cli, c[0])
		line("      %s", c[1])
	}
	line("  These find the broker and the token from %s; no other settings needed.", Tilde(loc.Path))
	line("")

	line("Or set it for this shell, or for a program using the Go or Python client")
	for _, l := range strings.Split(strings.TrimSpace(ClientEnv(url, id, file)), "\n") {
		line("  %s", l)
	}
	line("  agentbus %s", cmds[0][0])
	line("  Unset AGENTBUS_URL, AGENTBUS_AGENT and AGENTBUS_TOKEN_FILE afterwards, or later")
	line("  commands in this shell keep acting as %s.", id)
	line("")

	remote := "/etc/agentbus/" + id + ".token"
	line("Start it on another host")
	line("  1. Copy the token file there, readable only by the user that runs the agent:")
	line("       scp %s HOST:%s.token", shellPath(file), id)
	line("       ssh HOST 'sudo install -D -m 0600 -o AGENT-USER %s.token %s && rm %s.token'", id, remote, id)
	line("  2. Start it, pointing at this broker:")
	line("       agentbus --url %s --agent %s --token-file %s %s",
		remoteURL(cfg, url), shellQuote(id), remote, cmds[0][0])
	line("     Standalone agents (agentbus agent run, and programs made with agentbus agent init)")
	line("     find %s without the flag.", remote)
	line("  3. To keep it running, use a systemd unit: examples/systemd/agentbus-handle@.service")
	line("     in the agentbus repository runs it as agentbus-handle@%s.", id)
	if Loopback(cfg.Listen) {
		line("  ! The broker listens on %s, this machine only. Before another host can", orDefault(cfg.Listen))
		line("    connect, set listen, tls_cert and tls_key (agentbus config set ...), then restart it.")
	} else if cfg.TLSCert == "" {
		line("  ! No TLS: the token crosses the network in clear. Set tls_cert and tls_key.")
	}
	return b.String()
}

// startCommands are the CLI commands that suit an agent's grants, the most
// likely first, each with what it does.
func startCommands(ag broker.AgentConfig, id string) [][2]string {
	var out [][2]string
	if ag.Observer {
		out = append(out, [2]string{"console", "the full console: status, agents, traffic, sending, config"})
	}
	for _, c := range ag.Capabilities {
		if c != "*" {
			out = append(out, [2]string{"handle --capability " + shellQuote(c) + " --exec 'CMD'",
				"take its " + c + " tasks; CMD is your program, run once per task"})
			break
		}
	}
	to := "AGENT"
	if len(ag.SendTo) > 0 {
		to = ag.SendTo[0]
	}
	return append(out,
		[2]string{"handle --exec 'CMD'", "answer requests sent to " + id + "; CMD runs once per request"},
		[2]string{"listen", "print messages sent to " + id + " as they arrive"},
		[2]string{"request --to " + shellQuote(to) + " --body '{}'", "send a request as " + id + " and print the reply"})
}

// remoteURL is the broker URL as another host would use it: the listen
// host when it's a real address, else this machine's hostname.
func remoteURL(cfg *broker.Config, url string) string {
	host, port, err := net.SplitHostPort(orDefault(cfg.Listen))
	if err != nil {
		return url
	}
	if host == "" || host == "0.0.0.0" || host == "::" || Loopback(cfg.Listen) {
		host = "BROKER-HOST"
		if h, err := os.Hostname(); err == nil && h != "" && !Loopback(cfg.Listen) {
			host = h
		}
	}
	scheme := "tcp"
	if cfg.TLSCert != "" {
		scheme = "ssl"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func orDefault(listen string) string {
	if listen == "" {
		return broker.DefaultListen
	}
	return listen
}

// Loopback reports whether a listen address only accepts local
// connections: localhost, 127.0.0.0/8 or ::1 ([::1]:1883).
func Loopback(listen string) bool {
	host, _, err := net.SplitHostPort(orDefault(listen))
	if err != nil {
		host = strings.Trim(listen, "[]")
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
