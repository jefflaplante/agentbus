package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
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

The config file is --config, else $AGENTBUS_CONFIG, else ./broker.json,
else ~/.config/agentbus/broker.json. If none of those exists (and you
didn't name one), serve creates the default: a loopback listener on
127.0.0.1:1883, storage under ~/.local/state/agentbus, and an "admin"
observer identity whose token goes in ~/.config/agentbus/tokens/. The
broker starts right away; "agentbus status" on this machine connects as
admin automatically.

The running broker reloads the config when the file changes (checked
every --watch; 0 turns file watching off) or on SIGHUP. Agents, tokens, permissions and rate limits
apply at once; listener, TLS, storage and limit changes need a restart.`,
		Example: `  agentbus serve                      # first run: creates a config and starts
  agentbus serve --config broker.json
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
	if _, ok := confstore.ReadToken(loc, confstore.AdminID); ok {
		lipgloss.Fprintln(out, "  agentbus status                  "+dim("# watch this broker (connects as admin automatically)"))
	}
	lipgloss.Fprintln(out, "  agentbus config                  "+dim("# add agents, client setup, edit settings"))
	lipgloss.Fprintln(out, "  agentbus config agent add NAME   "+dim("# the same, from a script"))
	if !loopbackListen(cfg.Listen) && cfg.TLSCert == "" {
		lipgloss.Fprintln(out, tui.WarnStyle.Render("  ! listening beyond loopback without TLS: set tls_cert and tls_key"))
	}
	lipgloss.Fprintln(out)
}

// loopbackListen reports whether a listen address only accepts local
// connections: localhost, 127.0.0.0/8 or ::1 ([::1]:1883).
func loopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = strings.Trim(listen, "[]")
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
