package broker_test

// A crash test for the broker's store: senders publish to an offline
// agent, the broker process is killed with SIGKILL partway through, and a
// new broker on the same data directory must deliver what was
// acknowledged.
//
// SIGKILL stops the process but not the operating system, so what the
// process had already handed to the kernel survives. The test therefore
// covers writes still queued inside the broker and a transaction cut off
// partway. It does not cover power loss, which also needs fsync to have
// done its job.

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

const crashConfigEnv = "AGENTBUS_CRASH_CONFIG"

// TestCrashHelperBroker is the broker process the crash test kills. It
// only runs when the crash test starts it.
func TestCrashHelperBroker(t *testing.T) {
	path := os.Getenv(crashConfigEnv)
	if path == "" {
		t.Skip("helper process for TestCrashRecovery")
	}
	cfg, err := broker.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := broker.New(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	select {} // until killed
}

type crashMsg struct {
	S int `json:"s"` // sender
	N int `json:"n"` // that sender's message number
}

func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("starts and kills broker processes")
	}
	const (
		senders = 8
		rounds  = 6
		// The broker sends a PUBACK before it stores the message, so each
		// sender can lose the one packet the broker was processing.
		allowed = 1
	)

	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	tokens := map[string]string{}
	cfg := &broker.Config{
		Listen: addr, DataDir: filepath.Join(dir, "data"),
		RatePerSecond: 1e6, RateBurst: 1e6,
		// The default, 1024, fills within one round, and the broker drops
		// what doesn't fit, which would count here as lost.
		MaxQueued: 65535,
	}
	add := func(a broker.AgentConfig) {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		tokens[a.ID] = tok
		cfg.Agents = append(cfg.Agents, a)
	}
	add(broker.AgentConfig{ID: "sink"})
	for i := range senders {
		add(broker.AgentConfig{ID: senderID(i), SendTo: []string{"sink"}})
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "broker.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var proc *exec.Cmd
	start := func() {
		t.Helper()
		proc = exec.Command(os.Args[0], "-test.run", "^TestCrashHelperBroker$")
		proc.Env = append(os.Environ(), crashConfigEnv+"="+cfgPath)
		proc.Stdout, proc.Stderr = os.Stderr, os.Stderr
		if err := proc.Start(); err != nil {
			t.Fatal(err)
		}
	}
	kill := func() {
		_ = proc.Process.Kill() // SIGKILL
		_ = proc.Wait()
	}
	t.Cleanup(func() { kill() })

	// connect retries until the broker process is listening.
	connect := func(id string, inbox client.Handler) *client.Client {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			c, err := client.Connect(ctx, client.Options{
				BrokerURL: "tcp://" + addr, AgentID: id, Token: tokens[id],
				Inbox: inbox, Logger: quietLog(), MaxConcurrent: 4096,
			})
			cancel()
			if err == nil {
				return c
			}
			if time.Now().After(deadline) {
				t.Fatalf("connect %s: %v", id, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	var mu sync.Mutex
	got := map[crashMsg]int{}
	var lastArrival time.Time
	receive := func(_ context.Context, env *a2a.Envelope) (any, error) {
		var m crashMsg
		if json.Unmarshal(env.Body, &m) == nil {
			mu.Lock()
			got[m]++
			lastArrival = time.Now()
			mu.Unlock()
		}
		return nil, nil
	}

	start()
	connect("sink", receive).Close() // the sink's session exists; it is offline from here on

	next := make([]int, senders)
	var totalAcked, totalLost, worst int
	for round := range rounds {
		// Senders publish until the broker dies.
		ctx, cancel := context.WithCancel(context.Background())
		acked := make([][]crashMsg, senders)
		clients := make([]*client.Client, senders)
		for i := range clients {
			clients[i] = connect(senderID(i), nil)
		}
		var wg sync.WaitGroup
		for i := range senders {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for ctx.Err() == nil {
					m := crashMsg{S: i, N: next[i]}
					next[i]++
					if clients[i].Send(ctx, "sink", m) == nil {
						acked[i] = append(acked[i], m)
					}
				}
			}()
		}
		time.Sleep(time.Duration(200+rand.IntN(500)) * time.Millisecond)
		kill()
		cancel()
		wg.Wait()
		for _, c := range clients {
			c.Close()
		}

		// A new broker on the same data directory delivers what it has.
		start()
		mu.Lock()
		lastArrival = time.Now()
		mu.Unlock()
		sink := connect("sink", receive)
		for {
			time.Sleep(200 * time.Millisecond)
			mu.Lock()
			quiet := time.Since(lastArrival)
			mu.Unlock()
			if quiet > 2*time.Second {
				break
			}
		}
		sink.Close()

		mu.Lock()
		for i := range senders {
			lost := 0
			for _, m := range acked[i] {
				if got[m] == 0 {
					lost++
				}
			}
			totalAcked += len(acked[i])
			totalLost += lost
			worst = max(worst, lost)
			if lost > allowed {
				t.Errorf("round %d: %s lost %d of %d acknowledged messages; at most %d allowed",
					round, senderID(i), lost, len(acked[i]), allowed)
			}
		}
		mu.Unlock()
	}
	t.Logf("%d kills: %d messages acknowledged, %d lost, at most %d from one sender in one kill (allowed %d)",
		rounds, totalAcked, totalLost, worst, allowed)
}
