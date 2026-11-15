package agents

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/client"
)

// Op is a direct request to the worker's inbox.
type Op struct {
	Op   string `json:"op"`             // "ping" or "reverse"
	Text string `json:"text,omitempty"` // for "reverse"
}

// OpResult answers an Op.
type OpResult struct {
	Op     string    `json:"op"` // "pong" or "reverse"
	Text   string    `json:"text,omitempty"`
	Worker string    `json:"worker"`
	Time   time.Time `json:"time"`
}

// Note is a one-way message between the demo agents' inboxes.
type Note struct {
	Kind string `json:"kind"` // "hello" or "hello-back"
	Text string `json:"text"`
}

// JobDone is the event the worker emits on events/{worker}/jobs/done.
type JobDone struct {
	RequestID string `json:"request_id"`
	From      string `json:"from"`
	Words     int    `json:"words"`
}

// WorkerOptions configures RunWorker.
type WorkerOptions struct {
	Client     client.Options // Inbox and Description are set by RunWorker
	Capability string         // task queue to serve; default "textstats"
	Delay      time.Duration  // simulated work time per task
	Log        *slog.Logger
}

// RunWorker connects, serves the capability and the inbox, and blocks
// until ctx ends.
func RunWorker(ctx context.Context, opts WorkerOptions) error {
	if opts.Capability == "" {
		opts.Capability = "textstats"
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	w := &worker{opts: opts, log: opts.Log}

	co := opts.Client
	co.Description = "Demo worker: text statistics and ping/reverse"
	co.Inbox = w.inbox
	// Registered before connecting, so tasks queued while the worker was
	// offline are handled when the broker redelivers them.
	co.Tasks = map[string]client.Handler{opts.Capability: w.task}
	c, err := client.Connect(ctx, co)
	if err != nil {
		return fmt.Errorf("connect and serve %s: %w", opts.Capability, err)
	}
	defer c.Close()
	w.c.Store(c)
	w.log.Info("worker ready", "agent", c.ID(), "capability", opts.Capability)
	<-ctx.Done()
	w.log.Info("worker stopping")
	return nil
}

type worker struct {
	opts WorkerOptions
	log  *slog.Logger
	c    atomic.Pointer[client.Client]
}

func (w *worker) id() string { return w.opts.Client.AgentID }

// inbox answers ping/reverse requests and replies to hello notes.
func (w *worker) inbox(ctx context.Context, e *a2a.Envelope) (any, error) {
	switch e.Type {
	case a2a.TypeRequest:
		var op Op
		if err := e.DecodeBody(&op); err != nil {
			return nil, fmt.Errorf("bad request body: %w", err)
		}
		w.log.Info("request", "from", e.From, "op", op.Op, "id", e.ID)
		switch op.Op {
		case "ping":
			return OpResult{Op: "pong", Worker: w.id(), Time: time.Now().UTC()}, nil
		case "reverse":
			r := []rune(op.Text)
			for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
				r[i], r[j] = r[j], r[i]
			}
			return OpResult{Op: "reverse", Text: string(r), Worker: w.id(), Time: time.Now().UTC()}, nil
		}
		return nil, fmt.Errorf("unknown op %q", op.Op)

	case a2a.TypeEvent:
		var n Note
		_ = e.DecodeBody(&n)
		w.log.Info("note", "from", e.From, "kind", n.Kind, "text", n.Text)
		if n.Kind == "hello" {
			if c := w.c.Load(); c != nil {
				reply := Note{Kind: "hello-back", Text: "hi " + e.From + ", " + w.id() + " here"}
				if err := c.Send(ctx, e.From, reply); err != nil {
					w.log.Error("send hello-back", "err", err)
				}
			}
		}

	case a2a.TypeError:
		w.log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
	}
	return nil, nil
}

// task handles one textstats request from the shared queue.
func (w *worker) task(ctx context.Context, e *a2a.Envelope) (any, error) {
	var req StatsRequest
	if err := e.DecodeBody(&req); err != nil {
		return nil, fmt.Errorf("bad task body: %w", err)
	}
	if w.opts.Delay > 0 {
		select {
		case <-time.After(w.opts.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	res := Stats(req.Text)
	res.Worker = w.id()
	w.log.Info("task", "from", e.From, "id", e.ID, "words", res.Words)

	if c := w.c.Load(); c != nil {
		if err := c.Emit(ctx, "jobs/done", JobDone{RequestID: e.ID, From: e.From, Words: res.Words}); err != nil {
			w.log.Error("emit jobs/done", "err", err)
		}
	}
	return res, nil
}
