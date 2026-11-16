// Command service is a minimal agent that serves all three kinds of
// topic: a task queue (wordcount), its own inbox, and other agents'
// events. docs/SERVICES.md walks through it.
//
//	AGENTBUS_AGENT=counter go run ./examples/service
//
// The identity needs the capability: agentbus config agent add counter --capability wordcount
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// The task body and reply. The presence description below tells other
// agents (and the models driving them) this shape.
type countRequest struct {
	Text string `json:"text"`
}

type countReply struct {
	Words int `json:"words"`
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agent := env("AGENTBUS_AGENT", "counter")
	tokenFile := env("AGENTBUS_TOKEN_FILE", filepath.Join(os.Getenv("HOME"), ".config/agentbus/tokens", agent+".token"))
	tok, err := os.ReadFile(tokenFile)
	if err != nil {
		log.Error("read token", "err", err)
		os.Exit(1)
	}

	c, err := client.Connect(ctx, client.Options{
		BrokerURL:   env("AGENTBUS_URL", "tcp://127.0.0.1:1883"),
		AgentID:     agent,
		Token:       strings.TrimSpace(string(tok)),
		Description: `wordcount: body {"text": string}, replies {"words": int}. Inbox: {"op": "ping"}.`,
		// Persistent session (CleanSession false, the default): work sent
		// while this agent is down is delivered when it returns.
		// Task handlers go here, not in a later ServeTasks call, so
		// queued tasks have a handler the moment the session resumes.
		Tasks:         map[string]client.Handler{"wordcount": wordcount},
		Inbox:         inbox(log),
		MaxConcurrent: 8, // at most 8 handlers at once; more requests get "busy"
		Logger:        log,
	})
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer c.Close()

	// Other agents' broadcasts. No reply is possible or expected.
	err = c.WatchEvents(ctx, "a2a/v1/events/+/jobs/#", func(e *a2a.Envelope) {
		log.Info("job event", "from", e.From, "event", e.Event)
	})
	if err != nil {
		log.Error("watch events", "err", err)
	}

	log.Info("serving", "agent", agent)
	<-ctx.Done()
}

// wordcount handles one task from the shared queue. Exactly one worker
// gets each task. The return value is the reply body; an error becomes an
// error reply whose reason the requester sees.
func wordcount(ctx context.Context, e *a2a.Envelope) (any, error) {
	var req countRequest
	if err := e.DecodeBody(&req); err != nil || req.Text == "" {
		return nil, errors.New(`body must be {"text": "..."}`)
	}
	if err := ctx.Err(); err != nil {
		return nil, err // the request's TTL ran out; nobody is waiting
	}
	return countReply{Words: len(strings.Fields(req.Text))}, nil
}

// inbox handles everything sent to this agent directly, including replies
// that arrived too late and errors the broker sent about our own
// publishes. Switch on the type; only requests get a reply.
func inbox(log *slog.Logger) client.Handler {
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		switch e.Type {
		case a2a.TypeRequest:
			var op struct {
				Op string `json:"op"`
			}
			_ = e.DecodeBody(&op)
			if op.Op == "ping" {
				return map[string]string{"op": "pong"}, nil
			}
			return nil, fmt.Errorf("unknown op %q; try {\"op\": \"ping\"}", op.Op)
		case a2a.TypeEvent:
			log.Info("note", "from", e.From, "event", e.Event, "priority", e.Priority)
		case a2a.TypeError:
			log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
		case a2a.TypeResponse:
			log.Info("late reply ignored", "from", e.From, "correlation_id", e.CorrelationID)
		}
		return nil, nil // ignored for anything but a request
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
