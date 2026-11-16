// Command trafficgen runs a broker in-process with a handful of agents
// that keep talking: tasks to a shared queue served by two workers,
// requests and replies between agents, broadcast events, and now and then
// a request the broker refuses. It is the live traffic behind the console
// recording in the README; watch it with the console:
//
//	go run ./tools/trafficgen -listen 127.0.0.1:18960 -token admin.token
//	agentbus --url tcp://127.0.0.1:18960 --agent admin --token-file admin.token console --tab map
//
// It runs until interrupted. Nothing it starts outlives it: the broker's
// state is in a temporary directory that is removed on exit.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18960", "broker listen address (loopback)")
	tokenFile := flag.String("token", "admin.token", "where to write the token of the observer \"admin\", for the console")
	pace := flag.Float64("pace", 1, "traffic speed: 2 is twice as busy")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *tokenFile, *pace); err != nil {
		log.Fatal(err)
	}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func run(ctx context.Context, listen, tokenFile string, pace float64) error {
	tmp, err := os.MkdirTemp("", "agentbus-trafficgen")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	tokens := map[string]string{}
	cfg := &broker.Config{Listen: listen, DataDir: tmp + "/data", StatusInterval: broker.Duration(time.Second)}
	for _, a := range []broker.AgentConfig{
		{ID: "admin", Observer: true},
		{ID: "claude-code", SendTo: []string{"weather", "researcher"}},
		{ID: "weather"},
		{ID: "researcher"},
		{ID: "coordinator"},
		{ID: "worker-1", Capabilities: []string{"textstats"}},
		{ID: "worker-2", Capabilities: []string{"textstats"}},
		{ID: "ops", SendTo: []string{"worker-1"}}, // so its requests to researcher are refused
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256, tokens[a.ID] = hash, tok
		cfg.Agents = append(cfg.Agents, a)
	}
	srv, err := broker.New(cfg, quiet)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()
	if err := os.WriteFile(tokenFile, []byte(tokens["admin"]+"\n"), 0o600); err != nil {
		return err
	}
	url := "tcp://" + srv.Addr()

	answer := func(delay time.Duration, reply func(*a2a.Envelope) any) client.Handler {
		return func(ctx context.Context, e *a2a.Envelope) (any, error) {
			if e.Type != a2a.TypeRequest {
				return nil, nil
			}
			jitter(ctx, delay)
			return reply(e), nil
		}
	}
	connect := func(id, description string, set func(*client.Options)) (*client.Client, error) {
		o := client.Options{BrokerURL: url, AgentID: id, Token: tokens[id], Logger: quiet, CleanSession: true, Description: description}
		if set != nil {
			set(&o)
		}
		return client.Connect(ctx, o)
	}
	agents := map[string]*client.Client{}
	for _, a := range []struct {
		id, desc string
		set      func(*client.Options)
	}{
		{"weather", "Forecasts from public weather services", func(o *client.Options) {
			o.Inbox = answer(250*time.Millisecond, func(*a2a.Envelope) any {
				return map[string]any{"location": "Seattle, Washington, US", "temperature": 40 + rand.IntN(12), "summary": "Chance Light Rain"}
			})
		}},
		{"researcher", "Answers questions", func(o *client.Options) {
			o.Inbox = answer(500*time.Millisecond, func(*a2a.Envelope) any { return map[string]string{"answer": "42"} })
		}},
		{"claude-code", "Claude Code, over MCP", func(o *client.Options) {
			o.Inbox = answer(300*time.Millisecond, func(*a2a.Envelope) any { return map[string]string{"ack": "noted"} })
		}},
		{"worker-1", "textstats worker", func(o *client.Options) { o.Tasks = map[string]client.Handler{"textstats": wordcount} }},
		{"worker-2", "textstats worker", func(o *client.Options) { o.Tasks = map[string]client.Handler{"textstats": wordcount} }},
		{"coordinator", "Hands out work", nil},
		{"ops", "Operations scripts", nil},
	} {
		c, err := connect(a.id, a.desc, a.set)
		if err != nil {
			return fmt.Errorf("connect %s: %w", a.id, err)
		}
		defer c.Close()
		agents[a.id] = c
	}

	fmt.Printf("broker on %s with %d agents; token for admin in %s\n", url, len(agents), tokenFile)
	fmt.Printf("watch it:  agentbus --url %s --agent admin --token-file %s console --tab map\n", url, tokenFile)

	// Each loop sends one kind of message at its own rhythm.
	every := func(base time.Duration, send func(ctx context.Context)) {
		go func() {
			for ctx.Err() == nil {
				jitter(ctx, time.Duration(float64(base)/pace))
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				send(rctx)
				cancel()
			}
		}()
	}
	words := []string{"to be or not to be", "the quick brown fox", "agents talk the broker keeps them honest"}
	every(900*time.Millisecond, func(ctx context.Context) {
		_, _ = agents["coordinator"].RequestTask(ctx, "textstats", map[string]string{"text": words[rand.IntN(len(words))]})
	})
	every(1500*time.Millisecond, func(ctx context.Context) {
		_, _ = agents["claude-code"].Request(ctx, "weather", map[string]any{"location": "Seattle, WA", "days": 2})
	})
	every(2600*time.Millisecond, func(ctx context.Context) {
		_, _ = agents["claude-code"].Request(ctx, "researcher", map[string]string{"q": "what does the broker check?"})
	})
	every(3200*time.Millisecond, func(ctx context.Context) {
		_, _ = agents["researcher"].Request(ctx, "claude-code", map[string]string{"note": "found it"})
	})
	every(2200*time.Millisecond, func(ctx context.Context) {
		w := []string{"worker-1", "worker-2"}[rand.IntN(2)]
		_ = agents[w].Emit(ctx, "jobs/done", map[string]int{"job": rand.IntN(1000)})
	})
	every(5500*time.Millisecond, func(ctx context.Context) {
		_, _ = agents["ops"].Request(ctx, "researcher", map[string]string{"q": "can I?"}) // refused: not in send_to
	})

	<-ctx.Done()
	return nil
}

func wordcount(ctx context.Context, e *a2a.Envelope) (any, error) {
	var b struct{ Text string }
	if err := e.DecodeBody(&b); err != nil {
		return nil, client.Errorf("invalid", `body must be {"text": "..."}`)
	}
	jitter(ctx, 300*time.Millisecond)
	return map[string]int{"words": len(strings.Fields(b.Text))}, nil
}

// jitter waits about d, give or take a third, or until ctx ends.
func jitter(ctx context.Context, d time.Duration) {
	d = d*2/3 + time.Duration(rand.Int64N(int64(d)*2/3+1))
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
