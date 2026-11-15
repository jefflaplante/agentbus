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

Client (connection from flags or AGENTBUS_URL, AGENTBUS_AGENT, AGENTBUS_TOKEN):
  agentbus send    -to AGENT -body JSON              one-way event to an inbox
  agentbus request -to AGENT -body JSON [-timeout]   request/response, prints the reply
  agentbus task    -capability CAP -body JSON        request via a capability work queue
  agentbus emit    -name NAME -body JSON             broadcast on this agent's event topic
  agentbus watch   [-filter a2a/v1/events/#]         print events as JSON lines
  agentbus listen                                    print inbox envelopes as JSON lines
  agentbus handle  -exec CMD [-capability CAP]       answer requests by running CMD
  agentbus agents                                    list agents and presence

"handle" runs CMD with sh -c once per message, envelope JSON on stdin.
For requests, stdout (JSON, or text wrapped as a string) is the response
body; a non-zero exit sends an error with stderr as the reason.
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
	token := fs.String("token", os.Getenv("AGENTBUS_TOKEN"), "this agent's token")
	to := fs.String("to", "", "recipient agent (send, request)")
	capability := fs.String("capability", "", "capability (task, handle)")
	name := fs.String("name", "", "event name (emit)")
	body := fs.String("body", "", "JSON body; '-' reads stdin")
	filter := fs.String("filter", a2a.Root+"/events/#", "event filter (watch)")
	execCmd := fs.String("exec", "", "command to run per message (handle)")
	timeout := fs.Duration("timeout", 60*time.Second, "request timeout (request, task)")
	_ = fs.Parse(args)

	if *agent == "" || *token == "" {
		return errors.New("agent ID and token are required (-agent/-token or AGENTBUS_AGENT/AGENTBUS_TOKEN)")
	}
	raw, err := readBody(*body)
	if err != nil {
		return err
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
		if *capability == "" {
			opts.Inbox = execHandler(*execCmd)
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
		return c.Send(ctx, required(*to, "-to"), raw)
	case "emit":
		return c.Emit(ctx, required(*name, "-name"), raw)
	case "request", "task":
		rctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		var reply *a2a.Envelope
		if cmd == "request" {
			reply, err = c.Request(rctx, required(*to, "-to"), raw)
		} else {
			reply, err = c.RequestTask(rctx, required(*capability, "-capability"), raw)
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
		if *capability != "" {
			if err := c.ServeTasks(ctx, *capability, execHandler(*execCmd)); err != nil {
				return err
			}
		}
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

// execHandler runs command once per envelope; see usage.
func execHandler(command string) client.Handler {
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		in, err := e.Marshal()
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = bytes.NewReader(in)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			reason := strings.TrimSpace(stderr.String())
			if reason == "" {
				reason = err.Error()
			}
			return nil, errors.New(reason)
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
