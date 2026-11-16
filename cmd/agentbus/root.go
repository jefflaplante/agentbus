package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

// app holds the flags every client command shares.
type app struct {
	url, agent, token, tokenFile string
	configFlag                   string
	plain                        bool
	verbose                      bool

	// changed reports whether a persistent flag was given on the command
	// line, rather than taken from the environment or left unset.
	changed func(flag string) bool
	// from says where each connection setting came from ("url", "agent",
	// "token"), filled in by resolveConnection for error messages.
	from     map[string]string
	envNoted sync.Once

	stdin          io.Reader
	stdout, stderr io.Writer
}

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:   "agentbus",
		Short: "MQTT agent-to-agent bus: broker and client (contract a2a/v1)",
		Long: `agentbus runs the a2a/v1 broker and is a client for agents that don't
embed the Go library.

Start a broker with no setup at all: "agentbus serve" creates
~/.config/agentbus/broker.json with an admin identity the first time, and
"agentbus console" on the same machine finds it. Every command reads that
file unless --config or $AGENTBUS_CONFIG names another.
$AGENTBUS_CONFIG_DIR moves the whole directory, config and tokens/, for a
server (/etc/agentbus) or a second broker; $AGENTBUS_STATE_DIR moves the
broker's data and audit log (/var/lib/agentbus). "agentbus config" adds agents and edits settings; a
running broker applies the changes live.

Client commands connect as one agent. Set the connection with flags or
AGENTBUS_URL, AGENTBUS_AGENT and AGENTBUS_TOKEN_FILE (or AGENTBUS_TOKEN,
which is visible in ps). Unset, they come from the local broker config:
its listen address, the token file for --agent, and "admin" as the
agent when its token is there. "agentbus console" manages everything from
one full-screen view: status, agents, traffic, sending and answering
messages, and config. On a terminal, status, agents, watch, listen, request
and task are interactive too. Piped, or with --plain, they print JSON or
plain text for scripts.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)

	pf := root.PersistentFlags()
	pf.StringVar(&a.url, "url", os.Getenv("AGENTBUS_URL"), "broker URL (env AGENTBUS_URL; default from the local config, else tcp://localhost:1883)")
	pf.StringVar(&a.configFlag, "config", "", "broker config file (env AGENTBUS_CONFIG; default broker.json in $AGENTBUS_CONFIG_DIR, else ~/.config/agentbus)")
	pf.StringVar(&a.agent, "agent", os.Getenv("AGENTBUS_AGENT"), "this agent's ID (env AGENTBUS_AGENT)")
	pf.StringVar(&a.token, "token", os.Getenv("AGENTBUS_TOKEN"), "this agent's token; visible in ps, prefer --token-file (env AGENTBUS_TOKEN)")
	pf.StringVar(&a.tokenFile, "token-file", os.Getenv("AGENTBUS_TOKEN_FILE"), "file containing this agent's token (env AGENTBUS_TOKEN_FILE)")
	a.changed = pf.Changed
	pf.BoolVar(&a.plain, "plain", false, "never use the interactive UI, even on a terminal")
	pf.BoolVarP(&a.verbose, "verbose", "v", false, "log client activity to stderr")

	root.AddGroup(
		&cobra.Group{ID: "broker", Title: "Broker:"},
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "msg", Title: "Messaging:"},
		&cobra.Group{ID: "observe", Title: "Observing:"},
		&cobra.Group{ID: "agents", Title: "Standalone agents:"},
	)
	agentCmd := newAgentCmd(a)
	agentCmd.GroupID = "agents"
	root.AddCommand(agentCmd)
	for _, c := range []*cobra.Command{newServeCmd(a), newTokenCmd(a)} {
		c.GroupID = "broker"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newConfigCmd(a), newJoinCmd(a)} {
		c.GroupID = "setup"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newSendCmd(a), newRequestCmd(a), newTaskCmd(a), newEmitCmd(a), newHandleCmd(a), newMCPCmd(a)} {
		c.GroupID = "msg"
		root.AddCommand(c)
	}
	for _, c := range []*cobra.Command{newStatusCmd(a), newAgentsCmd(a), newWatchCmd(a), newListenCmd(a), newConsoleCmd(a)} {
		c.GroupID = "observe"
		root.AddCommand(c)
	}
	return root
}

// interactive reports whether to use the Bubble Tea UI: a terminal on
// both ends and no --plain.
func (a *app) interactive() bool {
	return !a.plain && isTerminal(a.stdout) && isTerminal(a.stdin)
}

func isTerminal(v any) bool {
	f, ok := v.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// logger returns the client library's logger. Interactive views own the
// screen, so logs go to stderr only when asked for or when piped.
func (a *app) logger(tui bool) *slog.Logger {
	if tui && !a.verbose {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	level := slog.LevelWarn
	if a.verbose {
		level = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level}))
}

// location is the broker config this command would use.
func (a *app) location() confstore.Location {
	a.noteEnv()
	return confstore.Resolve(a.configFlag)
}

// envSettings are the AGENTBUS_* variables and the flags that override them.
var envSettings = []struct{ env, flag string }{
	{"AGENTBUS_CONFIG", "config"},
	{"AGENTBUS_CONFIG_DIR", "config"},
	{"AGENTBUS_STATE_DIR", ""},
	{"AGENTBUS_URL", "url"},
	{"AGENTBUS_AGENT", "agent"},
	{"AGENTBUS_TOKEN_FILE", "token-file"},
	{"AGENTBUS_TOKEN", "token"},
}

// fromEnv lists the AGENTBUS_* settings in effect from the environment,
// as NAME=value (a token's value is not shown). A variable is left out
// when its flag overrides it.
func (a *app) fromEnv() []string {
	tokenFlag := a.flagged("token") || a.flagged("token-file")
	var out []string
	for _, e := range envSettings {
		v := os.Getenv(e.env)
		if v == "" || a.flagged(e.flag) || (tokenFlag && strings.HasPrefix(e.env, "AGENTBUS_TOKEN")) {
			continue
		}
		if e.env == "AGENTBUS_TOKEN" {
			v = "(set)"
		}
		out = append(out, e.env+"="+v)
	}
	return out
}

func (a *app) flagged(name string) bool { return a.changed != nil && a.changed(name) }

// noteEnv says once, on a terminal, which settings came from AGENTBUS_*
// variables. A variable left over from another shell session otherwise
// sends every command to a different config or broker without a word.
func (a *app) noteEnv() {
	a.envNoted.Do(func() {
		if vars := a.fromEnv(); len(vars) > 0 && isTerminal(a.stderr) {
			lipgloss.Fprintln(a.stderr, tui.WarnStyle.Render("agentbus: using "+strings.Join(vars, " ")+
				" from the environment"))
		}
	})
}

// source says where a setting came from: its flag, its variable, or def.
func (a *app) source(flag, env, def string) string {
	switch {
	case a.flagged(flag):
		return "--" + flag
	case os.Getenv(env) != "":
		return "$" + env
	}
	return def
}

// resolveConnection fills in unset connection settings from the local
// broker config: the URL from its listen address, and the token file for
// the agent (with "admin" as the agent when only its token is there).
func (a *app) resolveConnection() {
	// A credential named on the command line beats one from the env:
	// --token wins over $AGENTBUS_TOKEN_FILE, --token-file over
	// $AGENTBUS_TOKEN. (Given both flags, the file wins, as before.)
	if a.flagged("token") && !a.flagged("token-file") {
		a.tokenFile = ""
	}
	if a.flagged("token-file") && !a.flagged("token") {
		a.token = ""
	}
	loc := a.location()
	a.from = map[string]string{
		"url":   a.source("url", "AGENTBUS_URL", ""),
		"agent": a.source("agent", "AGENTBUS_AGENT", ""),
		"token": a.source("token-file", "AGENTBUS_TOKEN_FILE", ""),
	}
	if a.token != "" && a.tokenFile == "" {
		a.from["token"] = a.source("token", "AGENTBUS_TOKEN", "")
	}
	inConfig := "config " + loc.Path
	if a.url == "" {
		a.url = "tcp://localhost:1883"
		a.from["url"] = "default; no config at " + loc.Path
		if cfg, err := confstore.Load(loc.Path); err == nil {
			a.url = confstore.URL(cfg)
			a.from["url"] = inConfig
		}
	}
	if a.token != "" || a.tokenFile != "" {
		return
	}
	if a.agent == "" {
		if _, ok := confstore.ReadToken(loc, confstore.AdminID); ok {
			a.agent = confstore.AdminID
			a.from["agent"] = "the admin token beside " + inConfig
		}
	}
	if a.agent != "" {
		if _, ok := confstore.ReadToken(loc, a.agent); ok {
			a.tokenFile = loc.TokenFile(a.agent)
			a.from["token"] = "beside " + inConfig
		}
	}
}

// settings describes the connection settings and where each came from,
// for an error message.
func (a *app) settings() string {
	loc := confstore.Resolve(a.configFlag)
	cfg := loc.Path + " (" + string(loc.Source)
	if !loc.Exists() {
		cfg += ", does not exist"
	}
	cfg += ")"
	token := a.tokenFile
	if token == "" && a.token != "" {
		token = "(given directly)"
	}
	rows := [][3]string{
		{"config", cfg, ""},
		{"url", a.url, a.from["url"]},
		{"agent", a.agent, a.from["agent"]},
		{"token", token, a.from["token"]},
	}
	var b strings.Builder
	for _, r := range rows {
		v := r[1]
		if v == "" {
			v = "(none)"
		}
		if r[2] != "" {
			v += " (" + r[2] + ")"
		}
		fmt.Fprintf(&b, "\n  %-6s %s", r[0], v)
	}
	if len(a.fromEnv()) > 0 {
		b.WriteString("\n  Settings from AGENTBUS_* variables may be left over from an earlier shell session; " +
			"unset them to use " + filepath.Join(confstore.DefaultConfigDir(), "broker.json") + ".")
	}
	return b.String()
}

// credentials resolves the connection settings and returns the token.
func (a *app) credentials() (string, error) {
	a.resolveConnection()
	token := a.token
	if a.tokenFile != "" {
		b, err := os.ReadFile(a.tokenFile)
		if err != nil {
			return "", fmt.Errorf("read token file: %w%s", err, a.settings())
		}
		token = strings.TrimSpace(string(b))
	}
	if a.agent == "" || token == "" {
		return "", fmt.Errorf("agent ID and token are required: --agent and --token-file (or AGENTBUS_AGENT and "+
			"AGENTBUS_TOKEN_FILE). No local token found under %s; run \"agentbus serve\" once to create an admin identity, "+
			"or \"agentbus config agent add NAME\"%s", a.location().TokensDir(), a.settings())
	}
	return token, nil
}

// connect resolves credentials and connects as this agent.
func (a *app) connect(ctx context.Context, tui bool, mutate func(*client.Options)) (*client.Client, error) {
	token, err := a.credentials()
	if err != nil {
		return nil, err
	}
	opts := client.Options{BrokerURL: a.url, AgentID: a.agent, Token: token, Logger: a.logger(tui)}
	// One-shot commands use a clean session so they leave nothing queued;
	// long-running ones (listen, handle) set CleanSession false in mutate.
	opts.CleanSession = true
	if mutate != nil {
		mutate(&opts)
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := client.Connect(cctx, opts)
	if err != nil && ctx.Err() == nil {
		return nil, fmt.Errorf("%w%s", err, a.settings())
	}
	return c, err
}

// msgFlags are the message options shared by send, request, task and emit.
type msgFlags struct {
	body     string
	priority string
	event    string
	headers  []string
}

func (f *msgFlags) register(c *cobra.Command) {
	c.Flags().StringVarP(&f.body, "body", "b", "", "JSON body; '-' reads stdin, '@file' reads a file")
	c.Flags().StringVar(&f.priority, "priority", "", "priority: high, normal, low or info")
	c.Flags().StringVar(&f.event, "event", "", "event name carried on the message")
	c.Flags().StringArrayVarP(&f.headers, "header", "H", nil, "header as key=value (repeatable)")
	_ = c.RegisterFlagCompletionFunc("priority", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"high", "normal", "low", "info"}, cobra.ShellCompDirectiveNoFileComp
	})
}

func (f *msgFlags) options() ([]client.MsgOption, error) {
	var opts []client.MsgOption
	if f.priority != "" {
		opts = append(opts, client.WithPriority(a2a.Priority(f.priority)))
	}
	if f.event != "" {
		opts = append(opts, client.WithEvent(f.event))
	}
	if len(f.headers) > 0 {
		h := map[string]string{}
		for _, kv := range f.headers {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k == "" {
				return nil, fmt.Errorf("--header %q: want key=value", kv)
			}
			h[k] = v
		}
		opts = append(opts, client.WithHeaders(h))
	}
	return opts, nil
}

// readBody parses --body: literal JSON, '-' for stdin, '@path' for a file.
func readBody(s string, stdin io.Reader) (json.RawMessage, error) {
	var b []byte
	switch {
	case s == "":
		return nil, nil
	case s == "-":
		var err error
		if b, err = io.ReadAll(stdin); err != nil {
			return nil, err
		}
	case strings.HasPrefix(s, "@"):
		var err error
		if b, err = os.ReadFile(s[1:]); err != nil {
			return nil, err
		}
	default:
		b = []byte(s)
	}
	b = bytes.TrimSpace(b)
	if !json.Valid(b) {
		return nil, errors.New("--body is not valid JSON")
	}
	return json.RawMessage(b), nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
