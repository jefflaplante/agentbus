package client_test

import (
	"context"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// A task is acknowledged only when its handler returns. If the worker dies
// mid-task, the broker resends the task when that worker's persistent
// session reconnects, and a fresh process handles it. docs/SERVICES.md
// relies on this.
func TestTaskRedeliveredAfterWorkerDies(t *testing.T) {
	b := startBus(t)
	started := make(chan struct{}, 1)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	first := b.connect(t, "bob", func(o *client.Options) {
		o.CleanSession = false
		o.Tasks = map[string]client.Handler{"summarize": func(context.Context, *a2a.Envelope) (any, error) {
			started <- struct{}{}
			<-stuck // ignores ctx, like a process that hangs and is then killed
			return nil, nil
		}}
	})
	alice := b.connect(t, "alice", nil)

	type result struct {
		env *a2a.Envelope
		err error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r, err := alice.RequestTask(ctx, "summarize", "text")
		done <- result{r, err}
	}()
	recv(t, started, "the first worker to start the task")
	first.Close() // dies without acknowledging the task

	b.connect(t, "bob", func(o *client.Options) {
		o.CleanSession = false
		o.Tasks = map[string]client.Handler{"summarize": func(context.Context, *a2a.Envelope) (any, error) {
			return "done by the restarted worker", nil
		}}
	})
	r := recv(t, done, "the redelivered task's reply")
	if r.err != nil {
		t.Fatalf("task failed: %v", r.err)
	}
	var body string
	if err := r.env.DecodeBody(&body); err != nil || body != "done by the restarted worker" {
		t.Errorf("reply body = %q (%v)", body, err)
	}
}
