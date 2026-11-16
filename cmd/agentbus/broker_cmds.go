package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

func newServeCmd(a *app) *cobra.Command {
	var format, level string
	var watch time.Duration
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the broker",
		Long: `Run the a2a/v1 broker.

The config file is ~/.config/agentbus/broker.json unless --config or
$AGENTBUS_CONFIG names another. Every agentbus command looks there, so
"agentbus console" in another terminal finds this broker with no flags.

On a server, set $AGENTBUS_CONFIG_DIR (say /etc/agentbus) to keep
broker.json and tokens/ there, and $AGENTBUS_STATE_DIR (say
/var/lib/agentbus) for the data and audit log. Without
$AGENTBUS_STATE_DIR, state goes in a "state" directory beside the config.

If the config doesn't exist, serve creates it: a loopback listener on
127.0.0.1:1883, storage under ~/.local/state/agentbus, and an "admin"
observer identity whose token goes in the tokens/ directory beside it. The
broker starts right away; "agentbus console" on this machine connects as
admin automatically.

The running broker reloads the config when the file changes (checked
every --watch; 0 turns file watching off) or on SIGHUP. Agents, tokens, permissions and rate limits
apply at once; listener, TLS, storage and limit changes need a restart.`,
		Example: `  agentbus serve                      # first run: creates a config and starts
  agentbus serve --config broker.json
  AGENTBUS_CONFIG_DIR=/etc/agentbus AGENTBUS_STATE_DIR=/var/lib/agentbus agentbus serve
  agentbus serve --log-format json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if watch < 0 {
				return fmt.Errorf("--watch must be 0 (reload on SIGHUP only) or a positive interval like 1s, not %s", watch)
			}
			var lvl slog.Level
			if err := lvl.UnmarshalText([]byte(level)); err != nil {
				return fmt.Errorf("--log-level: %w", err)
			}
			hopts := &slog.HandlerOptions{Level: lvl}
			var log *slog.Logger
			switch format {
			case "text":
				log = slog.New(slog.NewTextHandler(a.stderr, hopts))
			case "json":
				log = slog.New(slog.NewJSONHandler(a.stderr, hopts))
			default:
				return fmt.Errorf("--log-format must be text or json")
			}

			loc := a.location()
			created := false
			if !loc.Exists() {
				if loc.Explicit() {
					return fmt.Errorf("config file %s (from %s) does not exist; create it with "+
						"\"agentbus config init --config %s\", or run \"agentbus serve\" with no --config to use the default",
						loc.Path, loc.Source, loc.Path)
				}
				if _, err := confstore.Bootstrap(loc); err != nil {
					return fmt.Errorf("create default config: %w", err)
				}
				created = true
			}
			cfg, err := broker.LoadConfig(loc.Path)
			if err != nil {
				return err
			}
			b, err := broker.New(cfg, log)
			if err != nil {
				return err
			}
			if err := b.Start(); err != nil {
				return err
			}
			log.Info("agentbus broker listening", "addr", b.Addr(), "agents", len(cfg.Agents), "tls", cfg.TLSCert != "",
				"config", loc.Path, "data_dir", cfg.DataDir, "audit_file", cfg.AuditFile)
			if m := b.MetricsAddr(); m != "" {
				log.Info("metrics, health and readiness over HTTP", "metrics", "http://"+m+"/metrics",
					"healthz", "http://"+m+"/healthz", "readyz", "http://"+m+"/readyz")
			}
			// The join endpoint shares the MCP server when both name one
			// address, and gets its own otherwise.
			join := newJoinServer(loc, b.Addr(), cfg.TLSCert != "", log)
			var mounts []func(*http.ServeMux)
			if cfg.JoinListen != "" && cfg.JoinListen == cfg.MCPListen {
				mounts = append(mounts, join.mount)
				log.Info("join requests over HTTP", "addr", cfg.JoinListen, "path", "/join", "tls", cfg.TLSCert != "")
			}
			stopMCP, _, err := startMCPGateway(b, cfg, log, mounts...)
			if err != nil {
				_ = b.Close()
				return err
			}
			stopJoin := func() {}
			if cfg.JoinListen != "" && cfg.JoinListen != cfg.MCPListen {
				if stopJoin, _, err = startJoinEndpoint(join, cfg, log); err != nil {
					stopMCP()
					_ = b.Close()
					return err
				}
			}
			printServeHelp(a, loc, cfg, created)

			ctx := cmd.Context()
			if watch > 0 {
				go b.WatchConfig(ctx, loc.Path, watch)
			} else {
				b.SetConfigPath(loc.Path)
				log.Info("config file watching off (--watch 0); send SIGHUP to reload")
			}
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, syscall.SIGHUP)
			defer signal.Stop(hup)
			for {
				select {
				case <-hup:
					_, _ = b.ReloadFrom(loc.Path)
				case <-ctx.Done():
					stopJoin()
					stopMCP() // before the broker, so its agents disconnect cleanly
					return b.Close()
				}
			}
		},
	}
	c.Flags().StringVar(&format, "log-format", "text", "log format: text or json")
	c.Flags().StringVar(&level, "log-level", "info", "log level: debug, info, warn or error")
	c.Flags().DurationVar(&watch, "watch", time.Second, "how often to check the config file for changes (0: only reload on SIGHUP)")
	return c
}

// printServeHelp tells the operator what's running and what to do next.
func printServeHelp(a *app, loc confstore.Location, cfg *broker.Config, created bool) {
	out := a.stderr
	if !isTerminal(out) && !created {
		return // keep logs clean when not watched by a person
	}
	head := tui.AccentStyle.Render
	dim := tui.SubtleStyle.Render
	if created {
		lipgloss.Fprintln(out, "\n"+tui.OKStyle.Render("●")+" No config found, so agentbus created one and started with it:")
	} else {
		lipgloss.Fprintln(out, "\n"+tui.OKStyle.Render("●")+" agentbus is running")
	}
	rows := [][2]string{
		{"config", loc.Path + dim("  ("+string(loc.Source)+"; edits apply live)")},
		{"tokens", loc.TokensDir() + "/"},
		{"listen", cfg.Listen + dim("  ("+confstore.URL(cfg)+")")},
	}
	if cfg.DataDir != "" {
		rows = append(rows, [2]string{"data", cfg.DataDir})
	}
	if cfg.AuditFile != "" {
		rows = append(rows, [2]string{"audit", cfg.AuditFile})
	}
	for _, r := range rows {
		lipgloss.Fprintf(out, "  %s %s\n", head(fmt.Sprintf("%-7s", r[0])), r[1])
	}
	var ids []string
	for _, ag := range cfg.Agents {
		ids = append(ids, ag.ID)
	}
	lipgloss.Fprintf(out, "  %s %s\n\n", head(fmt.Sprintf("%-7s", "agents")), strings.Join(ids, ", "))
	lipgloss.Fprintln(out, head("Next, in another terminal:"))
	// Another terminal may not have the flag or variable that picked a
	// non-default config, so name it in every suggested command.
	cfgArg := ""
	if loc.Source != confstore.FromDefault {
		cfgArg = " --config " + loc.Path
	}
	cmd := func(c, note string) {
		lipgloss.Fprintln(out, "  agentbus "+c+cfgArg+"\n      "+dim("# "+note))
	}
	if cfgArg == "" {
		cmd = func(c, note string) { lipgloss.Fprintf(out, "  %-31s %s\n", "agentbus "+c, dim("# "+note)) }
	}
	if _, ok := confstore.ReadToken(loc, confstore.AdminID); ok {
		cmd("console", "status, agents, traffic, send and answer messages, config (as admin)")
	}
	cmd("config", "add agents, client setup, edit settings")
	cmd("config agent add NAME", "the same, from a script")
	if cfg.JoinListen != "" {
		cmd("config join", "approve or deny agents that asked to join (join_listen "+cfg.JoinListen+")")
	}
	if !confstore.Loopback(cfg.Listen) && cfg.TLSCert == "" {
		lipgloss.Fprintln(out, tui.WarnStyle.Render("  ! listening beyond loopback without TLS: set tls_cert and tls_key"))
	}
	lipgloss.Fprintln(out)
}

func newTokenCmd(a *app) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "token",
		Short: "Mint an agent token and the hash for the broker config",
		Long: `Print a new random 256-bit token and its SHA-256. Give the token to the
agent (in a file it reads with --token-file) and put token_sha256 in the
broker config's agents list.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			tok, hash := broker.NewToken()
			if asJSON {
				return json.NewEncoder(a.stdout).Encode(map[string]string{"token": tok, "token_sha256": hash})
			}
			if isTerminal(a.stdout) && !a.plain {
				lipgloss.Fprintf(a.stdout, "%s %s\n%s %s\n",
					tui.AccentStyle.Render("token:       "), tok,
					tui.AccentStyle.Render("token_sha256:"), hash)
				return nil
			}
			fmt.Fprintf(a.stdout, "token:        %s\ntoken_sha256: %s\n", tok, hash)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return c
}
