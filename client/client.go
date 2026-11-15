// Package client is a Go client for the a2a/v1 agent bus.
//
// It speaks MQTT 3.1.1 (via paho.mqtt.golang, which conduit already uses),
// so request/reply, correlation and TTLs travel in the envelope rather
// than in MQTT 5 properties.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/mqtt/a2a"
)

// Handler handles an envelope delivered to this agent. For requests, the
// returned value becomes the response body; a returned error is sent back
// as an error envelope. For other types the return values are ignored.
//
// ctx is cancelled when the envelope's TTL runs out or the client closes.
type Handler func(ctx context.Context, env *a2a.Envelope) (reply any, err error)

// Options configures a client.
type Options struct {
	// BrokerURL is "tcp://host:1883" or "ssl://host:8883".
	BrokerURL string
	// AgentID and Token identify this agent to the broker.
	AgentID string
	Token   string
	TLS     *tls.Config

	// Inbox handles requests and events sent directly to this agent, plus
	// responses and errors that match no pending Request (for example the
	// broker rejecting a Send or Emit). If nil, requests are answered with
	// an error and everything else is dropped.
	Inbox Handler

	// Description and Meta are published on this agent's presence card.
	Description string
	Meta        map[string]string

	// CleanSession discards queued inbox messages on reconnect. The default
	// (false) keeps a persistent session so messages sent while this agent
	// was offline are delivered when it returns.
	CleanSession bool

	// QoS for publishes and subscriptions. Defaults to 1.
	QoS byte

	Logger *slog.Logger
}

// Client is a connected agent.
type Client struct {
	opts Options
	mqtt paho.Client
	log  *slog.Logger
	qos  byte

	ctx       context.Context
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once

	mu       sync.Mutex
	pending  map[string]chan *a2a.Envelope
	tasks    map[string]Handler
	watchers []watcher
	agents   map[string]a2a.Presence
}

type watcher struct {
	filter string
	fn     func(*a2a.Envelope)
}

// Connect connects to the broker, subscribes to this agent's inbox and to
// presence, and publishes this agent's presence card.
func Connect(ctx context.Context, opts Options) (*Client, error) {
	if !a2a.ValidName(opts.AgentID) {
		return nil, fmt.Errorf("invalid agent id %q", opts.AgentID)
	}
	if opts.QoS == 0 {
		opts.QoS = 1
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	cctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		opts:    opts,
		log:     opts.Logger.With("agent", opts.AgentID),
		qos:     opts.QoS,
		ctx:     cctx,
		cancel:  cancel,
		ready:   make(chan struct{}),
		pending: map[string]chan *a2a.Envelope{},
		tasks:   map[string]Handler{},
		agents:  map[string]a2a.Presence{},
	}

	mo := paho.NewClientOptions().
		AddBroker(opts.BrokerURL).
		SetClientID(opts.AgentID).
		SetUsername(opts.AgentID).
		SetPassword(opts.Token).
		SetProtocolVersion(4).
		SetCleanSession(opts.CleanSession).
		SetOrderMatters(false). // handlers may run long (LLM calls)
		SetAutoReconnect(true).
		SetMaxReconnectInterval(time.Minute).
		SetKeepAlive(30 * time.Second).
		SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) { c.dispatch(m) }).
		SetOnConnectHandler(func(paho.Client) { go c.onConnect() }).
		SetConnectionLostHandler(func(_ paho.Client, err error) { c.log.Warn("connection lost", "err", err) })
	if opts.TLS != nil {
		mo.SetTLSConfig(opts.TLS)
	}
	c.mqtt = paho.NewClient(mo)

	tok := c.mqtt.Connect()
	if err := wait(ctx, tok); err != nil {
		cancel()
		return nil, fmt.Errorf("connect %s: %w", opts.BrokerURL, err)
	}
	select { // first subscribe and announce done
	case <-c.ready:
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	}
	return c, nil
}

// ID is this agent's ID.
func (c *Client) ID() string { return c.opts.AgentID }

// Close disconnects. The broker publishes this agent's offline card.
func (c *Client) Close() {
	c.cancel()
	c.mqtt.Disconnect(250)
}

// onConnect (re)subscribes and (re)announces after every connect.
func (c *Client) onConnect() {
	filters := map[string]byte{
		a2a.InboxTopic(c.opts.AgentID): c.qos,
		a2a.PresenceFilter:             c.qos,
	}
	c.mu.Lock()
	for capability := range c.tasks {
		filters[a2a.TaskShareFilter(capability)] = c.qos
	}
	for _, w := range c.watchers {
		filters[w.filter] = c.qos
	}
	c.mu.Unlock()
	if err := c.subscribe(c.ctx, filters); err != nil {
		c.log.Error("subscribe", "err", err)
	}
	if err := c.announce(c.ctx); err != nil {
		c.log.Error("announce presence", "err", err)
	}
	c.readyOnce.Do(func() { close(c.ready) })
}

// announce publishes this agent's presence card.
func (c *Client) announce(ctx context.Context) error {
	c.mu.Lock()
	caps := make([]string, 0, len(c.tasks))
	for capability := range c.tasks {
		caps = append(caps, capability)
	}
	c.mu.Unlock()
	sort.Strings(caps)
	p := a2a.Presence{
		V: a2a.Version, Agent: c.opts.AgentID, Status: a2a.StatusOnline, TS: time.Now().UTC(),
		Description: c.opts.Description, Capabilities: caps, Meta: c.opts.Meta,
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return wait(ctx, c.mqtt.Publish(a2a.PresenceTopic(c.opts.AgentID), c.qos, true, data))
}

// Publish sends env to topic as is. Most callers want Send, Request,
// RequestTask or Emit instead.
func (c *Client) Publish(ctx context.Context, topic string, env *a2a.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	return wait(ctx, c.mqtt.Publish(topic, c.qos, false, data))
}

// Send delivers a one-way event to another agent's inbox.
func (c *Client) Send(ctx context.Context, to string, body any) error {
	env, err := a2a.New(a2a.TypeEvent, c.opts.AgentID, body)
	if err != nil {
		return err
	}
	env.To = to
	setTTL(ctx, env)
	return c.Publish(ctx, a2a.InboxTopic(to), env)
}

// Request sends a request to another agent's inbox and waits for the
// response. The context deadline, if any, becomes the envelope TTL.
// A TypeError reply is returned as a *RemoteError.
func (c *Client) Request(ctx context.Context, to string, body any) (*a2a.Envelope, error) {
	env, err := a2a.New(a2a.TypeRequest, c.opts.AgentID, body)
	if err != nil {
		return nil, err
	}
	env.To = to
	return c.roundTrip(ctx, a2a.InboxTopic(to), env)
}

// RequestTask sends a request to the work queue for capability. Exactly
// one worker consuming that capability handles it.
func (c *Client) RequestTask(ctx context.Context, capability string, body any) (*a2a.Envelope, error) {
	env, err := a2a.New(a2a.TypeRequest, c.opts.AgentID, body)
	if err != nil {
		return nil, err
	}
	env.Capability = capability
	return c.roundTrip(ctx, a2a.TaskTopic(capability), env)
}

func (c *Client) roundTrip(ctx context.Context, topic string, env *a2a.Envelope) (*a2a.Envelope, error) {
	setTTL(ctx, env)
	ch := make(chan *a2a.Envelope, 1)
	c.mu.Lock()
	c.pending[env.ID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, env.ID)
		c.mu.Unlock()
	}()

	if err := c.Publish(ctx, topic, env); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		if reply.Type == a2a.TypeError {
			return reply, newRemoteError(reply)
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errors.New("client closed")
	}
}

// Emit publishes an event on this agent's event topic name (which may
// contain '/'), for any agent watching it.
func (c *Client) Emit(ctx context.Context, name string, body any) error {
	env, err := a2a.New(a2a.TypeEvent, c.opts.AgentID, body)
	if err != nil {
		return err
	}
	setTTL(ctx, env)
	return c.Publish(ctx, a2a.EventTopic(c.opts.AgentID, name), env)
}

// ServeTasks consumes the work queue for capability through a shared
// subscription and adds capability to this agent's presence card. The
// broker must allow this agent to consume capability.
func (c *Client) ServeTasks(ctx context.Context, capability string, h Handler) error {
	if !a2a.ValidName(capability) {
		return fmt.Errorf("invalid capability %q", capability)
	}
	c.mu.Lock()
	c.tasks[capability] = h
	c.mu.Unlock()
	if err := c.subscribe(ctx, map[string]byte{a2a.TaskShareFilter(capability): c.qos}); err != nil {
		c.mu.Lock()
		delete(c.tasks, capability)
		c.mu.Unlock()
		return err
	}
	return c.announce(ctx)
}

// WatchEvents calls fn for each event matching filter, an MQTT filter
// under a2a/v1/events/ such as "a2a/v1/events/+/jobs/#".
func (c *Client) WatchEvents(ctx context.Context, filter string, fn func(*a2a.Envelope)) error {
	if t, err := a2a.ParseTopic(filter); err != nil || t.Kind != a2a.KindEvent {
		return fmt.Errorf("%q is not an event filter", filter)
	}
	c.mu.Lock()
	c.watchers = append(c.watchers, watcher{filter: filter, fn: fn})
	c.mu.Unlock()
	return c.subscribe(ctx, map[string]byte{filter: c.qos})
}

// Agents returns the last known presence card of every agent, sorted by ID.
func (c *Client) Agents() []a2a.Presence {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]a2a.Presence, 0, len(c.agents))
	for _, p := range c.agents {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b a2a.Presence) int { return strings.Compare(a.Agent, b.Agent) })
	return out
}

// dispatch routes an incoming MQTT message by topic.
func (c *Client) dispatch(m paho.Message) {
	t, err := a2a.ParseTopic(m.Topic())
	if err != nil {
		c.log.Warn("message on unknown topic", "topic", m.Topic())
		return
	}
	if t.Kind == a2a.KindPresence {
		p, err := a2a.ParsePresence(m.Payload())
		if err != nil {
			c.log.Warn("bad presence card", "topic", m.Topic(), "err", err)
			return
		}
		c.mu.Lock()
		c.agents[p.Agent] = *p
		c.mu.Unlock()
		return
	}
	env, err := a2a.Parse(m.Payload())
	if err != nil {
		c.log.Warn("bad envelope", "topic", m.Topic(), "err", err)
		return
	}
	if env.Expired(time.Now()) {
		c.log.Info("expired envelope dropped", "id", env.ID, "from", env.From)
		return
	}
	switch t.Kind {
	case a2a.KindInbox:
		if c.resolve(env) {
			return
		}
		c.handle(env, c.opts.Inbox)
	case a2a.KindTask:
		c.mu.Lock()
		h := c.tasks[t.Capability]
		c.mu.Unlock()
		c.handle(env, h)
	case a2a.KindEvent:
		c.mu.Lock()
		ws := slices.Clone(c.watchers)
		c.mu.Unlock()
		for _, w := range ws {
			if topicMatches(w.filter, m.Topic()) {
				w.fn(env)
			}
		}
	}
}

// resolve hands a response or error to the Request waiting for it.
func (c *Client) resolve(env *a2a.Envelope) bool {
	if env.Type != a2a.TypeResponse && env.Type != a2a.TypeError {
		return false
	}
	c.mu.Lock()
	ch, ok := c.pending[env.CorrelationID]
	c.mu.Unlock()
	if !ok {
		return false // e.g. the broker rejecting a Send; goes to the Inbox handler
	}
	select {
	case ch <- env:
	default:
	}
	return true
}

func (c *Client) handle(env *a2a.Envelope, h Handler) {
	if env.Type != a2a.TypeRequest {
		if h != nil {
			_, _ = h(c.ctx, env)
		}
		return
	}
	ctx, cancel := c.ctx, context.CancelFunc(func() {})
	if env.TTLMillis > 0 {
		ctx, cancel = context.WithDeadline(c.ctx, env.TS.Add(time.Duration(env.TTLMillis)*time.Millisecond))
	}
	defer cancel()

	var reply *a2a.Envelope
	var err error
	if h == nil {
		reply, err = env.Reply(a2a.TypeError, c.opts.AgentID, ErrorBody{Code: "no_handler", Reason: "agent does not accept this request"})
	} else if body, herr := h(ctx, env); herr != nil {
		reply, err = env.Reply(a2a.TypeError, c.opts.AgentID, ErrorBody{Code: "handler_error", Reason: herr.Error()})
	} else {
		reply, err = env.Reply(a2a.TypeResponse, c.opts.AgentID, body)
	}
	if err != nil {
		c.log.Error("build reply", "id", env.ID, "err", err)
		return
	}
	pubCtx, pubCancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer pubCancel()
	if err := c.Publish(pubCtx, env.ReplyTopic(), reply); err != nil {
		c.log.Error("send reply", "id", env.ID, "to", env.From, "err", err)
	}
}

// ErrorBody is the body of an error envelope.
type ErrorBody struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// RemoteError is a TypeError reply from another agent or the broker.
type RemoteError struct {
	From   string
	Code   string
	Reason string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("%s: %s: %s", e.From, e.Code, e.Reason)
}

func newRemoteError(env *a2a.Envelope) *RemoteError {
	var b ErrorBody
	_ = env.DecodeBody(&b)
	return &RemoteError{From: env.From, Code: b.Code, Reason: b.Reason}
}

// setTTL derives the envelope TTL from the context deadline.
func setTTL(ctx context.Context, env *a2a.Envelope) {
	if d, ok := ctx.Deadline(); ok && env.TTLMillis == 0 {
		env.TTLMillis = max(1, time.Until(d).Milliseconds())
	}
}

// codePacketIDInUse is the SUBACK code mochi-mqtt returns when the
// client's SUBSCRIBE packet ID matches one of the broker's own in-flight
// deliveries to this client (mochi shares one in-flight table for both
// directions). It is transient: a retry gets a new packet ID.
const codePacketIDInUse = 0x91

// subscribe subscribes and fails if the broker refuses any filter.
func (c *Client) subscribe(ctx context.Context, filters map[string]byte) error {
	for attempt := 1; ; attempt++ {
		tok := c.mqtt.SubscribeMultiple(filters, nil)
		if err := wait(ctx, tok); err != nil {
			return err
		}
		retry := map[string]byte{}
		for filter, code := range tok.(*paho.SubscribeToken).Result() {
			switch {
			case code == codePacketIDInUse && attempt < 5:
				retry[filter] = filters[filter]
			case code >= 0x80:
				return fmt.Errorf("broker refused subscription to %q (code 0x%02x)", filter, code)
			}
		}
		if len(retry) == 0 {
			return nil
		}
		filters = retry
		select {
		case <-time.After(time.Duration(attempt) * 20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// wait blocks on a paho token until it completes or ctx ends.
func wait(ctx context.Context, tok paho.Token) error {
	select {
	case <-tok.Done():
		return tok.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

// topicMatches reports whether topic matches an MQTT filter.
func topicMatches(filter, topic string) bool {
	fs, ts := strings.Split(filter, "/"), strings.Split(topic, "/")
	for i, f := range fs {
		if f == "#" {
			return true
		}
		if i >= len(ts) || (f != "+" && f != ts[i]) {
			return false
		}
	}
	return len(fs) == len(ts)
}
