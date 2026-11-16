package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/agent"
	"github.com/jefflaplante/agentbus/client"
)

// execHandler runs command once per envelope of the given types, with the
// same runner the agent runtime uses (see agent.CommandHandler).
func execHandler(command string, timeout time.Duration, types map[a2a.Type]bool, log *slog.Logger) client.Handler {
	run := agent.CommandHandler([]string{"sh", "-c", command}, agent.CommandOptions{Timeout: timeout, Log: log})
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		if !types[e.Type] {
			if e.Type == a2a.TypeError {
				log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
			}
			return nil, nil
		}
		return run(ctx, e)
	}
}
