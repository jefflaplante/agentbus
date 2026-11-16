package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/tui"
)

// exitError ends the command with a status code, its message already shown.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func newSendCmd(a *app) *cobra.Command {
	var f msgFlags
	var to string
	c := &cobra.Command{
		Use:     "send --to AGENT --body JSON",
		Short:   "Send a one-way event to an agent's inbox",
		Example: `  agentbus send --to worker --body '{"kind":"hello","text":"hi"}' --event greeting`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.oneWay(cmd.Context(), &f, "event to "+to, func(c *client.Client, body json.RawMessage, opts []client.MsgOption) error {
				return c.Send(cmd.Context(), to, body, opts...)
			})
		},
	}
	c.Flags().StringVarP(&to, "to", "t", "", "recipient agent ID")
	_ = c.MarkFlagRequired("to")
	f.register(c)
	return c
}

func newEmitCmd(a *app) *cobra.Command {
	var f msgFlags
	var name string
	c := &cobra.Command{
		Use:     "emit --name NAME --body JSON",
		Short:   "Broadcast an event on this agent's event topic",
		Example: `  agentbus emit --name jobs/finished --body '{"job":42}'`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.oneWay(cmd.Context(), &f, "event "+name, func(c *client.Client, body json.RawMessage, opts []client.MsgOption) error {
				return c.Emit(cmd.Context(), name, body, opts...)
			})
		},
	}
	c.Flags().StringVarP(&name, "name", "n", "", "event name, may contain '/'")
	_ = c.MarkFlagRequired("name")
	f.register(c)
	return c
}

// oneWay connects, publishes once and reports.
func (a *app) oneWay(ctx context.Context, f *msgFlags, what string, publish func(*client.Client, json.RawMessage, []client.MsgOption) error) error {
	body, err := readBody(f.body, a.stdin)
	if err != nil {
		return err
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	c, err := a.connect(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := publish(c, body, opts); err != nil {
		return err
	}
	if isTerminal(a.stderr) && !a.plain {
		lipgloss.Fprintln(a.stderr, tui.OKStyle.Render("✓")+" sent "+what)
	}
	return nil
}

func newRequestCmd(a *app) *cobra.Command {
	var f msgFlags
	var to string
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "request --to AGENT --body JSON",
		Short: "Send a request to an agent and print the reply",
		Long: `Send a request to an agent's inbox and wait for its reply. On a terminal
a spinner shows while waiting and the reply body is pretty-printed;
piped, the whole reply envelope is printed as one JSON line. An error
reply exits with status 1.`,
		Example: `  agentbus request --to worker --body '{"op":"ping"}'
  agentbus request --to worker --body @question.json --timeout 2m`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.roundTrip(cmd.Context(), &f, timeout, "waiting for "+to, func(ctx context.Context, c *client.Client, body json.RawMessage, opts []client.MsgOption) (*a2a.Envelope, error) {
				return c.Request(ctx, to, body, opts...)
			})
		},
	}
	c.Flags().StringVarP(&to, "to", "t", "", "recipient agent ID")
	_ = c.MarkFlagRequired("to")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long to wait for the reply (also the request's TTL)")
	f.register(c)
	return c
}

func newTaskCmd(a *app) *cobra.Command {
	var f msgFlags
	var capability string
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "task --capability CAP --body JSON",
		Short: "Send a request to a capability's work queue and print the reply",
		Long: `Send a request to the work queue for a capability. Exactly one connected
worker handles it; if every worker is offline it waits for one. Output
is the same as request.`,
		Example: `  agentbus task --capability textstats --body '{"text":"to be or not to be"}'`,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.roundTrip(cmd.Context(), &f, timeout, "waiting for a "+capability+" worker", func(ctx context.Context, c *client.Client, body json.RawMessage, opts []client.MsgOption) (*a2a.Envelope, error) {
				return c.RequestTask(ctx, capability, body, opts...)
			})
		},
	}
	c.Flags().StringVarP(&capability, "capability", "c", "", "capability whose queue gets the task")
	_ = c.MarkFlagRequired("capability")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long to wait for the reply (also the request's TTL)")
	f.register(c)
	return c
}

type requestFn func(context.Context, *client.Client, json.RawMessage, []client.MsgOption) (*a2a.Envelope, error)

// roundTrip connects, sends a request, waits (with a spinner on a
// terminal) and prints the reply.
func (a *app) roundTrip(ctx context.Context, f *msgFlags, timeout time.Duration, label string, send requestFn) error {
	body, err := readBody(f.body, a.stdin)
	if err != nil {
		return err
	}
	opts, err := f.options()
	if err != nil {
		return err
	}
	c, err := a.connect(ctx, false, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var reply *a2a.Envelope
	var reqErr error
	start := time.Now()
	if !a.interactive() {
		reply, reqErr = send(rctx, c, body, opts)
		if reply != nil {
			_ = json.NewEncoder(a.stdout).Encode(reply)
		}
		return reqErr
	}

	err = tui.Wait(rctx, a.stderr, a.stdin, label, func(ctx context.Context) error {
		reply, reqErr = send(ctx, c, body, opts)
		return nil
	})
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	if reqErr != nil && reply == nil {
		return reqErr
	}
	var re *client.RemoteError
	if errors.As(reqErr, &re) {
		// Code, sender and reason come from another agent: escape them.
		lipgloss.Fprintf(a.stderr, "%s %s from %s after %s\n  %s\n", tui.ErrStyle.Render("✗"),
			tui.ErrStyle.Render(tui.Clean(re.Code)), tui.AccentStyle.Render(tui.Clean(re.From)), elapsed, tui.Clean(re.Reason))
		return exitError{1}
	}
	lipgloss.Fprintf(a.stderr, "%s %s from %s in %s\n", tui.OKStyle.Render("✓"), tui.Clean(string(reply.Type)),
		tui.AccentStyle.Render(tui.Clean(reply.From)), elapsed)
	printBody(a, reply.Body)
	return nil
}

// printBody pretty-prints a reply body to the terminal. The body is
// another agent's, so control characters (escape sequences) are shown
// escaped, never sent to the terminal; newlines and tabs are kept.
func printBody(a *app, body json.RawMessage) {
	if len(body) == 0 {
		return
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		fmt.Fprintln(a.stdout, tui.CleanBlock(string(body)))
		return
	}
	if s, ok := v.(string); ok {
		fmt.Fprintln(a.stdout, tui.CleanBlock(s))
		return
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Fprintln(a.stdout, tui.CleanBlock(string(out)))
}

// handleTypes parses handle --types.
func handleTypes(s string) (map[a2a.Type]bool, error) {
	sel := map[a2a.Type]bool{}
	for _, t := range strings.Split(s, ",") {
		switch typ := a2a.Type(strings.TrimSpace(t)); typ {
		case a2a.TypeRequest, a2a.TypeEvent:
			sel[typ] = true
		default:
			return nil, fmt.Errorf("--types: unknown type %q; use request, event or request,event", strings.TrimSpace(t))
		}
	}
	return sel, nil
}

func newHandleCmd(a *app) *cobra.Command {
	var execCmd, capability, types string
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "handle --exec CMD [--capability CAP]",
		Short: "Answer requests by running a command per message",
		Long: `Run CMD with sh -c once per request (--types to include events), with the
envelope JSON on stdin. Its stdout (JSON, or text wrapped as a string)
is the response body. To refuse with a specific error, exit 3 and print
{"code": "...", "reason": "..."}; any other non-zero exit sends a generic
error. stderr is logged locally, not returned. CMD runs without
AGENTBUS_TOKEN in its environment. It gets AGENTBUS_FROM (the sender,
checked by the broker), AGENTBUS_TYPE, AGENTBUS_ID, AGENTBUS_OP (body.op),
AGENTBUS_BODY (bodies up to 32 KiB), and AGENTBUS_PREAMBLE: text to put in
front of the envelope if an LLM will read it. CMD is killed, with its children,
when the request's TTL (or --timeout) runs out. Output over 1 MiB is an error.

With --capability, consume that capability's task queue; otherwise
answer this agent's inbox. The session is persistent, so work queued
while the handler was down is delivered when it starts.`,
		Example: `  agentbus handle --capability summarize --exec ./summarize.sh
  agentbus handle --exec 'jq -c "{echo: .body}"'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := handleTypes(types)
			if err != nil {
				return err
			}
			if timeout <= 0 {
				return fmt.Errorf("--timeout must be positive, not %s", timeout)
			}
			log := a.logger(false)
			h := execHandler(execCmd, timeout, sel, log)
			c, err := a.connect(cmd.Context(), false, func(o *client.Options) {
				o.CleanSession = false
				if capability == "" {
					o.Inbox = h
				} else {
					// Registered before connecting so tasks queued while this
					// worker was offline have a handler when redelivered.
					o.Tasks = map[string]client.Handler{capability: h}
				}
			})
			if err != nil {
				return err
			}
			defer c.Close()
			target := "inbox"
			if capability != "" {
				target = "capability " + capability
			}
			lipgloss.Fprintf(a.stderr, "%s handling %s as %s with %q; ctrl+c to stop\n", tui.OKStyle.Render("●"), target, a.agent, execCmd)
			<-cmd.Context().Done()
			return nil
		},
	}
	c.Flags().StringVarP(&execCmd, "exec", "e", "", "command to run per message (sh -c)")
	_ = c.MarkFlagRequired("exec")
	c.Flags().StringVarP(&capability, "capability", "c", "", "serve this capability's task queue instead of the inbox")
	c.Flags().StringVar(&types, "types", "request", "comma-separated envelope types to run CMD for (request,event)")
	c.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "longest a single run of CMD may take")
	return c
}
