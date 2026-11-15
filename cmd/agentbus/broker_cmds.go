package main

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/internal/tui"
)

func newServeCmd(a *app) *cobra.Command {
	var config, format, level string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the broker",
		Long: `Run the a2a/v1 broker with the given config file. See CONTRACT.md for
what it enforces and docs/RETENTION.md for data_dir and audit_file.`,
		Example: "  agentbus serve --config broker.json\n  agentbus serve --config broker.json --log-format json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			cfg, err := broker.LoadConfig(config)
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
				"data_dir", cfg.DataDir, "audit_file", cfg.AuditFile)
			<-cmd.Context().Done()
			return b.Close()
		},
	}
	c.Flags().StringVarP(&config, "config", "c", "broker.json", "broker config file")
	c.Flags().StringVar(&format, "log-format", "text", "log format: text or json")
	c.Flags().StringVar(&level, "log-level", "info", "log level: debug, info, warn or error")
	return c
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
