// Command agentbus runs the a2a/v1 broker and provides a CLI client for
// agents that don't embed the Go client.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

const usage = `agentbus: MQTT agent-to-agent bus (contract a2a/v1)

Broker:
  agentbus serve -config broker.json
  agentbus token                       print a new token and its sha256 for the config

Client (connection from flags or AGENTBUS_URL, AGENTBUS_AGENT, and
AGENTBUS_TOKEN or AGENTBUS_TOKEN_FILE; prefer the file, -token shows in ps):
  agentbus send    -to AGENT -body JSON              one-way event to an inbox
  agentbus request -to AGENT -body JSON [-timeout]   request/response, prints the reply
  agentbus task    -capability CAP -body JSON        request via a capability work queue
  agentbus emit    -name NAME -body JSON             broadcast on this agent's event topic
  agentbus watch   [-filter a2a/v1/events/#]         print events as JSON lines
  agentbus listen                                    print inbox envelopes as JSON lines
  agentbus handle  -exec CMD [-capability CAP]       answer requests by running CMD
  agentbus agents                                    list agents and presence

"handle" runs CMD with sh -c once per request (-types to include events),
envelope JSON on stdin. stdout (JSON, or text wrapped as a string) is the
response body. A non-zero exit sends a generic error; stderr is logged
locally, not returned. CMD runs without AGENTBUS_TOKEN in its environment
and is killed, with its children, when the request's TTL (or -timeout)
runs out. Output over 1 MiB is an error.

send, request, task and emit accept -priority (high|normal|low|info) and
-event NAME.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, log, args)
	case "token":
		tok, hash := broker.NewToken()
		fmt.Printf("token:        %s\ntoken_sha256: %s\n", tok, hash)
	case "send", "request", "task", "emit", "watch", "listen", "handle", "agents":
		err = runClient(ctx, log, cmd, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentbus:", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "broker.json", "broker config file")
	_ = fs.Parse(args)

	cfg, err := broker.LoadConfig(*path)
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
	log.Info("agentbus broker listening", "addr", b.Addr(), "agents", len(cfg.Agents), "tls", cfg.TLSCert != "")
	<-ctx.Done()
	return b.Close()
}

func runClient(ctx context.Context, log *slog.Logger, cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	url := fs.String("url", env("AGENTBUS_URL", "tcp://localhost:1883"), "broker URL")
	agent := fs.String("agent", os.Getenv("AGENTBUS_AGENT"), "this agent's ID")
	token := fs.String("token", os.Getenv("AGENTBUS_TOKEN"), "this agent's token (visible in ps; prefer -token-file)")
	tokenFile := fs.String("token-file", os.Getenv("AGENTBUS_TOKEN_FILE"), "file containing this agent's token")
	priority := fs.String("priority", "", "message priority: high, normal, low or info")
	event := fs.String("event", "", "event name carried on the message")
	types := fs.String("types", "request", "handle: comma-separated envelope types to run CMD for (request,event)")
	to := fs.String("to", "", "recipient agent (send, request)")
	capability := fs.String("capability", "", "capability (task, handle)")
	name := fs.String("name", "", "event name (emit)")
	body := fs.String("body", "", "JSON body; '-' reads stdin")
	filter := fs.String("filter", a2a.Root+"/events/#", "event filter (watch)")
	execCmd := fs.String("exec", "", "command to run per message (handle)")
	timeout := fs.Duration("timeout", 60*time.Second, "request timeout (request, task)")
	_ = fs.Parse(args)

	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read token file: %w", err)
		}
		*token = strings.TrimSpace(string(b))
	}
	if *agent == "" || *token == "" {
		return errors.New("agent ID and token are required (-agent/-token or AGENTBUS_AGENT/AGENTBUS_TOKEN)")
	}
	raw, err := readBody(*body)
	if err != nil {
		return err
	}

	var msgOpts []client.MsgOption
	if *priority != "" {
		msgOpts = append(msgOpts, client.WithPriority(a2a.Priority(*priority)))
	}
	if *event != "" {
		msgOpts = append(msgOpts, client.WithEvent(*event))
	}
	handleTypes := map[a2a.Type]bool{}
	for _, t := range strings.Split(*types, ",") {
		handleTypes[a2a.Type(strings.TrimSpace(t))] = true
	}

	opts := client.Options{BrokerURL: *url, AgentID: *agent, Token: *token, Logger: log}
	// One-shot commands use a clean session so they don't leave queued
	// state behind on the broker. Long-running ones keep a persistent inbox.
	opts.CleanSession = cmd != "listen" && cmd != "handle"
	out := json.NewEncoder(os.Stdout)

	switch cmd {
	case "listen":
		opts.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) {
			_ = out.Encode(e)
			if e.Type == a2a.TypeRequest {
				return nil, errors.New("listener does not answer requests")
			}
			return nil, nil
		}
	case "handle":
		if *execCmd == "" {
			return errors.New("handle needs -exec")
		}
		h := execHandler(*execCmd, *timeout, handleTypes, log)
		if *capability == "" {
			opts.Inbox = h
		} else {
			// Registered before connecting so tasks queued while this
			// worker was offline have a handler when they're redelivered.
			opts.Tasks = map[string]client.Handler{*capability: h}
		}
	}

	connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, err := client.Connect(connCtx, opts)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close()

	switch cmd {
	case "send":
		return c.Send(ctx, required(*to, "-to"), raw, msgOpts...)
	case "emit":
		return c.Emit(ctx, required(*name, "-name"), raw, msgOpts...)
	case "request", "task":
		rctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		var reply *a2a.Envelope
		if cmd == "request" {
			reply, err = c.Request(rctx, required(*to, "-to"), raw, msgOpts...)
		} else {
			reply, err = c.RequestTask(rctx, required(*capability, "-capability"), raw, msgOpts...)
		}
		if reply != nil {
			_ = out.Encode(reply)
		}
		return err
	case "watch":
		if err := c.WatchEvents(ctx, *filter, func(e *a2a.Envelope) { _ = out.Encode(e) }); err != nil {
			return err
		}
	case "handle":
		log.Info("handling", "agent", *agent, "capability", *capability, "exec", *execCmd)
	case "agents":
		time.Sleep(500 * time.Millisecond) // let retained presence cards arrive
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "AGENT\tSTATUS\tSINCE\tCAPABILITIES\tDESCRIPTION")
		for _, p := range c.Agents() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Agent, p.Status, p.TS.Local().Format(time.DateTime),
				strings.Join(p.Capabilities, ","), p.Description)
		}
		return tw.Flush()
	}
	<-ctx.Done() // listen, watch, handle run until interrupted
	return nil
}

// maxExecOutput caps what a handler command may write to stdout or stderr.
const maxExecOutput = 1 << 20

// execHandler runs command once per envelope of the given types; see usage.
func execHandler(command string, timeout time.Duration, types map[a2a.Type]bool, log *slog.Logger) client.Handler {
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		if !types[e.Type] {
			if e.Type == a2a.TypeError {
				log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
			}
			return nil, nil
		}
		in, err := e.Marshal()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = scrubEnv(os.Environ())
		// Own process group, so a timeout kills the command's children too.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		stdout := &capBuffer{max: maxExecOutput}
		stderr := &capBuffer{max: maxExecOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		runErr := cmd.Run()
		if stdout.over || stderr.over {
			log.Warn("handler output too large", "id", e.ID, "max", maxExecOutput)
			return nil, errors.New("handler output too large")
		}
		if runErr != nil {
			// The details stay local: stderr can hold paths, secrets or
			// stack traces that the requesting agent shouldn't see.
			log.Warn("handler command failed", "id", e.ID, "from", e.From, "err", runErr,
				"stderr", strings.TrimSpace(stderr.String()))
			if ctx.Err() != nil {
				return nil, errors.New("handler timed out")
			}
			return nil, errors.New("handler failed")
		}
		outb := bytes.TrimSpace(stdout.Bytes())
		if len(outb) == 0 {
			return nil, nil
		}
		if json.Valid(outb) {
			return json.RawMessage(outb), nil
		}
		return string(outb), nil
	}
}

// scrubEnv drops agentbus credentials from a child's environment.
func scrubEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTBUS_TOKEN=") || strings.HasPrefix(kv, "AGENTBUS_TOKEN_FILE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// capBuffer collects output up to max bytes. It wraps bytes.Buffer
// instead of embedding it: an embedded Buffer's ReadFrom would let
// io.Copy bypass the cap in Write.
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil // keep draining so the child doesn't block
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *capBuffer) String() string { return b.buf.String() }

func readBody(s string) (json.RawMessage, error) {
	var b []byte
	switch s {
	case "":
		return nil, nil
	case "-":
		var err error
		if b, err = io.ReadAll(os.Stdin); err != nil {
			return nil, err
		}
	default:
		b = []byte(s)
	}
	b = bytes.TrimSpace(b)
	if !json.Valid(b) {
		return nil, errors.New("-body is not valid JSON")
	}
	return json.RawMessage(b), nil
}

func required(v, flagName string) string {
	if v == "" {
		fmt.Fprintf(os.Stderr, "agentbus: %s is required\n", flagName)
		os.Exit(2)
	}
	return v
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
