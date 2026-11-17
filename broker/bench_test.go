package broker_test

// Throughput and latency benchmarks for the broker, with and without
// data_dir. "make bench" runs them; docs/BENCHMARKS.md explains the
// output and how to run them on the disk the broker will use.
//
// Each case starts its own broker and connects its agents with
// persistent sessions, the way long-running agents connect. The data
// directory comes from b.TempDir, so TMPDIR decides which disk is
// measured.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
)

const benchCapability = "bench"

// benchSenders is how many sender agents each case runs with.
var benchSenders = []int{1, 8, 100}

// benchWorkers is how many workers serve the task queue. The cases with
// more than one worker run with the largest number of senders only.
var benchWorkers = []int{1, 4, 16}

// benchBus is a broker with nSenders sender agents, the "sink" agent for
// inbox messages and nWorkers worker agents for tasks.
type benchBus struct {
	b      *testing.B
	url    string
	tokens map[string]string
}

func startBenchBus(b *testing.B, persist bool, nSenders, nWorkers int) *benchBus {
	b.Helper()
	bb := &benchBus{b: b, tokens: map[string]string{}}
	cfg := &broker.Config{
		Listen: "127.0.0.1:0",
		// The defaults (20/s, burst 50) would measure the rate limiter.
		RatePerSecond: 1e6, RateBurst: 1e6,
		// Every agent connects from 127.0.0.1; the default is 64 per address.
		MaxConnectionsPerIP: 1024,
	}
	if persist {
		cfg.DataDir = filepath.Join(b.TempDir(), "data")
	}
	add := func(a broker.AgentConfig) {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		bb.tokens[a.ID] = tok
		cfg.Agents = append(cfg.Agents, a)
	}
	add(broker.AgentConfig{ID: "sink"})
	for i := range nWorkers {
		add(broker.AgentConfig{ID: workerID(i), Capabilities: []string{benchCapability}})
	}
	for i := range nSenders {
		add(broker.AgentConfig{ID: senderID(i), SendTo: []string{"sink"}})
	}
	srv, err := broker.New(cfg, quietLog())
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = srv.Close() })
	bb.url = "tcp://" + srv.Addr()
	return bb
}

func senderID(i int) string { return fmt.Sprintf("sender-%d", i) }
func workerID(i int) string { return fmt.Sprintf("worker-%d", i) }

func (bb *benchBus) connect(id string, inbox client.Handler, tasks map[string]client.Handler) *client.Client {
	bb.b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: bb.url, AgentID: id, Token: bb.tokens[id],
		Inbox: inbox, Tasks: tasks, Logger: quietLog(),
		// The default, 64, makes the worker answer "busy" with 100 senders.
		MaxConcurrent: 256,
	})
	if err != nil {
		bb.b.Fatalf("connect %s: %v", id, err)
	}
	bb.b.Cleanup(c.Close)
	return c
}

// stamp is the message body: when the sender started publishing it.
type stamp struct {
	T int64 `json:"t"`
}

// latencies collects durations from several goroutines.
type latencies struct {
	mu sync.Mutex
	d  []time.Duration
}

func (l *latencies) add(d time.Duration) {
	l.mu.Lock()
	l.d = append(l.d, d)
	l.mu.Unlock()
}

// report adds p50 and p99 of l, in milliseconds, as name-p50-ms and
// name-p99-ms.
func (l *latencies) report(b *testing.B, name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.d) == 0 {
		return
	}
	slices.Sort(l.d)
	at := func(p float64) float64 {
		return float64(l.d[int(float64(len(l.d)-1)*p)].Microseconds()) / 1000
	}
	b.ReportMetric(at(0.50), name+"-p50-ms")
	b.ReportMetric(at(0.99), name+"-p99-ms")
}

// split runs n operations across workers goroutines; op gets the
// worker number. It stops at the first error.
func split(b *testing.B, n, workers int, op func(w int) error) {
	var wg sync.WaitGroup
	var failed atomic.Bool
	for w := range workers {
		count := n / workers
		if w < n%workers {
			count++
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range count {
				if failed.Load() {
					return
				}
				if err := op(w); err != nil {
					failed.Store(true)
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if failed.Load() {
		b.FailNow()
	}
}

// BenchmarkInbox sends one-way messages from each sender to one inbox.
// ack is the time until the sender has the broker's PUBACK; delivery is
// the time until the receiver's handler runs.
func BenchmarkInbox(b *testing.B) {
	for _, persist := range []bool{false, true} {
		for _, senders := range benchSenders {
			b.Run(fmt.Sprintf("data_dir=%v/senders=%d", onOff(persist), senders), func(b *testing.B) {
				benchInbox(b, persist, senders)
			})
		}
	}
}

func benchInbox(b *testing.B, persist bool, nSenders int) {
	bb := startBenchBus(b, persist, nSenders, 0)
	var ack, delivery latencies
	var got atomic.Int64
	want := int64(b.N)
	done := make(chan struct{})
	bb.connect("sink", func(_ context.Context, env *a2a.Envelope) (any, error) {
		var s stamp
		if json.Unmarshal(env.Body, &s) == nil && s.T > 0 {
			delivery.add(time.Since(time.Unix(0, s.T)))
		}
		if got.Add(1) == want {
			close(done)
		}
		return nil, nil
	}, nil)
	senders := make([]*client.Client, nSenders)
	for i := range senders {
		senders[i] = bb.connect(senderID(i), nil, nil)
	}
	ctx := context.Background()

	b.ResetTimer()
	start := time.Now()
	split(b, b.N, nSenders, func(w int) error {
		t := time.Now()
		if err := senders[w].Send(ctx, "sink", stamp{T: t.UnixNano()}); err != nil {
			return err
		}
		ack.add(time.Since(t))
		return nil
	})
	select {
	case <-done:
	case <-time.After(time.Minute):
		b.Fatalf("%d of %d messages arrived within a minute of the last send", got.Load(), want)
	}
	elapsed := time.Since(start)
	b.StopTimer()

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "msgs/s")
	ack.report(b, "ack")
	delivery.report(b, "delivery")
}

// BenchmarkTask sends requests to a task queue and waits for each reply.
// The queue is served by one worker, and with the most senders also by
// several. Each round trip is two stored messages: the task
// and the reply.
func BenchmarkTask(b *testing.B) {
	for _, persist := range []bool{false, true} {
		for _, senders := range benchSenders {
			for _, workers := range benchWorkers {
				if workers > 1 && senders != benchSenders[len(benchSenders)-1] {
					continue
				}
				b.Run(fmt.Sprintf("data_dir=%v/senders=%d/workers=%d", onOff(persist), senders, workers), func(b *testing.B) {
					benchTask(b, persist, senders, workers)
				})
			}
		}
	}
}

func benchTask(b *testing.B, persist bool, nSenders, nWorkers int) {
	bb := startBenchBus(b, persist, nSenders, nWorkers)
	for i := range nWorkers {
		bb.connect(workerID(i), nil, map[string]client.Handler{
			benchCapability: func(_ context.Context, env *a2a.Envelope) (any, error) { return env.Body, nil },
		})
	}
	senders := make([]*client.Client, nSenders)
	for i := range senders {
		senders[i] = bb.connect(senderID(i), nil, nil)
	}
	var rtt latencies

	b.ResetTimer()
	start := time.Now()
	split(b, b.N, nSenders, func(w int) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		t := time.Now()
		if _, err := senders[w].RequestTask(ctx, benchCapability, stamp{T: t.UnixNano()}); err != nil {
			return err
		}
		rtt.add(time.Since(t))
		return nil
	})
	elapsed := time.Since(start)
	b.StopTimer()

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "tasks/s")
	rtt.report(b, "roundtrip")
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
