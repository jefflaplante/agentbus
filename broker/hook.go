package broker

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/jefflaplante/agentbus/a2a"
)

// Limits that aren't worth configuring.
const (
	maxSubscriptions = 256 // per client
	logIDLen         = 64  // attacker-chosen IDs are truncated in logs
	// refusedFilter replaces a subscription filter the contract forbids.
	// It is not a valid MQTT filter, so mochi refuses it in the SUBACK.
	refusedFilter = "a2a/v1/#/refused"
)

// contractHook enforces the a2a/v1 contract on a mochi-mqtt server.
type contractHook struct {
	mqtt.HookBase

	// st is the current config and lookups. Reload swaps it atomically;
	// each check loads it once and works from that snapshot.
	st     atomic.Pointer[hookState]
	server *mqtt.Server
	log    *slog.Logger
	now    func() time.Time

	rate  *rateLimiter // publishes, per agent
	noise *rateLimiter // reject logs and error envelopes, per agent; auth-failure logs, per IP
	dedup *dedupe
	open  *openRequests // accepted requests awaiting their one reply
	audit *auditLog     // nil when audit_file is unset

	counters *counters // decision counts for Status

	// closing is set when the broker starts shutting down; logins are
	// refused from then on.
	closing atomic.Bool

	// reconnects marks connections that replaced a lingering session for
	// the same agent; see guardReconnect.
	reconnects sync.Map // *mqtt.Client -> struct{}

	mu       sync.Mutex
	presence map[string]a2a.Presence // last card per agent, for offline cards and refresh
}

func newContractHook(cfg *Config, server *mqtt.Server, log *slog.Logger) *contractHook {
	h := &contractHook{
		server:   server,
		log:      log,
		now:      time.Now,
		rate:     newRateLimiter(),
		noise:    newRateLimiter(),
		dedup:    newDedupe(time.Duration(cfg.DedupeWindow)),
		open:     newOpenRequests(),
		presence: map[string]a2a.Presence{},
		counters: newCounters(),
	}
	h.st.Store(newHookState(cfg))
	return h
}

// hookState is an immutable snapshot of the config and its lookups.
type hookState struct {
	cfg    *Config
	agents map[string]*AgentConfig
	hashes map[string][]byte
}

func newHookState(cfg *Config) *hookState {
	s := &hookState{cfg: cfg, agents: map[string]*AgentConfig{}, hashes: map[string][]byte{}}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		s.agents[a.ID] = a
		s.hashes[a.ID], _ = hex.DecodeString(a.TokenSHA256) // validated by Config.Validate
	}
	return s
}

func (h *contractHook) state() *hookState { return h.st.Load() }

// mayRead reports whether agent may receive a message on topic.
func (s *hookState) mayRead(agent *AgentConfig, topic string) bool {
	if agent.Observer {
		return true
	}
	t, err := a2a.ParseTopic(topic)
	if err != nil {
		return false
	}
	switch t.Kind {
	case a2a.KindInbox:
		return t.Owner == agent.ID
	case a2a.KindPresence, a2a.KindEvent:
		return true
	case a2a.KindTask:
		return agent.mayConsume(t.Capability)
	}
	return false
}

func (h *contractHook) ID() string { return "a2a-contract" }

func (h *contractHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnect,
		mqtt.OnConnectAuthenticate,
		mqtt.OnWill,
		mqtt.OnACLCheck,
		mqtt.OnSubscribe,
		mqtt.OnPublish,
		mqtt.OnSelectSubscribers,
		mqtt.OnRetainedExpired,
		mqtt.OnSessionEstablish,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
	}, []byte{b})
}

// OnConnect refuses connections that carry a Will message. mochi publishes
// a Will without calling OnPublish, so a Will would skip every contract
// check: it could claim any sender, reach any inbox, or replace another
// agent's presence card. The contract doesn't need Wills: the broker
// writes each agent's offline card itself.
func (h *contractHook) OnConnect(cl *mqtt.Client, pk packets.Packet) error {
	if cl.Net.Inline || !pk.Connect.WillFlag {
		return nil
	}
	// This runs before authentication, so throttle per remote address like
	// failed logins: otherwise anyone who can reach the port could flood
	// the log and rotate the audit file away.
	if h.noise.allow("ip:"+ipKey(hostOf(cl.Net.Remote)), h.now(), 1, 10) {
		h.log.Warn("connection refused", "client", trunc(cl.ID), "remote", cl.Net.Remote,
			"reason", "will messages are not allowed; the broker publishes offline presence itself")
		h.audit.write(AuditRecord{Action: "refused", Agent: trunc(cl.ID), Remote: cl.Net.Remote, Reason: "will message"})
	}
	code := packets.ErrNotAuthorized
	if cl.Properties.ProtocolVersion < 5 {
		// MQTT 3.1.1 has no 0x87; mochi maps this one to v3 "not authorized".
		code = packets.ErrBadUsernameOrPassword
	}
	_ = h.server.SendConnack(cl, code, false, nil)
	h.counters.inc(&h.counters.decisions.WillsRefused)
	return code
}

// OnWill drops any Will that got past OnConnect. Defense in depth.
func (h *contractHook) OnWill(cl *mqtt.Client, will mqtt.Will) (mqtt.Will, error) {
	if cl.Net.Inline {
		return will, nil
	}
	return mqtt.Will{}, nil
}

// OnConnectAuthenticate requires username = client ID = a configured agent
// ID and password = that agent's token.
func (h *contractHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	if h.closing.Load() {
		return false
	}
	user := string(pk.Connect.Username)
	if reason := h.state().checkLogin(cl.ID, user, pk.Connect.Password); reason != "" {
		h.authFailed(cl, user, reason)
		return false
	}
	return true
}

// checkLogin returns why a login fails against this config, or "".
func (s *hookState) checkLogin(clientID, user string, password []byte) string {
	want, ok := s.hashes[user]
	if !ok || clientID != user {
		return "unknown agent or client id mismatch"
	}
	got, _ := hex.DecodeString(HashToken(string(password)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return "bad token"
	}
	return ""
}

// authFailed logs a failed login, throttled per remote IP so a flood of
// bad logins can't flood the log.
func (h *contractHook) authFailed(cl *mqtt.Client, user, reason string) {
	h.counters.inc(&h.counters.decisions.AuthFailures)
	if h.noise.allow("ip:"+ipKey(hostOf(cl.Net.Remote)), h.now(), 1, 10) {
		h.log.Warn("auth rejected", "client", trunc(cl.ID), "username", trunc(user), "remote", cl.Net.Remote, "reason", reason)
		h.audit.write(AuditRecord{Action: "refused", Agent: trunc(user), Remote: cl.Net.Remote, Reason: reason})
	}
}

// OnACLCheck applies the contract's read permissions. It is called for
// subscription filters and again for each delivery with the concrete topic.
//
// Writes always pass here and are authorized in OnPublish instead: mochi
// disconnects an MQTT 3.1.1 client whose QoS 1 publish fails the ACL, and
// the client would resend the same message on reconnect, forever.
func (h *contractHook) OnACLCheck(cl *mqtt.Client, topic string, write bool) bool {
	if cl.Net.Inline {
		return true
	}
	agent, ok := h.state().agents[cl.ID]
	if !ok {
		return false
	}
	if write {
		return true
	}
	if agent.Observer {
		return !a2a.IsShared(topic) // see everything, consume nothing
	}
	t, err := a2a.ParseTopic(topic)
	if err != nil {
		return false
	}
	switch t.Kind {
	case a2a.KindInbox:
		return t.Owner == agent.ID
	case a2a.KindPresence, a2a.KindEvent:
		return true
	case a2a.KindTask:
		// The exact $share filter is enforced in OnSubscribe; this check
		// also runs per delivery with the plain topic.
		return agent.mayConsume(t.Capability)
	}
	return false
}

// OnSubscribe enforces subscription rules that the ACL hook can't express,
// because the ACL sees a plain task subscription and a task delivery as
// the same string:
//
//   - Task queues may only be consumed through
//     $share/{capability}/a2a/v1/tasks/{capability}. A plain or
//     differently grouped subscription would get a copy of every task.
//   - At most maxSubscriptions per client.
//   - Observers are granted QoS 0. They are read-only, and QoS 0 leaves no
//     in-flight state, which avoids mochi refusing a later SUBSCRIBE with
//     0x91 when its packet ID matches an in-flight delivery (for example
//     a desktop client subscribing to "#" and then "$SYS/#").
//
// Refused filters are swapped for an invalid one so mochi refuses them in
// the SUBACK.
func (h *contractHook) OnSubscribe(cl *mqtt.Client, pk packets.Packet) packets.Packet {
	agent, ok := h.state().agents[cl.ID]
	if !ok {
		return pk
	}
	existing := cl.State.Subscriptions.Len()
	for i := range pk.Filters {
		f := &pk.Filters[i]
		if existing+i >= maxSubscriptions {
			f.Filter = refusedFilter
			continue
		}
		if agent.Observer {
			f.Qos = 0
			continue
		}
		if t, err := a2a.ParseTopic(f.Filter); err == nil && t.Kind == a2a.KindTask && f.Filter != a2a.TaskShareFilter(t.Capability) {
			h.log.Warn("subscription refused", "agent", cl.ID, "filter", trunc(f.Filter),
				"reason", "task queues are consumed only via "+a2a.TaskShareFilter(t.Capability))
			f.Filter = refusedFilter
		}
	}
	return pk
}

// OnSelectSubscribers picks which member of each shared group (task
// queue) gets a message. mochi's default takes an arbitrary member,
// connected or not, so with persistent sessions a task could wait in an
// offline worker's queue while another worker sat idle. Prefer a
// connected member, at random; fall back to an offline one (the task
// queues until it returns) only when the whole group is offline.
func (h *contractHook) OnSelectSubscribers(subs *mqtt.Subscribers, pk packets.Packet) *mqtt.Subscribers {
	if len(subs.Shared) == 0 {
		return subs
	}
	selected := map[string]packets.Subscription{}
	st := h.state()
	for group, members := range subs.Shared {
		// After a reload a stored subscription can outlive its grant
		// (capability revoked, agent removed). Drop such members from the
		// group itself, so mochi's own fallback can't pick them either.
		for id := range members {
			if a, ok := st.agents[id]; !ok || !st.mayRead(a, pk.TopicName) {
				delete(members, id)
			}
		}
		if len(members) == 0 {
			delete(subs.Shared, group)
			continue
		}
		var online, all []string
		for id := range members {
			all = append(all, id)
			if cl, ok := h.server.Clients.Get(id); ok && !cl.Closed() {
				online = append(online, id)
			}
		}
		pool := online
		if len(pool) == 0 {
			pool = all
		}
		if len(pool) == 0 {
			continue
		}
		id := pool[rand.IntN(len(pool))]
		sub := members[id]
		if prev, ok := selected[id]; ok {
			sub = prev.Merge(sub)
		}
		selected[id] = sub
	}
	subs.SharedSelected = selected
	return subs
}

// rejection is why a publish was dropped: a stable code for the error
// envelope and the MQTT 5 PUBACK reason code.
type rejection struct {
	code string
	mqtt packets.Code
}

var (
	rjNotAuthorized = rejection{"not_authorized", packets.ErrNotAuthorized}
	rjRateLimited   = rejection{"rate_limited", packets.ErrQuotaExceeded}
	rjTooLarge      = rejection{"too_large", packets.ErrPacketTooLarge}
	rjInvalid       = rejection{"invalid", packets.ErrPayloadFormatInvalid}
	rjExpired       = rejection{"expired", packets.ErrPayloadFormatInvalid}
	rjNoRoute       = rejection{"no_route", packets.ErrImplementationSpecificError}
	rjNoConsumer    = rejection{"no_consumer", packets.ErrImplementationSpecificError}
	rjQueueFull     = rejection{"queue_full", packets.ErrQuotaExceeded}
	rjInternal      = rejection{"internal", packets.ErrImplementationSpecificError}
)

// OnPublish validates every client publish. Rejected messages are
// acknowledged and dropped (so QoS 1 senders don't retry them forever),
// and the sender gets an error envelope in its inbox.
//
// Accepted envelopes are forwarded re-encoded from the parsed struct, not
// as the sender's bytes. encoding/json matches keys case-insensitively
// and keeps the last duplicate, so {"from":"bob","FROM":"carol"} passes
// as carol here but reads as bob to a case-sensitive parser. Re-encoding
// means every receiver sees exactly what the broker checked.
func (h *contractHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil
	}
	now := h.now()
	st := h.state() // one config snapshot for the whole decision
	agent, ok := st.agents[cl.ID]
	if !ok {
		return pk, h.reject(cl, pk, rjNotAuthorized, "unknown agent")
	}
	// Rate-limit first, before parsing anything, so a flood costs the
	// sender tokens and the broker little CPU.
	rate, burst := st.cfg.RatePerSecond, st.cfg.RateBurst
	if agent.RatePerSecond > 0 {
		rate = agent.RatePerSecond
	}
	if agent.RateBurst > 0 {
		burst = agent.RateBurst
	}
	if !h.rate.allow(cl.ID, now, rate, burst) {
		return pk, h.reject(cl, pk, rjRateLimited, "rate limit exceeded")
	}
	t, err := h.authorizeWrite(agent, pk.TopicName)
	// An inbox outside send_to is still allowed for a reply to a request
	// that inbox's owner sent this agent. That needs the parsed envelope,
	// so the decision waits until after validation.
	var outsideSendTo error
	if errors.Is(err, errOutsideSendTo) {
		outsideSendTo, err = err, nil
	}
	if err != nil {
		return pk, h.reject(cl, pk, rjNotAuthorized, err.Error())
	}
	// MQTT 5 properties (response topic, correlation data, user
	// properties) would travel unchecked beside the envelope. The
	// contract carries all of that in the envelope instead.
	pk.Properties = packets.Properties{}
	if t.Kind == a2a.KindPresence {
		return h.publishPresence(cl, agent, pk, now)
	}
	// Only presence is retained. A retained inbox or task message would
	// be redelivered, and handled again, on every resubscribe.
	pk.FixedHeader.Retain = false

	if len(pk.Payload) > st.cfg.MaxEnvelopeBytes {
		return pk, h.reject(cl, pk, rjTooLarge, "envelope too large")
	}
	env, err := a2a.Parse(pk.Payload)
	if err == nil {
		err = env.ValidateFor(t, cl.ID)
	}
	isReply := err == nil && t.Kind == a2a.KindInbox && (env.Type == a2a.TypeResponse || env.Type == a2a.TypeError)
	if outsideSendTo != nil && !isReply {
		// Only a reply can be allowed outside send_to.
		return pk, h.reject(cl, pk, rjNotAuthorized, outsideSendTo.Error())
	}
	if err != nil {
		return pk, h.reject(cl, pk, rjInvalid, err.Error())
	}
	if err := env.CheckTime(now, time.Duration(st.cfg.MaxClockSkew)); err != nil {
		return pk, h.reject(cl, pk, rjInvalid, err.Error())
	}
	if env.Expired(now) {
		return pk, h.reject(cl, pk, rjExpired, "envelope expired before delivery")
	}
	if rj, reason := h.route(st, t); reason != "" {
		return pk, h.reject(cl, pk, rj, reason)
	}
	if !h.dedup.firstSeen(env.From+"/"+env.ID, now) {
		h.log.Info("duplicate dropped", "action", "duplicate", "from", env.From, "id", env.ID, "topic", truncTopic(pk.TopicName))
		h.audit.write(envelopeRecord("duplicate", truncTopic(pk.TopicName), env, len(pk.Payload)))
		h.counters.inc(&h.counters.decisions.Duplicates)
		return pk, packets.CodeSuccessIgnore
	}
	if isReply {
		// A reply to an open request must come from the request's
		// answerer, even when send_to would allow the sender: otherwise
		// any agent that can reach the requester could answer a task first
		// with a forged reply, and the requester can't tell which worker
		// took the task. Claiming is atomic, so only one reply is accepted.
		r, found, allowed := h.open.claim(t.Owner, env.CorrelationID, agent, env.Type, now)
		switch {
		case found && !allowed:
			return pk, h.reject(cl, pk, rjNotAuthorized,
				fmt.Sprintf("reply to %s must come from %s", env.CorrelationID, r.answerer()))
		case !found && outsideSendTo != nil:
			return pk, h.reject(cl, pk, rjNotAuthorized, outsideSendTo.Error())
		case outsideSendTo != nil:
			h.log.Info("reply allowed outside send_to", "from", env.From, "to", t.Owner, "correlation_id", env.CorrelationID)
		}
	}
	data, err := env.Marshal()
	if err != nil {
		return pk, h.reject(cl, pk, rjInternal, err.Error())
	}
	pk.Payload = data
	if env.Type == a2a.TypeRequest {
		h.trackRequest(env, t, now)
	}
	h.log.Info("envelope", "action", "accept", "from", env.From, "to", env.To, "type", env.Type, "id", env.ID,
		"correlation_id", env.CorrelationID, "priority", env.Priority, "event", env.Event,
		"topic", truncTopic(pk.TopicName), "bytes", len(data))
	h.audit.write(envelopeRecord("accept", truncTopic(pk.TopicName), env, len(data)))
	h.counters.accept(cl.ID, now)
	return pk, nil
}

// errOutsideSendTo marks an inbox publish outside the sender's send_to.
var errOutsideSendTo = errors.New("not allowed to send to")

// trackRequest opens an entry for an accepted request, so its one reply
// can be checked and its outcome counted.
func (h *contractHook) trackRequest(env *a2a.Envelope, t a2a.Topic, now time.Time) {
	r := openRequest{start: now, expires: openExpiry(env, now)}
	if t.Kind == a2a.KindTask {
		r.capability = t.Capability
	} else {
		r.responder = t.Owner
	}
	if !h.open.add(env.From, env.ID, r, now) && h.noise.allow("open-requests-full", now, 0.01, 1) {
		h.log.Warn("open-request table full; replies to new requests fall back to send_to",
			"max", maxOpenRequests)
	}
}

// authorizeWrite applies the contract's write permissions to a publish.
func (h *contractHook) authorizeWrite(agent *AgentConfig, topic string) (a2a.Topic, error) {
	if a2a.HasWildcard(topic) || a2a.IsShared(topic) {
		return a2a.Topic{}, errors.New("cannot publish to a filter")
	}
	t, err := a2a.ParseTopic(topic)
	if err != nil {
		return t, err
	}
	switch t.Kind {
	case a2a.KindInbox:
		if !agent.maySendTo(t.Owner) {
			return t, fmt.Errorf("%w %q", errOutsideSendTo, t.Owner)
		}
	case a2a.KindPresence, a2a.KindEvent:
		if t.Owner != agent.ID {
			return t, fmt.Errorf("%s topic belongs to %q", t.Kind, t.Owner)
		}
	}
	return t, nil
}

// route fails fast when a message has nowhere to go, instead of the
// sender waiting for a timeout: an inbox whose agent isn't configured or
// has no session, or a task queue nobody is serving. A disconnected agent
// that keeps a persistent session still counts; its messages queue.
func (h *contractHook) route(st *hookState, t a2a.Topic) (rejection, string) {
	switch t.Kind {
	case a2a.KindInbox:
		if _, ok := st.agents[t.Owner]; !ok {
			return rjNoRoute, fmt.Sprintf("no agent %q is configured", t.Owner)
		}
		cl, ok := h.server.Clients.Get(t.Owner)
		if !ok {
			return rjNoRoute, fmt.Sprintf("agent %q is offline and has no session to queue for", t.Owner)
		}
		// mochi drops a message silently when the recipient's queue is
		// full. Refuse it here instead, so the sender hears about it.
		if n := cl.State.Inflight.Len(); n >= st.cfg.MaxQueued {
			return rjQueueFull, fmt.Sprintf("agent %q has %d messages waiting; its queue is full", t.Owner, n)
		}
	case a2a.KindTask:
		topic := a2a.TaskTopic(t.Capability)
		serving := false
		for _, members := range h.server.Topics.Subscribers(topic).Shared {
			for id := range members {
				if a, ok := st.agents[id]; ok && st.mayRead(a, topic) {
					serving = true
				}
			}
		}
		if !serving {
			return rjNoConsumer, fmt.Sprintf("no agent is serving capability %q", t.Capability)
		}
	}
	return rejection{}, ""
}

// publishPresence validates a presence card and stamps it with the
// broker's view of status, time, capabilities and key trust. Cards are
// always retained.
func (h *contractHook) publishPresence(cl *mqtt.Client, agent *AgentConfig, pk packets.Packet, now time.Time) (packets.Packet, error) {
	p, err := a2a.ParsePresence(pk.Payload)
	if err != nil {
		return pk, h.reject(cl, pk, rjInvalid, err.Error())
	}
	if p.Agent != cl.ID {
		return pk, h.reject(cl, pk, rjNotAuthorized, "presence agent does not match authenticated agent")
	}
	// Only advertise capabilities the broker actually grants.
	granted := p.Capabilities[:0]
	for _, c := range p.Capabilities {
		if agent.mayConsume(c) {
			granted = append(granted, c)
		}
	}
	p.Capabilities = granted
	// Key trust: pinned keys must match; unpinned keys are only claims.
	p.KeysPinned = false
	if agent.Keys != nil {
		switch {
		case p.Keys == nil:
			p.Keys = agent.Keys
		case *p.Keys != *agent.Keys:
			return pk, h.reject(cl, pk, rjNotAuthorized, "presence keys do not match the keys pinned in broker config")
		}
		p.KeysPinned = true
	}
	p.Status, p.TS = a2a.StatusOnline, now.UTC()
	data, err := json.Marshal(p)
	if err != nil {
		return pk, h.reject(cl, pk, rjInternal, err.Error())
	}
	h.mu.Lock()
	h.presence[cl.ID] = *p
	h.mu.Unlock()
	pk.Payload = data
	pk.FixedHeader.Retain = true
	h.log.Info("presence", "agent", cl.ID, "status", p.Status, "capabilities", p.Capabilities, "keys_pinned", p.KeysPinned)
	h.audit.write(AuditRecord{Action: "presence", Agent: cl.ID, Topic: pk.TopicName, Bytes: len(data)})
	return pk, nil
}

// OnRetainedExpired republishes presence cards. mochi deletes retained
// messages older than its maximum message expiry (24h), which would make
// long-running agents vanish from discovery.
func (h *contractHook) OnRetainedExpired(filter string) {
	t, err := a2a.ParseTopic(filter)
	if err != nil || t.Kind != a2a.KindPresence {
		return
	}
	h.mu.Lock()
	p, ok := h.presence[t.Owner]
	h.mu.Unlock()
	if !ok {
		return
	}
	go func() { // not from inside mochi's expiry sweep
		data, err := json.Marshal(p)
		if err == nil {
			err = h.server.Publish(filter, data, true, 1)
		}
		if err != nil {
			h.log.Error("refresh presence", "agent", t.Owner, "err", err)
		}
	}()
}

// envRef is what the broker could read of a publish's envelope, used to
// address the error it sends back. Fields are empty if unreadable.
type envRef struct {
	ID   string   `json:"id"`
	Type a2a.Type `json:"type"`
}

// peek reads the id and type of a payload without validating it, so even
// a publish rejected before full parsing gets a correlated error.
func peek(payload []byte, limit int) envRef {
	var r envRef
	if len(payload) <= limit && json.Unmarshal(payload, &r) == nil && a2a.ValidID(r.ID) {
		return r
	}
	return envRef{}
}

// reject logs a dropped publish, tells the sender why, and returns the
// error that makes mochi acknowledge and drop the packet. Logs and error
// envelopes are throttled per agent, so a stream of bad publishes can't
// turn into a stream of log lines and broker messages.
func (h *contractHook) reject(cl *mqtt.Client, pk packets.Packet, rj rejection, reason string) error {
	h.counters.reject(cl.ID, rj.code, h.now())
	if h.noise.allow("agent:"+cl.ID, h.now(), 2, 20) {
		// Parsed only for the throttled few that get a log line and an
		// error envelope, not for every rejected publish.
		ref := peek(pk.Payload, h.state().cfg.MaxEnvelopeBytes)
		h.log.Warn("publish rejected", "action", "reject", "agent", cl.ID, "topic", trunc(pk.TopicName),
			"id", trunc(ref.ID), "type", trunc(string(ref.Type)), "code", rj.code, "reason", reason)
		h.audit.write(AuditRecord{Action: "reject", Agent: cl.ID, Topic: trunc(pk.TopicName), ID: trunc(ref.ID),
			Type: a2a.Type(trunc(string(ref.Type))), Code: rj.code, Reason: reason, Bytes: len(pk.Payload)})
		// Don't answer a rejected error with another error.
		if ref.Type != a2a.TypeError {
			correlation := ref.ID
			if correlation == "" {
				correlation = "unknown"
			}
			go h.sendError(cl.ID, correlation, rj.code, reason)
		}
	}
	if cl.Properties.ProtocolVersion == 5 && pk.FixedHeader.Qos > 0 {
		return rj.mqtt // mochi sends this reason code in the PUBACK
	}
	return packets.CodeSuccessIgnore // ack and drop
}

// RejectBody is the body of a broker-sent error envelope.
type RejectBody struct {
	// Code is stable: not_authorized, rate_limited, too_large, invalid,
	// expired, no_route, no_consumer or internal.
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (h *contractHook) sendError(agent, correlation, code, reason string) {
	env, err := a2a.New(a2a.TypeError, a2a.BrokerID, RejectBody{Code: code, Reason: reason})
	if err != nil {
		return
	}
	env.To, env.CorrelationID = agent, correlation
	data, err := env.Marshal()
	if err != nil {
		return
	}
	if err := h.server.Publish(a2a.InboxTopic(agent), data, false, 1); err != nil {
		h.log.Error("send error envelope", "agent", agent, "err", err)
	}
}

// OnSessionEstablish runs after authentication and before mochi takes over
// any existing session with the same client ID. MQTT allows one connection
// per client ID, so a second process using an agent's identity silently
// disconnects the first; with auto-reconnect the two then take turns
// knocking each other off. Warn so that's visible in the log.
func (h *contractHook) OnSessionEstablish(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline {
		return
	}
	old, ok := h.server.Clients.Get(cl.ID)
	if !ok || old == cl {
		return
	}
	h.reconnects.Store(cl, struct{}{})
	if old.Closed() {
		return // an ordinary reconnect, not two live processes
	}
	h.log.Warn("agent session taken over: another connection is using this agent ID",
		"agent", cl.ID, "new_remote", cl.Net.Remote, "old_remote", old.Net.Remote,
		"hint", "give each process its own agent ID; two processes sharing one will keep disconnecting each other")
}

func (h *contractHook) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline {
		return
	}
	// Check the login again against the current config: a reload between
	// authentication and here (agent removed, token rotated) would
	// otherwise leave a session the reload never saw to disconnect.
	st := h.state()
	agent, ok := st.agents[cl.ID]
	if reason := st.checkLogin(cl.ID, string(pk.Connect.Username), pk.Connect.Password); !ok || reason != "" {
		h.log.Warn("connection dropped: the config changed during login", "agent", trunc(cl.ID), "remote", cl.Net.Remote)
		_ = h.server.DisconnectClient(cl, packets.ErrNotAuthorized)
		return
	}
	if tc, ok := cl.Net.Conn.(*trackedConn); ok {
		tc.authenticated() // leaves the listener's pre-auth budget
	}
	if _, ok := h.reconnects.LoadAndDelete(cl); ok {
		go h.guardReconnect(cl)
	}
	h.log.Info("agent connected", "action", "connect", "agent", cl.ID, "remote", cl.Net.Remote, "mqtt", cl.Properties.ProtocolVersion,
		"clean", cl.Properties.Clean, "observer", agent.Observer)
	h.audit.write(AuditRecord{Action: "connect", Agent: cl.ID, Remote: cl.Net.Remote})
	h.counters.seen(cl.ID, h.now())
}

// OnDisconnect publishes the agent's retained offline card, unless the
// connection was replaced by a new one for the same agent.
func (h *contractHook) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	if cl.Net.Inline {
		return
	}
	if cl.IsTakenOver() {
		h.log.Info("agent connection replaced", "agent", cl.ID, "remote", cl.Net.Remote)
		return
	}
	if _, ok := h.state().agents[cl.ID]; !ok {
		return // never authenticated
	}
	if errors.Is(err, packets.ErrServerShuttingDown) {
		// Shutdown, or mochi restoring a stored session at startup. The
		// broker marks restored cards offline itself once the store is
		// loaded (see restorePresence).
		return
	}
	h.log.Info("agent disconnected", "action", "disconnect", "agent", cl.ID, "err", errString(err))
	h.audit.write(AuditRecord{Action: "disconnect", Agent: cl.ID, Remote: cl.Net.Remote, Reason: errString(err)})
	h.counters.seen(cl.ID, h.now())

	// Decide and publish under h.mu. A new connection for the same agent
	// registers before it can publish its online card (which also takes
	// h.mu), so if one is live now we skip, and if not, our offline card
	// is retained before its online card can be.
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.server.Clients.Get(cl.ID); ok && cur != cl && !cur.Closed() {
		return
	}
	p, ok := h.presence[cl.ID]
	if !ok {
		p = a2a.Presence{V: a2a.Version, Agent: cl.ID}
	}
	p.Status, p.TS = a2a.StatusOffline, h.now().UTC()
	h.presence[cl.ID] = p
	data, mErr := json.Marshal(p)
	if mErr != nil {
		return
	}
	if pErr := h.server.Publish(a2a.PresenceTopic(cl.ID), data, true, 1); pErr != nil {
		h.log.Error("publish offline presence", "agent", cl.ID, "err", pErr)
	}
}

func errString(err error) string {
	if err == nil || errors.Is(err, packets.CodeDisconnect) {
		return ""
	}
	return err.Error()
}

// trunc shortens attacker-controlled strings before logging them.
func trunc(s string) string {
	if len(s) > logIDLen {
		return s[:logIDLen] + "…"
	}
	return s
}

func hostOf(remote string) string {
	for i := len(remote) - 1; i >= 0; i-- {
		if remote[i] == ':' {
			return remote[:i]
		}
	}
	return remote
}

// restorePresence runs once after the store is loaded and before any
// client can connect. Restored cards were saved while their agents were
// connected; nobody is connected now, so mark them offline. It also seeds
// the card cache used for offline cards and 24h refreshes.
func (h *contractHook) restorePresence() {
	for _, pk := range h.server.Topics.Messages(a2a.PresenceFilter) {
		p, err := a2a.ParsePresence(pk.Payload)
		if err != nil {
			continue
		}
		agent, ok := h.state().agents[p.Agent]
		if !ok {
			continue // agent was removed from config
		}
		// The config may have changed while the broker was down: re-apply
		// grants and pinned keys before trusting the stored card.
		changed := stampGrants(p, agent)
		if p.Status == a2a.StatusOnline {
			p.Status, p.TS = a2a.StatusOffline, h.now().UTC()
			changed = true
		}
		h.mu.Lock()
		h.presence[p.Agent] = *p
		h.mu.Unlock()
		if !changed {
			continue
		}
		data, err := json.Marshal(p)
		if err == nil {
			err = h.server.Publish(a2a.PresenceTopic(p.Agent), data, true, 1)
		}
		if err != nil {
			h.log.Error("restore presence", "agent", p.Agent, "err", err)
		}
	}
}

// stampGrants makes a cached presence card match agent's current config:
// capabilities it isn't granted are dropped, and keys follow the pins
// (pinned keys replace whatever the card had; without pins, keys are only
// the agent's claim). It reports whether the card changed.
func stampGrants(p *a2a.Presence, agent *AgentConfig) bool {
	changed := false
	granted := make([]string, 0, len(p.Capabilities))
	for _, c := range p.Capabilities {
		if agent.mayConsume(c) {
			granted = append(granted, c)
		}
	}
	if len(granted) != len(p.Capabilities) {
		p.Capabilities, changed = granted, true
	}
	switch {
	case agent.Keys != nil:
		if p.Keys == nil || *p.Keys != *agent.Keys || !p.KeysPinned {
			k := *agent.Keys
			p.Keys, p.KeysPinned, changed = &k, true, true
		}
	case p.KeysPinned:
		p.KeysPinned, changed = false, true
	}
	return changed
}

// applyReload brings presence and sessions in line with a reloaded
// config. Removed agents lose their card and their stored session (queued
// messages and subscriptions); changed agents get their card re-stamped
// with the new grants and keys.
func (h *contractHook) applyReload(st *hookState, removed, changed []string) {
	for _, id := range removed {
		if cl, ok := h.server.Clients.Get(id); ok {
			if !cl.Closed() {
				_ = h.server.DisconnectClient(cl, packets.ErrNotAuthorized)
			}
			h.server.UnsubscribeClient(cl)
			for _, m := range cl.State.Inflight.GetAll(false) {
				cl.State.Inflight.Delete(m.PacketID)
			}
			h.server.Clients.Delete(id)
		}
		h.mu.Lock()
		delete(h.presence, id)
		// An empty retained message deletes the retained card.
		if err := h.server.Publish(a2a.PresenceTopic(id), []byte{}, true, 1); err != nil {
			h.log.Error("clear presence", "agent", id, "err", err)
		}
		h.mu.Unlock()
	}
	for _, id := range changed {
		agent, ok := st.agents[id]
		if !ok {
			continue
		}
		h.mu.Lock()
		p, ok := h.presence[id]
		if ok && stampGrants(&p, agent) {
			h.presence[id] = p
			data, err := json.Marshal(p)
			if err == nil {
				err = h.server.Publish(a2a.PresenceTopic(id), data, true, 1)
			}
			if err != nil {
				h.log.Error("re-stamp presence", "agent", id, "err", err)
			}
		}
		h.mu.Unlock()
	}
}

// truncTopic shortens a topic for logs and audit records.
func truncTopic(s string) string {
	if len(s) > 256 {
		return s[:256] + "…"
	}
	return s
}

// guardReconnect works around a mochi race. When a clean-session client
// disconnects, mochi checks whether it was taken over and, if not,
// unsubscribes and deletes it from the client table by agent ID. A
// reconnect under the same ID that registers between that check and the
// delete loses its table entry and its subscriptions: the broker then
// reports the connected agent as having no session (no_route) and stops
// delivering to its inbox. Writes to data_dir during the unsubscribe make
// the window wide enough to hit in practice.
//
// For a short while after a reconnect, watch for that and put the new
// connection back: re-add it to the table and re-subscribe its filters
// (Subscribe is idempotent).
func (h *contractHook) guardReconnect(cl *mqtt.Client) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !cl.Closed() {
		if cur, ok := h.server.Clients.Get(cl.ID); !ok || cur != cl {
			if ok && cur != cl {
				return // a newer connection replaced this one
			}
			h.server.Clients.Add(cl)
			for _, sub := range cl.State.Subscriptions.GetAll() {
				h.server.Topics.Subscribe(cl.ID, sub)
			}
			if cl.Closed() { // closed while being restored: undo, as mochi would
				h.server.UnsubscribeClient(cl)
				h.server.Clients.Delete(cl.ID)
				return
			}
			h.log.Warn("restored a reconnected agent's session after a late teardown of its previous connection",
				"agent", cl.ID, "remote", cl.Net.Remote)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
