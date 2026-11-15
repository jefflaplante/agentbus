package agents

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/client"
)

// DefaultTexts are the documents the coordinator sends as tasks.
var DefaultTexts = []string{
	"The quick brown fox jumps over the lazy dog.",
	"Agents talk to agents.\nThe bus carries the envelope; the agent reads the body.",
	"Request, response, event, error: four types, one envelope, one contract.",
	"Presence is retained. Tasks are shared. Inboxes are private.",
	"To be, or not to be, that is the question.",
}

// CoordinatorOptions configures RunCoordinator.
type CoordinatorOptions struct {
	Client     client.Options // Inbox and Description are set by RunCoordinator
	Worker     string         // agent to talk to; default "worker"
	Capability string         // task queue; default "textstats"
	Texts      []string       // task bodies; default DefaultTexts
	Out        io.Writer      // live step log; nil for none
}

// Step is one checked exchange in the coordinator's script.
type Step struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Millis int64  `json:"ms"`
}

// Summary is the coordinator's result. OK is true when every step passed.
type Summary struct {
	OK    bool   `json:"ok"`
	Agent string `json:"agent"`
	Steps []Step `json:"steps"`
}

// RunCoordinator connects and runs this script against the worker:
//
//	presence  wait until the worker is online and offers the capability
//	hello     send a one-way note; the worker sends one back to our inbox
//	ping      direct request/response
//	reverse   direct request/response with a payload
//	tasks     send every text through the shared task queue concurrently,
//	          check each answer, and see one jobs/done event per task
//	acl       try to consume the capability; the broker must refuse
//
// It stops at the first failed step.
func RunCoordinator(ctx context.Context, opts CoordinatorOptions) (*Summary, error) {
	if opts.Worker == "" {
		opts.Worker = "worker"
	}
	if opts.Capability == "" {
		opts.Capability = "textstats"
	}
	if len(opts.Texts) == 0 {
		opts.Texts = DefaultTexts
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}

	notes := make(chan Note, 8)
	co := opts.Client
	co.Description = "Demo coordinator"
	co.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) {
		if e.Type == a2a.TypeEvent {
			var n Note
			if e.DecodeBody(&n) == nil {
				select {
				case notes <- n:
				default:
				}
			}
		}
		return nil, nil
	}
	c, err := client.Connect(ctx, co)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	r := &run{c: c, opts: opts, sum: &Summary{OK: true, Agent: c.ID()}}
	fmt.Fprintf(opts.Out, "coordinator %q connected; talking to %q\n\n", c.ID(), opts.Worker)

	r.step(ctx, "presence", func(ctx context.Context) (string, error) {
		for {
			for _, p := range c.Agents() {
				if p.Agent == opts.Worker && p.Status == a2a.StatusOnline && slices.Contains(p.Capabilities, opts.Capability) {
					return fmt.Sprintf("%s online, capabilities %v", p.Agent, p.Capabilities), nil
				}
			}
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("%s never came online with %q", opts.Worker, opts.Capability)
			case <-time.After(50 * time.Millisecond):
			}
		}
	})

	r.step(ctx, "hello", func(ctx context.Context) (string, error) {
		if err := c.Send(ctx, opts.Worker, Note{Kind: "hello", Text: "hello from " + c.ID()}); err != nil {
			return "", err
		}
		for {
			select {
			case n := <-notes:
				if n.Kind == "hello-back" {
					return fmt.Sprintf("sent hello, got back %q", n.Text), nil
				}
			case <-ctx.Done():
				return "", errors.New("no hello-back in our inbox")
			}
		}
	})

	r.step(ctx, "ping", func(ctx context.Context) (string, error) {
		var res OpResult
		if err := request(ctx, c, opts.Worker, Op{Op: "ping"}, &res); err != nil {
			return "", err
		}
		if res.Op != "pong" {
			return "", fmt.Errorf("got %q, want pong", res.Op)
		}
		return "pong from " + res.Worker, nil
	})

	r.step(ctx, "reverse", func(ctx context.Context) (string, error) {
		var res OpResult
		if err := request(ctx, c, opts.Worker, Op{Op: "reverse", Text: "agentbus"}, &res); err != nil {
			return "", err
		}
		if res.Text != "subtnega" {
			return "", fmt.Errorf("got %q, want %q", res.Text, "subtnega")
		}
		return `"agentbus" -> "subtnega"`, nil
	})

	r.step(ctx, "tasks", func(ctx context.Context) (string, error) {
		return runTasks(ctx, c, opts)
	})

	r.step(ctx, "acl", func(ctx context.Context) (string, error) {
		err := c.ServeTasks(ctx, opts.Capability, func(context.Context, *a2a.Envelope) (any, error) { return nil, nil })
		if err == nil {
			return "", fmt.Errorf("broker let %s consume %q", c.ID(), opts.Capability)
		}
		return "broker refused: " + err.Error(), nil
	})

	verdict := "PASS"
	if !r.sum.OK {
		verdict = "FAIL"
	}
	fmt.Fprintf(opts.Out, "\n%s: %d/%d steps\n", verdict, passed(r.sum.Steps), len(r.sum.Steps))
	return r.sum, nil
}

type run struct {
	c    *client.Client
	opts CoordinatorOptions
	sum  *Summary
}

// step runs fn with a 10s budget, records it and prints it. After a
// failure later steps are skipped.
func (r *run) step(ctx context.Context, name string, fn func(context.Context) (string, error)) {
	if !r.sum.OK {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := time.Now()
	detail, err := fn(sctx)
	s := Step{Name: name, OK: err == nil, Detail: detail, Millis: time.Since(start).Milliseconds()}
	mark := "ok  "
	if err != nil {
		s.Detail, mark = err.Error(), "FAIL"
		r.sum.OK = false
	}
	r.sum.Steps = append(r.sum.Steps, s)
	fmt.Fprintf(r.opts.Out, "  [%s] %-9s %s (%dms)\n", mark, name, s.Detail, s.Millis)
}

func request(ctx context.Context, c *client.Client, to string, body, out any) error {
	reply, err := c.Request(ctx, to, body)
	if err != nil {
		return err
	}
	return reply.DecodeBody(out)
}

// runTasks sends every text as a task at once, checks each result against
// a local computation, and waits for one jobs/done event per task.
func runTasks(ctx context.Context, c *client.Client, opts CoordinatorOptions) (string, error) {
	var mu sync.Mutex
	events := map[string]bool{}
	eventCh := make(chan struct{}, len(opts.Texts)*2)
	filter := a2a.Root + "/events/+/jobs/#"
	err := c.WatchEvents(ctx, filter, func(e *a2a.Envelope) {
		var d JobDone
		if e.DecodeBody(&d) == nil && d.From == c.ID() {
			mu.Lock()
			events[d.RequestID] = true
			mu.Unlock()
			eventCh <- struct{}{}
		}
	})
	if err != nil {
		return "", err
	}

	type result struct {
		requestID string
		worker    string
		err       error
	}
	results := make(chan result, len(opts.Texts))
	for _, text := range opts.Texts {
		go func() {
			reply, err := c.RequestTask(ctx, opts.Capability, StatsRequest{Text: text})
			if err != nil {
				results <- result{err: err}
				return
			}
			var got StatsResult
			if err := reply.DecodeBody(&got); err != nil {
				results <- result{err: err}
				return
			}
			want := Stats(text)
			want.Worker = got.Worker
			if !reflect.DeepEqual(got, want) {
				err = fmt.Errorf("wrong stats for %q: got %+v want %+v", text, got, want)
			}
			results <- result{requestID: reply.CorrelationID, worker: got.Worker, err: err}
		}()
	}

	workers := map[string]int{}
	var ids []string
	for range opts.Texts {
		res := <-results
		if res.err != nil {
			return "", res.err
		}
		workers[res.worker]++
		ids = append(ids, res.requestID)
	}

	for {
		mu.Lock()
		missing := 0
		for _, id := range ids {
			if !events[id] {
				missing++
			}
		}
		mu.Unlock()
		if missing == 0 {
			break
		}
		select {
		case <-eventCh:
		case <-ctx.Done():
			return "", fmt.Errorf("%d jobs/done events never arrived", missing)
		}
	}
	return fmt.Sprintf("%d tasks answered and verified, %d jobs/done events, handled by %v",
		len(opts.Texts), len(ids), workers), nil
}

func passed(steps []Step) int {
	n := 0
	for _, s := range steps {
		if s.OK {
			n++
		}
	}
	return n
}
