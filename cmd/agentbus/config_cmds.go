package main

import (
	"errors"
	"fmt"
	"os"
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

The subcommands do the same from scripts. The file is --config, else
$AGENTBUS_CONFIG, else ./broker.json, else ~/.config/agentbus/broker.json.`,
		Example: `  agentbus config                                  # interactive editor
  agentbus config agent add researcher --capability research --send-to conduit-home
  agentbus config env researcher >> ~/.researcher.env
  agentbus config set rate_per_second 50`,
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
		newConfigAgentCmd(a), newConfigEnvCmd(a))
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
			if !loc.Exists() {
				exists = "does not exist yet"
			}
			fmt.Fprintf(a.stdout, "config  %s (%s, %s)\ntokens  %s\nstate   %s\n", loc.Path, loc.Source, exists,
				loc.TokensDir(), confstore.StateDir())
			return nil
		},
	}
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
		Use:   "set KEY VALUE",
		Short: "Change a broker setting",
		Long: "Change one top-level broker setting. VALUE is JSON (50, true, \"10m\") or a bare string.\n\nKeys: " +
			strings.Join(settingKeys(), ", "),
		Example: `  agentbus config set rate_per_second 50
  agentbus config set session_expiry 72h
  agentbus config set listen 0.0.0.0:8883`,
		Args: cobra.ExactArgs(2),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return settingKeys(), cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			if err := confstore.Set(cfg, args[0], args[1]); err != nil {
				return err
			}
			if err := confstore.Save(loc.Path, cfg); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s %s = %s\n", tui.OKStyle.Render("✓"), args[0], args[1])
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
		Example: `  agentbus config agent add researcher --capability research --capability summarize
  agentbus config agent add cli-ops --send-to researcher --send-to conduit-home
  agentbus config agent add grafana --observer`,
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			file, err := confstore.AddAgent(cfg, loc, broker.AgentConfig{
				ID: args[0], Capabilities: caps, SendTo: sendTo, Observer: observer, RatePerSecond: rate, RateBurst: burst,
			})
			if err != nil {
				return err
			}
			if err := confstore.Save(loc.Path, cfg); err != nil {
				_ = os.Remove(file)
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s added %s; token in %s\n\nConnect as it with:\n", tui.OKStyle.Render("✓"), args[0], file)
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
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			if err := confstore.RemoveAgent(cfg, loc, args[0]); err != nil {
				return err
			}
			if err := confstore.Save(loc.Path, cfg); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s removed %s; a running broker disconnects it\n", tui.OKStyle.Render("✓"), args[0])
			return nil
		},
	}
	token := &cobra.Command{
		Use: "token ID", Aliases: []string{"rotate"}, Short: "Rotate an agent's token", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			loc := a.location()
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			file, err := confstore.RotateToken(cfg, loc, args[0])
			if err != nil {
				return err
			}
			if err := confstore.Save(loc.Path, cfg); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s new token for %s in %s; a running broker disconnects the old one\n",
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
			cfg, err := confstore.Load(loc.Path)
			if err != nil {
				return err
			}
			i := confstore.Find(cfg, args[0])
			if i < 0 {
				return fmt.Errorf("no agent %q in %s", args[0], loc.Path)
			}
			ag := &cfg.Agents[i]
			nonEmpty := func(v []string) []string {
				var out []string
				for _, s := range v {
					if s != "" {
						out = append(out, s)
					}
				}
				return out
			}
			f := cmd.Flags()
			if f.Changed("capability") {
				ag.Capabilities = nonEmpty(eCaps)
			}
			if f.Changed("send-to") {
				ag.SendTo = nonEmpty(eSendTo)
			}
			if f.Changed("observer") {
				ag.Observer = eObserver
			}
			if f.Changed("rate") {
				ag.RatePerSecond = eRate
			}
			if f.Changed("burst") {
				ag.RateBurst = eBurst
			}
			if err := confstore.Save(loc.Path, cfg); err != nil {
				return err
			}
			lipgloss.Fprintf(a.stderr, "%s %s: %s; may send to %s; rate %s\n", tui.OKStyle.Render("✓"), ag.ID,
				tui.AgentRole(*ag), tui.SendTo(*ag), tui.AgentRate(*ag))
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
	rm.ValidArgsFunction, token.ValidArgsFunction, edit.ValidArgsFunction = agentIDs, agentIDs, agentIDs
	c.AddCommand(list, add, edit, rm, token)
	return c
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
