package broker_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

// Regression tests for the security review. Each names the finding.

func rawPublish(t *testing.T, c paho.Client, topic string, retain bool, payload string) {
	t.Helper()
	tok := c.Publish(topic, 1, retain, payload)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		t.Fatalf("publish %s: %v", topic, tok.Error())
	}
}

func envJSON(fields string) string {
	return `{"v":1,"ts":"` + time.Now().UTC().Format(time.RFC3339Nano) + `",` + fields + `}`
}

// C1: a Will is published without OnPublish, so it could spoof senders
// and presence. Connections carrying one are refused.
func TestWillRefused(t *testing.T) {
	b := startBus(t, nil)
	opts := paho.NewClientOptions().AddBroker(b.url).SetClientID("carol").SetUsername("carol").
		SetPassword(b.tokens["carol"]).
		SetWill(a2a.PresenceTopic("alice"), `{"v":1,"agent":"alice","description":"SPOOFED"}`, 1, true)
	c := paho.NewClient(opts)
	tok := c.Connect()
	if tok.WaitTimeout(5*time.Second) && tok.Error() == nil {
		c.Disconnect(0)
		t.Fatal("connection with a Will was accepted")
	}
	if !strings.Contains(b.logs.String(), "will messages are not allowed") {
		t.Errorf("no log line explaining the refusal:\n%s", b.logs.String())
	}
}

// H1: retained inbox messages were replayed to the handler on every
// reconnect. Only presence may be retained.
func TestRetainIgnoredOutsidePresence(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", nil)
	alice := b.rawConnect("alice", func(paho.Client, paho.Message) {})
	rawPublish(t, alice, a2a.InboxTopic("bob"), true, envJSON(`"id":"r1","type":"event","from":"alice","to":"bob"`))

	got := make(chan string, 8)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) { got <- m.Topic() })
	suback(t, ops, a2a.InboxTopic("bob")) // retained messages arrive right after subscribing
	select {
	case topic := <-got:
		t.Fatalf("retained message delivered on %s", topic)
	case <-time.After(300 * time.Millisecond):
	}
}

// H2: encoding/json matches keys case-insensitively and keeps the last
// duplicate, so the broker and a case-sensitive receiver could disagree
// about "from". The broker now forwards its own canonical encoding.
func TestEnvelopeForwardedCanonical(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", nil)
	payloads := make(chan []byte, 8)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) { payloads <- m.Payload() })
	suback(t, ops, a2a.InboxTopic("bob"))

	alice := b.rawConnect("alice", func(paho.Client, paho.Message) {})
	rawPublish(t, alice, a2a.InboxTopic("bob"), false,
		envJSON(`"id":"dup1","type":"event","from":"bob","FROM":"alice","to":"bob","extra":"dropped"`))

	select {
	case p := <-payloads:
		var raw map[string]any
		if err := json.Unmarshal(p, &raw); err != nil {
			t.Fatal(err)
		}
		if raw["from"] != "alice" || strings.Contains(string(p), `"FROM"`) || strings.Contains(string(p), "extra") {
			t.Errorf("forwarded payload is not canonical: %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message not delivered")
	}
}

// M6: task queues may only be consumed through the capability's own
// $share group; anything else would copy every task.
func TestTaskSubscriptionMustBeExactShare(t *testing.T) {
	b := startBus(t, nil)
	bob := b.rawConnect("bob", func(paho.Client, paho.Message) {})
	for _, f := range []string{a2a.TaskTopic("summarize"), "$share/other/" + a2a.TaskTopic("summarize")} {
		if code := suback(t, bob, f); code < 0x80 {
			t.Errorf("bob allowed to subscribe to %q", f)
		}
	}
	if code := suback(t, bob, a2a.TaskShareFilter("summarize")); code >= 0x80 {
		t.Errorf("bob refused the proper share filter: 0x%02x", code)
	}
}

func remoteCode(t *testing.T, err error) string {
	t.Helper()
	var re *client.RemoteError
	if !errors.As(err, &re) {
		t.Fatalf("want a RemoteError, got %v", err)
	}
	return re.Code
}

// M4: reply_to may only name the sender's own inbox.
func TestReplyToMustBeSenderInbox(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", func(context.Context, *a2a.Envelope) (any, error) { return "ok", nil })
	inbox := newRecorder()
	alice := b.connect("alice", inbox.handler)

	env, _ := a2a.New(a2a.TypeRequest, "alice", "hi")
	env.To, env.ReplyTo = "bob", a2a.InboxTopic("carol")
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	rej := inbox.next(t)
	var body broker.RejectBody
	_ = rej.DecodeBody(&body)
	if rej.From != a2a.BrokerID || !strings.Contains(body.Reason, "reply_to") {
		t.Fatalf("want reply_to rejection, got %+v %+v", rej, body)
	}
}

// M7: a far-future ts would never expire.
func TestClockSkewRejected(t *testing.T) {
	b := startBus(t, nil)
	b.connect("bob", nil)
	inbox := newRecorder()
	alice := b.connect("alice", inbox.handler)

	env, _ := a2a.New(a2a.TypeEvent, "alice", "from the future")
	env.To, env.TS = "bob", time.Now().Add(time.Hour)
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	var body broker.RejectBody
	if rej := inbox.next(t); rej.DecodeBody(&body) != nil || body.Code != "invalid" || !strings.Contains(body.Reason, "ts is") {
		t.Fatalf("want clock skew rejection, got %+v", body)
	}
}

// Fast failure instead of a silent timeout.
func TestNoRouteAndNoConsumer(t *testing.T) {
	b := startBus(t, nil)
	alice := b.connect("alice", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := alice.Request(ctx, "carol", "hi"); remoteCode(t, err) != "no_route" { // configured, never connected
		t.Errorf("offline agent: %v", err)
	}
	if _, err := alice.Request(ctx, "nobody", "hi"); remoteCode(t, err) != "no_route" {
		t.Errorf("unknown agent: %v", err)
	}
	if _, err := alice.RequestTask(ctx, "summarize", "hi"); remoteCode(t, err) != "no_consumer" {
		t.Errorf("unserved capability: %v", err)
	}
}

func presenceOf(t *testing.T, b *bus, agent string) chan a2a.Presence {
	t.Helper()
	cards := make(chan a2a.Presence, 8)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) {
		if p, err := a2a.ParsePresence(m.Payload()); err == nil {
			cards <- *p
		}
	})
	suback(t, ops, a2a.PresenceTopic(agent))
	return cards
}

func waitCard(t *testing.T, cards chan a2a.Presence, ok func(a2a.Presence) bool) a2a.Presence {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case p := <-cards:
			if ok(p) {
				return p
			}
		case <-deadline:
			t.Fatal("no matching presence card")
		}
	}
}

// L3: presence may only advertise capabilities the broker grants.
func TestPresenceCapabilitiesLimitedToGrants(t *testing.T) {
	b := startBus(t, nil)
	cards := presenceOf(t, b, "alice")
	alice := b.rawConnect("alice", func(paho.Client, paho.Message) {})
	rawPublish(t, alice, a2a.PresenceTopic("alice"), true, `{"v":1,"agent":"alice","capabilities":["summarize","admin"]}`)
	p := waitCard(t, cards, func(p a2a.Presence) bool { return p.Status == a2a.StatusOnline })
	if len(p.Capabilities) != 0 {
		t.Errorf("ungranted capabilities advertised: %v", p.Capabilities)
	}
}

func key(b byte) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string(rune(b)), 32)))
}

// Pinned keys: the broker rejects cards with other keys and marks
// matching ones keys_pinned. Unpinned keys are never marked pinned.
func TestPresenceKeyPinning(t *testing.T) {
	pinned := &a2a.Keys{Sign: key('s'), KEX: key('k')}
	b := startBus(t, func(c *broker.Config) {
		for i := range c.Agents {
			if c.Agents[i].ID == "bob" {
				c.Agents[i].Keys = pinned
			}
		}
	})
	cards := make(chan a2a.Presence, 16)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) {
		if p, err := a2a.ParsePresence(m.Payload()); err == nil {
			cards <- *p
		}
	})
	suback(t, ops, a2a.PresenceFilter)

	bob := b.rawConnect("bob", func(paho.Client, paho.Message) {})
	rawPublish(t, bob, a2a.PresenceTopic("bob"), true, `{"v":1,"agent":"bob","description":"wrong keys","keys":{"sign":"`+key('x')+`"}}`)
	rawPublish(t, bob, a2a.PresenceTopic("bob"), true, `{"v":1,"agent":"bob","description":"no keys sent"}`)
	alice := b.rawConnect("alice", func(paho.Client, paho.Message) {})
	rawPublish(t, alice, a2a.PresenceTopic("alice"), true, `{"v":1,"agent":"alice","keys_pinned":true,"keys":{"kex":"`+key('a')+`"}}`)

	var bobCard, aliceCard *a2a.Presence
	deadline := time.After(3 * time.Second)
	for bobCard == nil || aliceCard == nil {
		select {
		case p := <-cards:
			switch {
			case p.Agent == "bob" && p.Description == "wrong keys":
				t.Fatal("card with keys that differ from the pinned keys was accepted")
			case p.Agent == "bob":
				bobCard = &p
			case p.Agent == "alice" && p.Status == a2a.StatusOnline:
				aliceCard = &p
			}
		case <-deadline:
			t.Fatalf("missing cards: bob %v alice %v\n%s", bobCard, aliceCard, b.logs.String())
		}
	}
	if !bobCard.KeysPinned || bobCard.Keys == nil || *bobCard.Keys != *pinned {
		t.Errorf("want the pinned keys filled in and keys_pinned, got %+v (keys %+v)", bobCard, bobCard.Keys)
	}
	if aliceCard.KeysPinned {
		t.Error("self-asserted keys marked pinned")
	}
}

func TestPerAgentRateOverride(t *testing.T) {
	b := startBus(t, func(c *broker.Config) {
		for i := range c.Agents {
			if c.Agents[i].ID == "alice" {
				c.Agents[i].RatePerSecond, c.Agents[i].RateBurst = 0.01, 2 // presence + 1
			}
		}
	})
	b.connect("bob", nil)
	inbox := newRecorder()
	alice := b.connect("alice", inbox.handler)
	for range 2 {
		if err := alice.Send(ctxT(t), "bob", "x"); err != nil {
			t.Fatal(err)
		}
	}
	var body broker.RejectBody
	if rej := inbox.next(t); rej.DecodeBody(&body) != nil || body.Code != "rate_limited" {
		t.Fatalf("want rate_limited for alice, got %+v", body)
	}
}

// M1: a socket that never sends CONNECT is closed, and per-IP caps hold.
func TestPreAuthTimeoutAndConnectionCap(t *testing.T) {
	b := startBus(t, func(c *broker.Config) {
		c.ConnectTimeout = broker.Duration(200 * time.Millisecond)
		c.MaxConnectionsPerIP = 2
	})
	addr := strings.TrimPrefix(b.url, "tcp://")

	idle, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	start := time.Now()
	_ = idle.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("idle pre-auth socket not closed promptly (err %v after %s)", err, time.Since(start))
	}

	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	time.Sleep(50 * time.Millisecond) // let the broker accept them
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	_ = third.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, err := third.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("third connection from one IP was not closed (err %v)", err)
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// H3: plaintext on a non-loopback address must be opted into.
func TestPlaintextNonLoopbackRefused(t *testing.T) {
	_, hash := broker.NewToken()
	base := func(listen string) *broker.Config {
		return &broker.Config{Listen: listen, Agents: []broker.AgentConfig{{ID: "a", TokenSHA256: hash}}}
	}
	if _, err := broker.New(base(":0"), quietLog()); err == nil {
		t.Error(`listen ":0" without TLS was accepted`)
	}
	c := base(":0")
	c.AllowInsecure = true
	if srv, err := broker.New(c, quietLog()); err != nil {
		t.Errorf("allow_insecure: %v", err)
	} else {
		_ = srv.Close()
	}
	if srv, err := broker.New(base("127.0.0.1:0"), quietLog()); err != nil {
		t.Errorf("loopback: %v", err)
	} else {
		_ = srv.Close()
	}
}
