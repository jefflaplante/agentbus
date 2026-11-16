// Command coordinator is the demo coordinator agent. It runs a scripted
// conversation with the worker, checks every reply, prints each step to
// stderr and a JSON summary to stdout, and exits non-zero if a step fails.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/mqtt/client"
	"github.com/jefflaplante/mqtt/demo/agents"
)

func main() {
	url := flag.String("url", env("AGENTBUS_URL", "tcp://localhost:1883"), "broker URL")
	agent := flag.String("agent", env("AGENTBUS_AGENT", "coordinator"), "agent ID")
	token := flag.String("token", os.Getenv("AGENTBUS_TOKEN"), "agent token")
	tokenFile := flag.String("token-file", os.Getenv("AGENTBUS_TOKEN_FILE"), "file containing the agent token")
	worker := flag.String("worker", "worker", "worker agent ID")
	capability := flag.String("capability", "textstats", "capability to send tasks to")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	flag.Parse()
	if *tokenFile != "" {
		b, err := os.ReadFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read token file:", err)
			os.Exit(1)
		}
		*token = strings.TrimSpace(string(b))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	sum, err := agents.RunCoordinator(ctx, agents.CoordinatorOptions{
		Client: client.Options{
			BrokerURL: *url, AgentID: *agent, Token: *token,
			CleanSession: true, // one-shot: leave nothing queued on the broker
			Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Worker:     *worker,
		Capability: *capability,
		Out:        os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "coordinator:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(sum)
	if !sum.OK {
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
