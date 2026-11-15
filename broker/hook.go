package broker

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/jefflaplante/mqtt/a2a"
)

// contractHook enforces the a2a/v1 contract on a mochi-mqtt server.
type contractHook struct {
	mqtt.HookBase

	cfg    *Config
	agents map[string]*AgentConfig
	hashes map[string][]byte
	server *mqtt.Server
	log    *slog.Logger
	now    func() time.Time

	rate  *rateLimiter
	dedup *dedupe

	mu       sync.Mutex
	presence map[string]a2a.Presence // last card per agent, reused for the offline card
}

func newContractHook(cfg *Config, server *mqtt.Server, log *slog.Logger) *contractHook {
	h := &contractHook{
		cfg:      cfg,
		agents:   map[string]*AgentConfig{},
		hashes:   map[string][]byte{},
		server:   server,
		log:      log,
		now:      time.Now,
		rate:     newRateLimiter(cfg.RatePerSecond, cfg.RateBurst),
		dedup:    newDedupe(time.Duration(cfg.DedupeWindow)),
		presence: map[string]a2a.Presence{},
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		h.agents[a.ID] = a
		h.hashes[a.ID], _ = hex.DecodeString(a.TokenSHA256) // validated by Config.Validate
	}
	return h
}

func (h *contractHook) ID() string { return "a2a-contract" }

func (h *contractHook) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnConnectAuthenticate,
		mqtt.OnACLCheck,
		mqtt.OnSubscribe,
		mqtt.OnPublish,
		mqtt.OnSessionEstablish,
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
	}, []byte{b})
}

// OnConnectAuthenticate requires username = client ID = a configured agent
// ID and password = that agent's token.
func (h *contractHook) OnConnectAuthenticate(cl *mqtt.Client, pk packets.Packet) bool {
	user := string(pk.Connect.Username)
	want, ok := h.hashes[user]
	if !ok || cl.ID != user {
		h.log.Warn("auth rejected", "client", cl.ID, "username", user, "remote", cl.Net.Remote, "reason", "unknown agent or client id mismatch")
		return false
	}
	got, _ := hex.DecodeString(HashToken(string(pk.Connect.Password)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		h.log.Warn("auth rejected", "agent", user, "remote", cl.Net.Remote, "reason", "bad token")
		return false
	}
	return true
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
	agent, ok := h.agents[cl.ID]
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
		return agent.mayConsume(t.Capability)
	}
	return false
}

// OnSubscribe grants observers QoS 0 on every subscription. Observers are
// read-only tools, and QoS 0 means the broker keeps no in-flight state for
// them. That matters because mochi-mqtt refuses a SUBSCRIBE (code 0x91)
// whose packet ID matches one of its own in-flight deliveries. Without
// this, a desktop client subscribing to "#" and then "$SYS/#" would have
// the second subscription refused while the retained presence cards from
// the first were still being acknowledged.
func (h *contractHook) OnSubscribe(cl *mqtt.Client, pk packets.Packet) packets.Packet {
	if agent, ok := h.agents[cl.ID]; ok && agent.Observer {
		for i := range pk.Filters {
			pk.Filters[i].Qos = 0
		}
	}
	return pk
}

// OnPublish validates every client publish. Rejected messages are
// acknowledged and dropped (so QoS 1 senders don't retry them forever),
// and the sender gets an error envelope in its inbox.
func (h *contractHook) OnPublish(cl *mqtt.Client, pk packets.Packet) (packets.Packet, error) {
	if cl.Net.Inline {
		return pk, nil
	}
	now := h.now()
	ref := peek(pk.Payload, h.cfg.MaxEnvelopeBytes)
	t, err := h.authorizeWrite(cl.ID, pk.TopicName)
	if err != nil {
		return pk, h.reject(cl, pk, ref, packets.ErrNotAuthorized, err.Error())
	}
	if !h.rate.allow(cl.ID, now) {
		return pk, h.reject(cl, pk, ref, packets.ErrQuotaExceeded, "rate limit exceeded")
	}
	if t.Kind == a2a.KindPresence {
		return h.publishPresence(cl, pk, now)
	}
	if len(pk.Payload) > h.cfg.MaxEnvelopeBytes {
		return pk, h.reject(cl, pk, ref, packets.ErrPacketTooLarge, "envelope too large")
	}
	env, err := a2a.Parse(pk.Payload)
	if err != nil {
		return pk, h.reject(cl, pk, ref, packets.ErrPayloadFormatInvalid, err.Error())
	}
	if err := env.ValidateFor(t, cl.ID); err != nil {
		return pk, h.reject(cl, pk, ref, packets.ErrPayloadFormatInvalid, err.Error())
	}
	if env.Expired(now) {
		return pk, h.reject(cl, pk, ref, packets.ErrPayloadFormatInvalid, "envelope expired before delivery")
	}
	if !h.dedup.firstSeen(env.From+"/"+env.ID, now) {
		h.log.Info("duplicate dropped", "from", env.From, "id", env.ID, "topic", pk.TopicName)
		return pk, packets.CodeSuccessIgnore
	}
	h.log.Info("envelope", "from", env.From, "to", env.To, "type", env.Type, "id", env.ID,
		"correlation_id", env.CorrelationID, "topic", pk.TopicName, "bytes", len(pk.Payload))
	return pk, nil
}

// authorizeWrite applies the contract's write permissions to a publish.
func (h *contractHook) authorizeWrite(agentID, topic string) (a2a.Topic, error) {
	agent, ok := h.agents[agentID]
	if !ok {
		return a2a.Topic{}, errors.New("unknown agent")
	}
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
			return t, fmt.Errorf("not allowed to send to %q", t.Owner)
		}
	case a2a.KindPresence, a2a.KindEvent:
		if t.Owner != agent.ID {
			return t, fmt.Errorf("%s topic belongs to %q", t.Kind, t.Owner)
		}
	}
	return t, nil
}

// publishPresence validates a presence card and stamps it with the
// broker's view of status and time. Cards are always retained.
func (h *contractHook) publishPresence(cl *mqtt.Client, pk packets.Packet, now time.Time) (packets.Packet, error) {
	p, err := a2a.ParsePresence(pk.Payload)
	if err != nil {
		return pk, h.reject(cl, pk, envRef{}, packets.ErrPayloadFormatInvalid, err.Error())
	}
	if p.Agent != cl.ID {
		return pk, h.reject(cl, pk, envRef{}, packets.ErrNotAuthorized, "presence agent does not match authenticated agent")
	}
	p.Status, p.TS = a2a.StatusOnline, now.UTC()
	data, err := json.Marshal(p)
	if err != nil {
		return pk, h.reject(cl, pk, envRef{}, packets.ErrImplementationSpecificError, err.Error())
	}
	h.mu.Lock()
	h.presence[cl.ID] = *p
	h.mu.Unlock()
	pk.Payload = data
	pk.FixedHeader.Retain = true
	h.log.Info("presence", "agent", cl.ID, "status", p.Status, "capabilities", p.Capabilities)
	return pk, nil
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
	if len(payload) <= limit && json.Unmarshal(payload, &r) == nil && len(r.ID) <= a2a.MaxIDLen {
		return r
	}
	return envRef{}
}

// reject logs a dropped publish, tells the sender why, and returns the
// error that makes mochi acknowledge and drop the packet.
func (h *contractHook) reject(cl *mqtt.Client, pk packets.Packet, ref envRef, code packets.Code, reason string) error {
	h.log.Warn("publish rejected", "agent", cl.ID, "topic", pk.TopicName, "id", ref.ID, "type", ref.Type, "reason", reason)

	// Don't answer a rejected error with another error.
	if ref.Type != a2a.TypeError {
		correlation := ref.ID
		if correlation == "" {
			correlation = "unknown"
		}
		go h.sendError(cl.ID, correlation, code, reason)
	}

	if cl.Properties.ProtocolVersion == 5 && pk.FixedHeader.Qos > 0 {
		return code // mochi sends this reason code in the PUBACK
	}
	return packets.CodeSuccessIgnore // ack and drop
}

// RejectBody is the body of a broker-sent error envelope.
type RejectBody struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

func (h *contractHook) sendError(agent, correlation string, code packets.Code, reason string) {
	env, err := a2a.New(a2a.TypeError, a2a.BrokerID, RejectBody{Code: code.Reason, Reason: reason})
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
	if !ok || old == cl || old.Closed() {
		return
	}
	h.log.Warn("agent session taken over: another connection is using this agent ID",
		"agent", cl.ID, "new_remote", cl.Net.Remote, "old_remote", old.Net.Remote,
		"hint", "give each process its own agent ID; two processes sharing one will keep disconnecting each other")
}

func (h *contractHook) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	if cl.Net.Inline {
		return
	}
	h.log.Info("agent connected", "agent", cl.ID, "remote", cl.Net.Remote, "mqtt", cl.Properties.ProtocolVersion,
		"clean", cl.Properties.Clean, "observer", h.agents[cl.ID].Observer)
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
	if _, ok := h.agents[cl.ID]; !ok {
		return // never authenticated
	}
	h.log.Info("agent disconnected", "agent", cl.ID, "err", errString(err))

	h.mu.Lock()
	p, ok := h.presence[cl.ID]
	h.mu.Unlock()
	if !ok {
		p = a2a.Presence{V: a2a.Version, Agent: cl.ID}
	}
	p.Status, p.TS = a2a.StatusOffline, h.now().UTC()
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
