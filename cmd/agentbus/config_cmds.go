package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

func newConfigCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage the broker config, agents and client setup",
		Long: `Manage the broker config file and the agent tokens beside it.

On a terminal, "agentbus config" opens an editor: add, edit and remove
agents, rotate tokens, get client setup snippets, and change broker
settings. Saves are validated. A running broker reloads the file within a
second: agent, token, permission and rate changes apply at once, and
listener, TLS and storage changes after a restart.

The subcommands do the same from scripts. The file is
~/.config/agentbus/broker.json unless --config or $AGENTBUS_CONFIG names
another; "agentbus config path" shows which one is in use.`,
		Example: `  agentbus config                                  # interactive editor
  agentbus config agent add researcher --capability research --send-to conduit-home
  agentbus config env researcher >> ~/.researcher.env
  agentbus config set rate_per_second 50
  agentbus config join approve worker-7            # answer a host that ran "agentbus join"
  agentbus config set tls_cert cert.pem tls_key key.pem listen 0.0.0.0:8883`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loc := a.location()
			if !a.interactive() {
				return showConfig(a, loc)
			}
			if !loc.Exists() {
				if loc.Explicit() {
					return fmt.Errorf("%s does not exist; run \"agentbus config init --config %s\"", loc.Path, loc.Path)
				}
				if _, err := confstore.Bootstrap(loc); err != nil {
					return err
				}
			}
			m, err := tui.NewConfigEditor(loc, "")
			if err != nil {
				return err
			}
			return a.runTUI(cmd.Context(), m)
		},
	}
	c.AddCommand(newConfigInitCmd(a), newConfigPathCmd(a), newConfigShowCmd(a), newConfigSetCmd(a),
		newConfigAgentCmd(a), newConfigJoinCmd(a), newConfigEnvCmd(a), newConfigMCPCmd(a))
	return c
}

func newConfigInitCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create a config with an admin identity",
		Long: `Create a working config: a loopback listener on 127.0.0.1:1883, storage,
and an "admin" observer identity with its token in the tokens directory.
"agentbus serve" does this by itself on first run.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			loc := a.location()
			cfg, err := confstore.Bootstrap(loc)
			if err != nil {
				return err
			}
			lipgloss.Fprintf(a.stdout, "%s created %s\n  admin token: %s\n  listen:      %s\n", tui.OKStyle.Render("✓"),
				loc.Path, loc.TokenFile(confstore.AdminID), cfg.Listen)
			return nil
		},
	}
}

func newConfigPathCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Show which config file and token directory are used",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			loc := a.location()
			exists := "exists"
			state := confstore.DefaultStateDir(loc)
			data, audit := filepath.Join(state, "data"), filepath.Join(state, "audit.jsonl")
			if !loc.Exists() {
				exists = "does not exist yet; these are the paths it would get"
			} else if cfg, err := confstore.Load(loc.Path); err == nil {
				data, audit = orNone(cfg.DataDir), orNone(cfg.AuditFile)
			}
			fmt.Fprintf(a.stdout, "config  %s (%s, %s)\ntokens  %s\ndata    %s\naudit   %s\n", loc.Path, loc.Source, exists,
				loc.TokensDir(), data, audit)
			return nil
		},
	}
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// change applies fn to the config file and saves it, token files only
// after the config (see confstore.Tx).
func change(loc confstore.Location, fn func(tx *confstore.Tx) error) (*broker.Config, error) {
	tx, err := confstore.Begin(loc)
	if err != nil {
		return nil, err
	}
	if err := fn(tx); err != nil {
		tx.Abort()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return tx.Config, nil
}

func newConfigShowCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the config file",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return showConfig(a, a.location()) },
	}
}

func showConfig(a *app, loc confstore.Location) error {
	data, err := os.ReadFile(loc.Path)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no config at %s (%s); \"agentbus serve\" or \"agentbus config init\" creates one", loc.Path, loc.Source)
	}
	if err != nil {
		return err
	}
	_, err = a.stdout.Write(data)
	return err
}

// settingKeys are the broker settings "config set" accepts, from the JSON
// tags of broker.Config.
func settingKeys() []string {
	var keys []string
	t := reflect.TypeFor[broker.Config]()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "agents" {
			keys = append(keys, name)
		}
	}
	return keys
}

func newConfigSetCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE [KEY VALUE]...",
		Short: "Change broker settings",
		Long: `Change top-level broker settings. VALUE is JSON (50, true, "10m") or a bare
string. Several KEY VALUE pairs are applied together and validated and
saved once, for settings that only make sense together: tls_cert and
tls_key, or a non-loopback listen address and TLS.

Keys: ` + strings.Join(settingKeys(), ", "),
		Example: `  agentbus config set rate_per_second 50
  agentbus config set session_expiry 72h
  agentbus config set tls_cert /etc/agentbus/cert.pem tls_key /etc/agentbus/key.pem listen 0.0.0.0:8883`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 || len(args)%2 != 0 {
				return errors.New("want KEY VALUE pairs, like: config set rate_per_second 50")
			}
			return nil
		},
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args)%2 == 0 {
				return settingKeys(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, args []string) error {
			_, err := change(a.location(), func(tx *confstore.Tx) error {
				for i := 0; i < len(args); i += 2 {
					if err := tx.Set(args[i], args[i+1]); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			for i := 0; i < len(args); i += 2 {
				lipgloss.Fprintf(a.stderr, "%s %s = %s\n", tui.OKStyle.Render("✓"), args[i], args[i+1])
			}
			return nil
		},
	}
}

func newConfigAgentCmd(a *app) *cobra.Command {
	c := &cobra.Command{Use: "agent", Short: "List, add, remove agents and rotate tokens"}

	list := &cobra.Command{
		Use: "list", Short: "List configured agents", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tROLE\tSEND TO\tRATE\tTOKEN FILE")
			for _, ag := range cfg.Agents {
				tok := loc.TokenFile(ag.ID)
				if _, ok := confstore.ReadToken(loc, ag.ID); !ok {
					tok = "(none: rotate to create)"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ag.ID, tui.AgentRole(ag), tui.SendTo(ag), tui.AgentRate(ag), tok)
			}
			return tw.Flush()
		},
	}

	var caps, sendTo []string
	var observer bool
	var rate float64
	var burst int
	add := &cobra.Command{
		Use: "add ID", Short: "Add an agent and create its token file", Args: cobra.ExactArgs(1),
		Long: `Add an agent to the config and write its token to tokens/ID.token beside
the config. On a terminal it then prints where the token is and how to
start the agent, on this machine and on another host ("agentbus config
agent setup ID" prints that again later). Piped, it prints the agent's
AGENTBUS_* exports on stdout instead.`,
		Example: `  agentbus config agent add researcher --capability research --capability summarize
  agentbus config agent add cli-ops --send-to researcher --send-to conduit-home
  agentbus config agent add grafana --observer`,
		RunE: func(_ *cobra.Command, args []string) error {
			var file string
			cfg, err := change(a.location(), func(tx *confstore.Tx) (err error) {
				file, err = tx.AddAgent(broker.AgentConfig{
					ID: args[0], Capabilities: caps, SendTo: sendTo, Observer: observer, RatePerSecond: rate, RateBurst: burst,
				})
				return err
			})
			if err != nil {
				return err
			}
			loc := a.location()
			if isTerminal(a.stdout) {
				lipgloss.Fprintf(a.stdout, "%s added %s; a running broker accepts it within a second\n\n%s\n",
					tui.OKStyle.Render("✓"), args[0], tui.RenderGuide(confstore.Guide(loc, cfg, args[0])))
				return nil
			}
			// Piped: the exports on stdout, for scripts; the rest on stderr.
			lipgloss.Fprintf(a.stderr, "%s added %s; token in %s\n  \"agentbus config agent setup %s\" shows how to start it\n",
				tui.OKStyle.Render("✓"), args[0], file, args[0])
			fmt.Fprint(a.stdout, confstore.ClientEnv(confstore.URL(cfg), args[0], file))
			return nil
		},
	}
	add.Flags().StringArrayVar(&caps, "capability", nil, "task queue it may consume (repeatable; * for all)")
	add.Flags().StringArrayVar(&sendTo, "send-to", nil, "agent whose inbox it may message (repeatable; default all)")
	add.Flags().BoolVar(&observer, "observer", false, "may read all traffic and broker status (admin tools only)")
	add.Flags().Float64Var(&rate, "rate", 0, "publish rate limit per second (default: the broker's)")
	add.Flags().IntVar(&burst, "burst", 0, "publish burst (default: the broker's)")

	rm := &cobra.Command{
		Use: "rm ID", Aliases: []string{"remove"}, Short: "Remove an agent and its token file", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if _, err := change(a.location(), func(tx *confstore.Tx) error { return tx.RemoveAgent(args[0]) }); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s removed %s; a running broker disconnects it\n", tui.OKStyle.Render("✓"), args[0])
			return nil
		},
	}
	token := &cobra.Command{
		Use: "token ID", Aliases: []string{"rotate"}, Short: "Rotate an agent's token", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var file string
			_, err := change(a.location(), func(tx *confstore.Tx) (err error) {
				file, err = tx.RotateToken(args[0])
				return err
			})
			if err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s new token for %s in %s\n"+
				"  A running broker disconnects clients using the old token. Clients that read this file\n"+
				"  pick up the new token when they restart; copy it again to any other host that has the old one.\n",
				tui.OKStyle.Render("✓"), args[0], file)
			return nil
		},
	}
	var eCaps, eSendTo []string
	var eObserver bool
	var eRate float64
	var eBurst int
	edit := &cobra.Command{
		Use: "edit ID", Short: "Change an agent's grants and limits", Args: cobra.ExactArgs(1),
		Long: `Change only the flags you pass. An empty value clears a list:
--capability= removes every capability, --send-to= allows sending to anyone.
A running broker applies the change within a second.`,
		Example: `  agentbus config agent edit worker-2 --capability=          # stop serving tasks
  agentbus config agent edit you --send-to worker --send-to researcher
  agentbus config agent edit grafana --observer=false`,
		RunE: func(cmd *cobra.Command, args []string) error {
			loc := a.location()
			var ag broker.AgentConfig
			_, err := change(loc, func(tx *confstore.Tx) error {
				i := confstore.Find(tx.Config, args[0])
				if i < 0 {
					return fmt.Errorf("no agent %q in %s", args[0], loc.Path)
				}
				editAgent(cmd.Flags().Changed, &tx.Config.Agents[i], eCaps, eSendTo, eObserver, eRate, eBurst)
				ag = tx.Config.Agents[i]
				return nil
			})
			if err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s %s: %s; may send to %s; rate %s\n", tui.OKStyle.Render("✓"), ag.ID,
				tui.AgentRole(ag), tui.SendTo(ag), tui.AgentRate(ag))
			return nil
		},
	}
	edit.Flags().StringArrayVar(&eCaps, "capability", nil, "replace its capabilities (repeatable; empty to clear)")
	edit.Flags().StringArrayVar(&eSendTo, "send-to", nil, "replace who it may message (repeatable; empty for anyone)")
	edit.Flags().BoolVar(&eObserver, "observer", false, "set the observer role")
	edit.Flags().Float64Var(&eRate, "rate", 0, "publish rate per second (0 for the broker default)")
	edit.Flags().IntVar(&eBurst, "burst", 0, "publish burst (0 for the broker default)")

	agentIDs := func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		cfg, err := confstore.Load(a.location().Path)
		if err != nil || len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var ids []string
		for _, ag := range cfg.Agents {
			ids = append(ids, ag.ID)
		}
		return ids, cobra.ShellCompDirectiveNoFileComp
	}
	setup := &cobra.Command{
		Use: "setup ID", Short: "Show where an agent's token is and how to start it", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			if confstore.Find(cfg, args[0]) < 0 {
				return fmt.Errorf("no agent %q in %s", args[0], loc.Path)
			}
			guide := confstore.Guide(loc, cfg, args[0])
			if isTerminal(a.stdout) {
				guide = tui.RenderGuide(guide) + "\n"
			}
			lipgloss.Fprint(a.stdout, guide)
			return nil
		},
	}
	rm.ValidArgsFunction, token.ValidArgsFunction, edit.ValidArgsFunction, setup.ValidArgsFunction = agentIDs, agentIDs, agentIDs, agentIDs
	c.AddCommand(list, add, setup, edit, rm, token)
	return c
}

// editAgent applies the "config agent edit" flags that were given.
func editAgent(changed func(flag string) bool, ag *broker.AgentConfig, caps, sendTo []string, observer bool, rate float64, burst int) {
	nonEmpty := func(v []string) []string {
		var out []string
		for _, s := range v {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if changed("capability") {
		ag.Capabilities = nonEmpty(caps)
	}
	if changed("send-to") {
		ag.SendTo = nonEmpty(sendTo)
	}
	if changed("observer") {
		ag.Observer = observer
	}
	if changed("rate") {
		ag.RatePerSecond = rate
	}
	if changed("burst") {
		ag.RateBurst = burst
	}
}

func newConfigEnvCmd(a *app) *cobra.Command {
	var format string
	c := &cobra.Command{
		Use:   "env ID",
		Short: "Print client setup for an agent",
		Long:  "Print how to connect as an agent: shell exports (default), Go code, or MQTT Explorer settings.",
		Example: `  eval "$(agentbus config env researcher)"
  agentbus config env researcher --format go`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			if confstore.Find(cfg, args[0]) < 0 {
				return fmt.Errorf("no agent %q in %s", args[0], loc.Path)
			}
			url, file := confstore.URL(cfg), loc.TokenFile(args[0])
			switch format {
			case "shell":
				fmt.Fprint(a.stdout, confstore.ClientEnv(url, args[0], file))
			case "go":
				fmt.Fprintln(a.stdout, confstore.GoSnippet(url, args[0], file))
			case "mqtt":
				fmt.Fprint(a.stdout, confstore.MQTTSettings(url, args[0], file))
			default:
				return fmt.Errorf("--format must be shell, go or mqtt")
			}
			return nil
		},
	}
	c.Flags().StringVar(&format, "format", "shell", "shell, go or mqtt (desktop clients like MQTT Explorer)")
	_ = c.RegisterFlagCompletionFunc("format", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"shell", "go", "mqtt"}, cobra.ShellCompDirectiveNoFileComp
	})
	return c
}
