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
	"maps"
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

	// Tasks maps capabilities to handlers for this agent's work queues.
	// Long-running workers should register here rather than with
	// ServeTasks: with a persistent session the broker redelivers queued
	// tasks the moment the agent connects, before any ServeTasks call
	// could run, and a task with no handler is dropped without a reply.
	Tasks map[string]Handler

	// Description and Meta are published on this agent's presence card.
	Description string
	Meta        map[string]string

	// CleanSession discards queued inbox messages on reconnect. The default
	// (false) keeps a persistent session so messages sent while this agent
	// was offline are delivered when it returns.
	CleanSession bool

	// Keys are this agent's public keys, published on its presence card
	// for other agents to verify signatures and encrypt to.
	Keys *a2a.Keys

	// QoS for publishes and subscriptions. Defaults to 1.
	QoS byte

	// MaxConcurrent caps handlers running at once (inbox and tasks
	// together). When full, requests are answered with a "busy" error and
	// other messages are dropped. Default 64.
	MaxConcurrent int
	// DefaultTimeout bounds a handler when the request has no ttl_ms.
	// Default 5 minutes.
	DefaultTimeout time.Duration

	// BeforePublish, if set, runs before every envelope this client
	// publishes, replies included. Returning an error stops the publish.
	// Use it for approval gating, e.g. conduit's approval flow for
	// agent-initiated sends.
	BeforePublish func(ctx context.Context, topic string, env *a2a.Envelope) error

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
	setupErr  error // from the first onConnect; read after ready closes

	slots chan struct{} // MaxConcurrent handler slots
	seen  *seenIDs      // client-side dedupe of redelivered envelopes

	mu       sync.Mutex
	pending  map[string]pendingReq
	tasks    map[string]Handler
	watchers []watcher
	raw      []rawWatcher
	agents   map[string]a2a.Presence
}

// pendingReq is a Request waiting for its reply. from is the agent the
// reply must come from ("" for task requests, which any worker answers).
type pendingReq struct {
	ch   chan *a2a.Envelope
	from string
}

type rawWatcher struct {
	filter string
	fn     func(topic string, payload []byte)
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
	for capability := range opts.Tasks {
		if !a2a.ValidName(capability) {
			return nil, fmt.Errorf("invalid capability %q in Tasks", capability)
		}
	}
	if opts.QoS == 0 {
		opts.QoS = 1
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = 64
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = 5 * time.Minute
	}
	cctx, cancel := context.WithCancel(context.Background())
	tasks := maps.Clone(opts.Tasks)
	if tasks == nil {
		tasks = map[string]Handler{}
	}
	c := &Client{
		opts:    opts,
		log:     opts.Logger.With("agent", opts.AgentID),
		qos:     opts.QoS,
		ctx:     cctx,
		cancel:  cancel,
		ready:   make(chan struct{}),
		slots:   make(chan struct{}, opts.MaxConcurrent),
		seen:    newSeenIDs(10*time.Minute, 10000),
		pending: map[string]pendingReq{},
		tasks:   tasks,
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
	if c.setupErr != nil {
		c.Close()
		return nil, c.setupErr
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
	for _, w := range c.raw {
		filters[w.filter] = c.qos
	}
	c.mu.Unlock()
	err := c.subscribe(c.ctx, filters)
	if err != nil {
		c.log.Error("subscribe", "err", err)
	}
	if aerr := c.announce(c.ctx); aerr != nil {
		c.log.Error("announce presence", "err", aerr)
		err = errors.Join(err, aerr)
	}
	c.readyOnce.Do(func() {
		c.setupErr = err
		close(c.ready)
	})
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
		Description: c.opts.Description, Capabilities: caps, Meta: c.opts.Meta, Keys: c.opts.Keys,
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
	if c.opts.BeforePublish != nil {
		if err := c.opts.BeforePublish(ctx, topic, env); err != nil {
			return fmt.Errorf("publish blocked: %w", err)
		}
	}
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	return wait(ctx, c.mqtt.Publish(topic, c.qos, false, data))
}

// MsgOption sets optional envelope fields on Send, Request, RequestTask
// and Emit.
type MsgOption func(*a2a.Envelope)

// WithPriority sets the envelope priority (high, normal, low, info).
func WithPriority(p a2a.Priority) MsgOption { return func(e *a2a.Envelope) { e.Priority = p } }

// WithEvent names what the message is about, e.g. "doorbell_ring".
func WithEvent(name string) MsgOption { return func(e *a2a.Envelope) { e.Event = name } }

// WithHeaders adds string headers, e.g. {"session": "tg:12345"}.
func WithHeaders(h map[string]string) MsgOption {
	return func(e *a2a.Envelope) {
		if e.Headers == nil {
			e.Headers = map[string]string{}
		}
		for k, v := range h {
			e.Headers[k] = v
		}
	}
}

func (c *Client) build(ctx context.Context, typ a2a.Type, body any, opts []MsgOption) (*a2a.Envelope, error) {
	env, err := a2a.New(typ, c.opts.AgentID, body)
	if err != nil {
		return nil, err
	}
	for _, o := range opts {
		o(env)
	}
	setTTL(ctx, env)
	return env, nil
}

// Send delivers a one-way event to another agent's inbox.
func (c *Client) Send(ctx context.Context, to string, body any, opts ...MsgOption) error {
	env, err := c.build(ctx, a2a.TypeEvent, body, opts)
	if err != nil {
		return err
	}
	env.To = to
	return c.Publish(ctx, a2a.InboxTopic(to), env)
}

// Request sends a request to another agent's inbox and waits for the
// response. The context deadline, if any, becomes the envelope TTL.
// A TypeError reply is returned as a *RemoteError. Only a reply from the
// addressed agent (or a $broker error) completes the request.
func (c *Client) Request(ctx context.Context, to string, body any, opts ...MsgOption) (*a2a.Envelope, error) {
	env, err := c.build(ctx, a2a.TypeRequest, body, opts)
	if err != nil {
		return nil, err
	}
	env.To = to
	return c.roundTrip(ctx, a2a.InboxTopic(to), env, to)
}

// RequestTask sends a request to the work queue for capability. Exactly
// one worker consuming that capability handles it.
func (c *Client) RequestTask(ctx context.Context, capability string, body any, opts ...MsgOption) (*a2a.Envelope, error) {
	env, err := c.build(ctx, a2a.TypeRequest, body, opts)
	if err != nil {
		return nil, err
	}
	env.Capability = capability
	return c.roundTrip(ctx, a2a.TaskTopic(capability), env, "")
}

func (c *Client) roundTrip(ctx context.Context, topic string, env *a2a.Envelope, from string) (*a2a.Envelope, error) {
	ch := make(chan *a2a.Envelope, 1)
	c.mu.Lock()
	c.pending[env.ID] = pendingReq{ch: ch, from: from}
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
func (c *Client) Emit(ctx context.Context, name string, body any, opts ...MsgOption) error {
	env, err := c.build(ctx, a2a.TypeEvent, body, opts)
	if err != nil {
		return err
	}
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

// Observe calls fn with the topic and raw payload of every message
// matching filter, any MQTT filter such as "#" or "a2a/v1/#". It's for
// monitoring tools: only agents configured as observers may subscribe
// outside their own inbox, presence, events and granted task queues, and
// the broker refuses the rest. Messages seen this way are not handled as
// this agent's own: another agent's request is never answered here.
func (c *Client) Observe(ctx context.Context, filter string, fn func(topic string, payload []byte)) error {
	c.mu.Lock()
	c.raw = append(c.raw, rawWatcher{filter: filter, fn: fn})
	c.mu.Unlock()
	if err := c.subscribe(ctx, map[string]byte{filter: c.qos}); err != nil {
		c.mu.Lock()
		c.raw = c.raw[:len(c.raw)-1]
		c.mu.Unlock()
		return err
	}
	return nil
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
	c.mu.Lock()
	raws := slices.Clone(c.raw)
	c.mu.Unlock()
	observed := false
	for _, w := range raws {
		if topicMatches(w.filter, m.Topic()) {
			observed = true
			c.safelyRaw(w, m)
		}
	}
	t, err := a2a.ParseTopic(m.Topic())
	if err != nil {
		if !observed { // topics outside a2a/v1 are expected through Observe
			c.log.Warn("message on unknown topic", "topic", m.Topic())
		}
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
	// QoS 1 can redeliver after a reconnect; handle each envelope once.
	if !c.seen.first(env.From+"/"+env.ID, time.Now()) {
		return
	}
	switch t.Kind {
	case a2a.KindInbox:
		if t.Owner != c.opts.AgentID {
			return // another agent's inbox, seen through Observe
		}
		if c.resolve(env) {
			return
		}
		c.handle(env, c.opts.Inbox)
	case a2a.KindTask:
		c.mu.Lock()
		h, serving := c.tasks[t.Capability]
		c.mu.Unlock()
		if !serving {
			return // a task seen through Observe; it belongs to the workers
		}
		c.handle(env, h)
	case a2a.KindEvent:
		c.mu.Lock()
		ws := slices.Clone(c.watchers)
		c.mu.Unlock()
		for _, w := range ws {
			if topicMatches(w.filter, m.Topic()) {
				c.safely("watcher", env, func() { w.fn(env) })
			}
		}
	}
}

// resolve hands a response or error to the Request waiting for it. A
// reply only counts if it comes from the agent the request went to (any
// agent for task requests) or from the broker; anything else could be
// another agent racing a forged reply with a guessed correlation ID.
func (c *Client) resolve(env *a2a.Envelope) bool {
	if env.Type != a2a.TypeResponse && env.Type != a2a.TypeError {
		return false
	}
	c.mu.Lock()
	p, ok := c.pending[env.CorrelationID]
	c.mu.Unlock()
	if !ok {
		return false // e.g. the broker rejecting a Send; goes to the Inbox handler
	}
	if p.from != "" && env.From != p.from && env.From != a2a.BrokerID {
		c.log.Warn("reply from unexpected agent ignored", "correlation_id", env.CorrelationID, "from", env.From, "want", p.from)
		return true
	}
	select {
	case p.ch <- env:
	default:
	}
	return true
}

func (c *Client) safelyRaw(w rawWatcher, m paho.Message) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("observer panicked", "topic", m.Topic(), "panic", fmt.Sprint(r))
		}
	}()
	w.fn(m.Topic(), m.Payload())
}

// safely runs fn, turning a panic into a log line so one bad message
// can't take down the process hosting the agent.
func (c *Client) safely(what string, env *a2a.Envelope, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error(what+" panicked", "id", env.ID, "from", env.From, "panic", fmt.Sprint(r))
			panicked = true
		}
	}()
	fn()
	return false
}

func (c *Client) handle(env *a2a.Envelope, h Handler) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		c.log.Warn("handler limit reached", "id", env.ID, "from", env.From, "type", env.Type, "max", c.opts.MaxConcurrent)
		if env.Type == a2a.TypeRequest {
			c.reply(env, a2a.TypeError, ErrorBody{Code: "busy", Reason: "agent is at its concurrent handler limit; retry later"})
		}
		return
	}

	deadline := time.Now().Add(c.opts.DefaultTimeout)
	if env.TTLMillis > 0 {
		deadline = env.TS.Add(time.Duration(env.TTLMillis) * time.Millisecond)
	}
	ctx, cancel := context.WithDeadline(c.ctx, deadline)
	defer cancel()

	if env.Type != a2a.TypeRequest {
		if h != nil {
			c.safely("handler", env, func() { _, _ = h(ctx, env) })
		}
		return
	}
	if h == nil {
		c.reply(env, a2a.TypeError, ErrorBody{Code: "no_handler", Reason: "agent does not accept this request"})
		return
	}
	var body any
	var herr error
	if c.safely("handler", env, func() { body, herr = h(ctx, env) }) {
		c.reply(env, a2a.TypeError, ErrorBody{Code: "handler_panic", Reason: "handler failed"})
		return
	}
	if herr != nil {
		c.reply(env, a2a.TypeError, ErrorBody{Code: "handler_error", Reason: herr.Error()})
		return
	}
	c.reply(env, a2a.TypeResponse, body)
}

// reply answers env.
func (c *Client) reply(env *a2a.Envelope, typ a2a.Type, body any) {
	reply, err := env.Reply(typ, c.opts.AgentID, body)
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
		env.TTLMillis = min(max(1, time.Until(d).Milliseconds()), a2a.MaxTTL.Milliseconds())
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

// seenIDs remembers recent envelope keys, bounded in time and size.
type seenIDs struct {
	mu     sync.Mutex
	window time.Duration
	max    int
	at     map[string]time.Time
}

func newSeenIDs(window time.Duration, max int) *seenIDs {
	return &seenIDs{window: window, max: max, at: map[string]time.Time{}}
}

func (s *seenIDs) first(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.at[key]; ok && now.Sub(t) <= s.window {
		return false
	}
	if len(s.at) >= s.max {
		for k, t := range s.at {
			if now.Sub(t) > s.window {
				delete(s.at, k)
			}
		}
		if len(s.at) >= s.max {
			clear(s.at) // still full of recent IDs: start over rather than grow
		}
	}
	s.at[key] = now
	return true
}
