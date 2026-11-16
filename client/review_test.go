package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// slowProxy forwards TCP to the broker, holding back everything the broker
// sends by delay, so the CONNACK arrives late.
type slowProxy struct {
	addr   string
	mu     sync.Mutex
	closed []chan struct{} // closed when the client side of each conn ends
}

func startSlowProxy(t *testing.T, target string, delay time.Duration) *slowProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	p := &slowProxy{addr: ln.Addr().String()}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			done := make(chan struct{})
			p.mu.Lock()
			p.closed = append(p.closed, done)
			p.mu.Unlock()
			go func() { // client -> broker, until the client hangs up
				_, _ = io.Copy(out, in)
				close(done)
				_ = out.Close()
				_ = in.Close()
			}()
			go func() { // broker -> client, late
				time.Sleep(delay)
				_, _ = io.Copy(in, out)
				_ = in.Close()
			}()
		}
	}()
	return p
}

func (p *slowProxy) conns() []chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]chan struct{}(nil), p.closed...)
}

// If Connect gives up before the CONNACK, the connection paho was still
// making must not survive and hold the agent ID.
func TestConnectTimeoutLeavesNoConnection(t *testing.T) {
	b := startBus(t)
	p := startSlowProxy(t, strings.TrimPrefix(b.url, "tcp://"), 500*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: "tcp://" + p.addr, AgentID: "alice",
		Token: b.tokens["alice"], CleanSession: true, Logger: quiet()})
	if err == nil {
		c.Close()
		t.Fatal("Connect succeeded through a proxy that delays the CONNACK")
	}
	conns := p.conns()
	if len(conns) == 0 {
		t.Fatal("no connection reached the proxy")
	}
	for _, done := range conns {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("the abandoned connection is still open after the late CONNACK")
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(p.conns()); n != len(conns) {
		t.Errorf("client reconnected after Connect failed: %d connections", n)
	}
}

func TestEmitRejectsBadNames(t *testing.T) {
	b := startBus(t)
	alice := b.connect(t, "alice", nil)
	for _, name := range []string{"", "jobs/#", "#", "jobs/+/done", "a\x00b", "/jobs", "jobs/", "jobs//done",
		strings.Repeat("x", 257)} {
		if err := alice.Emit(ctx5(t), name, 1); err == nil {
			t.Errorf("Emit(%q) accepted", name)
		}
	}
	// Still connected and working after the refusals.
	if err := alice.Emit(ctx5(t), strings.Repeat("x", 256), 1); err != nil {
		t.Errorf("256-byte name: %v", err)
	}
}

func TestEmitSetsEventName(t *testing.T) {
	b := startBus(t)
	got := make(chan *a2a.Envelope, 2)
	bob := b.connect(t, "bob", nil)
	if err := bob.WatchEvents(ctx5(t), "a2a/v1/events/alice/#", func(e *a2a.Envelope) { got <- e }); err != nil {
		t.Fatal(err)
	}
	alice := b.connect(t, "alice", nil)
	if err := alice.Emit(ctx5(t), "jobs/finished", 1); err != nil {
		t.Fatal(err)
	}
	if e := recv(t, got, "the event"); e.Event != "jobs/finished" {
		t.Errorf("event = %q, want jobs/finished", e.Event)
	}
	if err := alice.Emit(ctx5(t), "jobs/finished", 1, client.WithEvent("custom")); err != nil {
		t.Fatal(err)
	}
	if e := recv(t, got, "the event"); e.Event != "custom" {
		t.Errorf("event = %q, want custom (WithEvent wins)", e.Event)
	}
}

// Send's context bounds the publish, not the message's life: an offline
// agent gets the message when it returns, however short ctx was.
func TestSendDeadlineIsNotTTL(t *testing.T) {
	b := startBus(t)
	bob := b.connect(t, "bob", func(o *client.Options) { o.CleanSession = false })
	bob.Close() // offline, with a persistent session
	alice := b.connect(t, "alice", nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := alice.Send(ctx, "bob", "while you were out")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond) // past the Send ctx's deadline
	got := make(chan *a2a.Envelope, 1)
	b.connect(t, "bob", func(o *client.Options) {
		o.CleanSession = false
		o.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) { got <- e; return nil, nil }
	})
	if e := recv(t, got, "the queued message"); e.TTLMillis != 0 {
		t.Errorf("ttl_ms = %d, want 0", e.TTLMillis)
	}
}

func TestUnencodableReplyBecomesError(t *testing.T) {
	b := startBus(t)
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(context.Context, *a2a.Envelope) (any, error) { return math.NaN(), nil }
	})
	alice := b.connect(t, "alice", nil)
	_, err := alice.Request(ctx5(t), "bob", "hi")
	var re *client.RemoteError
	if !errors.As(err, &re) || re.Code != "handler_error" || !strings.Contains(re.Reason, "could not be encoded") {
		t.Fatalf("want handler_error about encoding, got %v", err)
	}
}

func TestReplyErrorCodes(t *testing.T) {
	b := startBus(t)
	b.connect(t, "bob", func(o *client.Options) {
		o.Inbox = func(_ context.Context, e *a2a.Envelope) (any, error) {
			var q string
			_ = e.DecodeBody(&q)
			switch q {
			case "custom":
				return nil, client.Errorf("not_found", "no job %s", "42")
			case "wrapped":
				return nil, fmt.Errorf("lookup: %w", &client.ReplyError{Code: "quota.exceeded", Reason: "try tomorrow"})
			case "badcode":
				return nil, client.Errorf("Not Valid!", "oops")
			default:
				return nil, errors.New("plain failure")
			}
		}
	})
	alice := b.connect(t, "alice", nil)
	for _, tc := range []struct{ q, code, reason string }{
		{"custom", "not_found", "no job 42"},
		{"wrapped", "quota.exceeded", "try tomorrow"},
		{"badcode", "handler_error", "Not Valid!: oops"},
		{"plain", "handler_error", "plain failure"},
	} {
		_, err := alice.Request(ctx5(t), "bob", tc.q)
		var re *client.RemoteError
		if !errors.As(err, &re) || re.Code != tc.code || re.Reason != tc.reason {
			t.Errorf("%s: got %v, want %s: %s", tc.q, err, tc.code, tc.reason)
		}
	}
}
