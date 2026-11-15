// Command worker is the demo worker agent. It serves the "textstats"
// capability from the shared task queue and answers ping/reverse requests
// and hello notes in its inbox. It runs until interrupted.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jefflaplante/mqtt/client"
	"github.com/jefflaplante/mqtt/demo/agents"
)

func main() {
	url := flag.String("url", env("AGENTBUS_URL", "tcp://localhost:1883"), "broker URL")
	agent := flag.String("agent", env("AGENTBUS_AGENT", "worker"), "agent ID")
	token := flag.String("token", os.Getenv("AGENTBUS_TOKEN"), "agent token")
	capability := flag.String("capability", "textstats", "capability to serve")
	delay := flag.Duration("delay", 0, "simulated work time per task, e.g. 500ms")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := agents.RunWorker(ctx, agents.WorkerOptions{
		Client: client.Options{
			BrokerURL: *url, AgentID: *agent, Token: *token,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		Capability: *capability,
		Delay:      *delay,
		Log:        log,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
