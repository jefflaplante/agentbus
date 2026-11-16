package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/confstore"
	"github.com/jefflaplante/agentbus/internal/tui"
)

// runTUI runs a full-screen Bubble Tea program on the app's terminal.
func (a *app) runTUI(ctx context.Context, m tea.Model) error {
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(a.stdin), tea.WithOutput(a.stdout))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil // interrupted
	}
	return err
}

// configFunc opens the config editor for the console. It's only offered
// when the local config file is the one for the broker we're connected to
// (same URL), so the console never edits some other broker.
func (a *app) configFunc() func() (*tui.ConfigModel, error) {
	loc := a.location()
	cfg, err := confstore.Load(loc.Path)
	if err != nil || confstore.URL(cfg) != a.url {
		return nil
	}
	return func() (*tui.ConfigModel, error) { return tui.NewConfigEditor(loc, "") }
}

// lineEncoder writes JSON lines. Handlers for different subscriptions
// run concurrently, so writes are serialized to keep lines whole.
type lineEncoder struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newLineEncoder(w io.Writer) *lineEncoder { return &lineEncoder{enc: json.NewEncoder(w)} }

func (l *lineEncoder) Encode(v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.enc.Encode(v)
}

// feed is a bounded channel of items for a TUI; it drops rather than
// blocking the MQTT client when the UI falls behind.
type feed chan tui.Item

func (f feed) push(it tui.Item) {
	select {
	case f <- it:
	default:
	}
}

func newAgentsCmd(a *app) *cobra.Command {
	var once bool
	c := &cobra.Command{
		Use:   "agents",
		Short: "List agents and their presence",
		Long: `Show every agent's presence card. On a terminal this opens the console
on its Agents tab (see agentbus console). With --once, --plain or when
piped, print the table once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.interactive() && !once {
				return a.runConsole(cmd.Context(), tui.TabAgents)
			}
			c, err := a.connect(cmd.Context(), false, nil)
			if err != nil {
				return err
			}
			defer c.Close()
			time.Sleep(500 * time.Millisecond) // let retained presence cards arrive
			tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "AGENT\tSTATUS\tSINCE\tCAPABILITIES\tKEYS\tDESCRIPTION")
			for _, row := range tui.AgentRows(c.Agents()) {
				fmt.Fprintln(tw, strings.Join(row, "\t"))
			}
			return tw.Flush()
		},
	}
	c.Flags().BoolVar(&once, "once", false, "print the table once instead of opening the console")
	return c
}

func newWatchCmd(a *app) *cobra.Command {
	var filter string
	c := &cobra.Command{
		Use:   "watch [--filter FILTER]",
		Short: "Watch broadcast events (or, for observers, any traffic)",
		Long: `Watch messages matching an MQTT filter. Any agent can watch events
(a2a/v1/events/...). An agent configured as an observer can watch any
filter, such as "a2a/v1/#" for all agent traffic.

On a terminal this is a live feed (q quit, p pause, c clear). Piped or
with --plain, each message is one JSON line: the envelope for event
filters, or {"topic":...,"payload":...} for other filters.`,
		Example: `  agentbus watch
  agentbus watch --filter 'a2a/v1/events/+/jobs/#'
  agentbus watch --filter 'a2a/v1/#'     # observers only`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tuiMode := a.interactive()
			c, err := a.connect(cmd.Context(), tuiMode, nil)
			if err != nil {
				return err
			}
			defer c.Close()
			ctx := cmd.Context()
			items := make(feed, 1024)
			enc := newLineEncoder(a.stdout)

			t, perr := a2a.ParseTopic(filter)
			if perr == nil && t.Kind == a2a.KindEvent {
				err = c.WatchEvents(ctx, filter, func(e *a2a.Envelope) {
					if tuiMode {
						items.push(tui.ItemFromEnvelope(a2a.EventTopic(e.From, ""), e))
					} else {
						_ = enc.Encode(e)
					}
				})
			} else {
				err = c.Observe(ctx, filter, func(topic string, payload []byte) {
					if tuiMode {
						items.push(tui.ItemFromMessage(topic, payload))
						return
					}
					_ = enc.Encode(rawLine(topic, payload))
				})
			}
			if err != nil {
				return fmt.Errorf("watch %s: %w (filters outside a2a/v1/events need an observer identity)", filter, err)
			}
			if tuiMode {
				return a.runTUI(ctx, tui.NewFeed("agentbus · watch "+filter, items))
			}
			<-ctx.Done()
			return nil
		},
	}
	c.Flags().StringVarP(&filter, "filter", "f", a2a.Root+"/events/#", "MQTT topic filter")
	return c
}

// rawLine is the JSON-lines form of a message seen through Observe.
func rawLine(topic string, payload []byte) map[string]any {
	line := map[string]any{"topic": topic}
	if json.Valid(payload) {
		line["payload"] = json.RawMessage(payload)
	} else {
		line["payload"] = string(payload)
	}
	return line
}

func newListenCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "listen",
		Short: "Show messages arriving in this agent's inbox",
		Long: `Show messages delivered to this agent's inbox. Requests are answered
with an error, since a listener can't handle them; use handle for that.
The session is persistent, so messages sent while it wasn't running
arrive when it starts.

On a terminal this is a live feed; piped or with --plain, JSON lines.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			tuiMode := a.interactive()
			items := make(feed, 1024)
			enc := newLineEncoder(a.stdout)
			c, err := a.connect(cmd.Context(), tuiMode, func(o *client.Options) {
				o.CleanSession = false
				o.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) {
					if tuiMode {
						items.push(tui.ItemFromEnvelope(a2a.InboxTopic(o.AgentID), e))
					} else {
						_ = enc.Encode(e)
					}
					if e.Type == a2a.TypeRequest {
						return nil, errors.New("listener does not answer requests")
					}
					return nil, nil
				}
			})
			if err != nil {
				return err
			}
			defer c.Close()
			if tuiMode {
				return a.runTUI(cmd.Context(), tui.NewFeed("agentbus · inbox of "+a.agent, items))
			}
			<-cmd.Context().Done()
			return nil
		},
	}
	return c
}

func newConsoleCmd(a *app) *cobra.Command {
	var tab string
	c := &cobra.Command{
		Use:     "console",
		Aliases: []string{"ui", "dashboard"},
		Short:   "Manage the bus from one full-screen console",
		Long: `A full-screen console for the whole bus, one tab each for:

  1 Overview  broker status, throughput and request outcomes
  2 Agents    every agent, its broker state and presence card
  3 Traffic   live messages, filtered by agent, type or text
  4 Compose   send requests, tasks, events and broadcasts, and see replies
  5 Inbox     messages to this agent; answer or refuse requests
  6 Config    agents, tokens, client setup and broker settings

Connected as an observer (admin is one) it sees all traffic and the
broker's status; otherwise events and its own inbox. Config is
available when the broker's config file is on this machine. Press ? in
the console for every key. Needs a terminal.`,
		Example: `  agentbus console
  agentbus console --tab compose --agent you`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !a.interactive() {
				return errors.New("console needs a terminal; for scripts use status --plain, watch --plain or request")
			}
			start, err := parseTab(tab)
			if err != nil {
				return err
			}
			return a.runConsole(cmd.Context(), start)
		},
	}
	c.Flags().StringVar(&tab, "tab", "overview", "tab to start on: overview, agents, traffic, compose, inbox, config or map")
	return c
}

func parseTab(s string) (tui.Tab, error) {
	for i, name := range []string{"overview", "agents", "traffic", "compose", "inbox", "config", "map"} {
		if strings.EqualFold(s, name) || s == fmt.Sprint(i+1) {
			return tui.Tab(i), nil
		}
	}
	return 0, fmt.Errorf("--tab %q: use overview, agents, traffic, compose, inbox, config or map", s)
}

// runConsole connects and runs the console, starting on tab start.
func (a *app) runConsole(ctx context.Context, start tui.Tab) error {
	items := make(feed, 4096)
	var observing atomic.Bool
	var inbox *tui.Answerer
	c, err := a.connect(ctx, true, func(o *client.Options) {
		inbox = tui.NewAnswerer(o.AgentID)
		o.Description = "agentbus console"
		o.Inbox = func(hctx context.Context, e *a2a.Envelope) (any, error) {
			if !observing.Load() { // an observer already sees its inbox via Observe
				items.push(tui.ItemFromEnvelope(a2a.InboxTopic(o.AgentID), e))
			}
			return inbox.Handle(hctx, e)
		}
	})
	if err != nil {
		return err
	}
	defer c.Close()

	scope := "all agent traffic (observer)"
	observing.Store(true)
	err = c.Observe(ctx, a2a.Root+"/#", func(topic string, payload []byte) {
		items.push(tui.ItemFromMessage(topic, payload))
	})
	if err != nil {
		observing.Store(false)
		scope = "events and your inbox (not an observer)"
		err = c.WatchEvents(ctx, a2a.Root+"/events/#", func(e *a2a.Envelope) {
			items.push(tui.ItemFromEnvelope(a2a.EventTopic(e.From, ""), e))
		})
		if err != nil {
			return err
		}
	}
	opt := tui.ConsoleOptions{Agent: a.agent, URL: a.url, Scope: scope, Bus: c, Traffic: items,
		Inbox: inbox, Config: a.configFunc(), Start: start}
	for _, v := range a.fromEnv() {
		name, _, _ := strings.Cut(v, "=")
		opt.Env = append(opt.Env, "$"+name)
	}
	if observing.Load() {
		statuses := make(chan broker.Status, 4)
		if err := observeStatus(ctx, c, statuses); err == nil {
			opt.Status = statuses
		}
	}
	return a.runTUI(ctx, tui.NewConsole(opt))
}

// observeStatus subscribes to the broker's status topic and decodes each
// snapshot into out (dropping snapshots if the reader is behind).
func observeStatus(ctx context.Context, c *client.Client, out chan<- broker.Status) error {
	return c.Observe(ctx, broker.StatusTopic, func(_ string, payload []byte) {
		var st broker.Status
		if json.Unmarshal(payload, &st) != nil {
			return
		}
		select {
		case out <- st:
		default:
		}
	})
}

func newStatusCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"top"},
		Short:   "Live view of the running broker",
		Long: `Connect to a running broker and show what it's doing: uptime, listener,
TLS, data_dir and audit settings, limits, traffic and message rates,
accept and reject counts by code, and every configured agent with its
state, address, queued messages and counts.

The broker publishes this every status_interval (default 2s) to
$SYS/agentbus/status, which only observer identities can read; connect
as one (for example the demo's "explorer").

On a terminal this opens the console on its Overview tab (see agentbus
console). Piped or with --plain, print one status snapshot as JSON and
exit.`,
		Example: `  agentbus status --agent explorer --token-file explorer.token
  agentbus status --plain | jq '.decisions'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if a.interactive() {
				return a.runConsole(ctx, tui.TabOverview)
			}
			c, err := a.connect(ctx, false, nil)
			if err != nil {
				return err
			}
			defer c.Close()
			statuses := make(chan broker.Status, 16)
			if err := observeStatus(ctx, c, statuses); err != nil {
				return fmt.Errorf("broker status needs an observer identity (\"observer\": true in the broker config): %w", err)
			}
			// The retained snapshot can be up to status_interval old. Wait
			// for one that shows this agent online: it was taken after we
			// connected, whatever the broker's clock says.
			timeout := time.After(10 * time.Second)
			for {
				select {
				case st := <-statuses:
					if !showsOnline(st, a.agent) {
						continue
					}
					enc := json.NewEncoder(a.stdout)
					if isTerminal(a.stdout) {
						enc.SetIndent("", "  ")
					}
					return enc.Encode(st)
				case <-timeout:
					return errors.New("no broker status within 10s; is this an older broker?")
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		},
	}
}

func showsOnline(st broker.Status, agent string) bool {
	for _, as := range st.Agents {
		if as.ID == agent {
			return as.State == "online"
		}
	}
	return false
}
