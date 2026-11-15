package broker

import (
	"encoding/json"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
)

// StatusTopic is where the broker publishes its Status, retained, every
// status interval. Only observer identities can subscribe to it.
const StatusTopic = "$SYS/agentbus/status"

// Status is a snapshot of a running broker.
type Status struct {
	Version   string        `json:"version"`
	Mochi     string        `json:"mochi_version"`
	Started   time.Time     `json:"started"`
	Time      time.Time     `json:"time"`
	Uptime    int64         `json:"uptime_seconds"`
	Listen    string        `json:"listen"`
	TLS       bool          `json:"tls"`
	DataDir   string        `json:"data_dir,omitempty"`
	AuditFile string        `json:"audit_file,omitempty"`
	Limits    StatusLimits  `json:"limits"`
	Traffic   StatusTraffic `json:"traffic"`
	Decisions Decisions     `json:"decisions"`
	Agents    []AgentStatus `json:"agents"`
}

// StatusLimits are the broker's configured limits.
type StatusLimits struct {
	MaxEnvelopeBytes    int     `json:"max_envelope_bytes"`
	RatePerSecond       float64 `json:"rate_per_second"`
	RateBurst           int     `json:"rate_burst"`
	MaxQueued           int     `json:"max_queued"`
	SessionExpiry       string  `json:"session_expiry"`
	MaxClockSkew        string  `json:"max_clock_skew"`
	MaxConnections      int     `json:"max_connections"`
	MaxConnectionsPerIP int     `json:"max_connections_per_ip"`
}

// StatusTraffic are mochi's MQTT-level counters since start.
type StatusTraffic struct {
	ClientsConnected int64 `json:"clients_connected"`
	Sessions         int64 `json:"sessions"` // connected plus persistent sessions waiting
	Subscriptions    int64 `json:"subscriptions"`
	Retained         int64 `json:"retained"`
	Inflight         int64 `json:"inflight"`
	InflightDropped  int64 `json:"inflight_dropped"`
	MessagesReceived int64 `json:"messages_received"`
	MessagesSent     int64 `json:"messages_sent"`
	BytesReceived    int64 `json:"bytes_received"`
	BytesSent        int64 `json:"bytes_sent"`
	MemoryAlloc      int64 `json:"memory_alloc"`
	Goroutines       int64 `json:"goroutines"`
}

// Decisions counts the contract hook's decisions since start. Unlike the
// logs and audit file, these are never throttled.
type Decisions struct {
	Accepted     int64            `json:"accepted"`
	Duplicates   int64            `json:"duplicates"`
	Rejected     map[string]int64 `json:"rejected"` // by code
	AuthFailures int64            `json:"auth_failures"`
	WillsRefused int64            `json:"wills_refused"`
}

// AgentStatus is one configured agent as the broker sees it.
type AgentStatus struct {
	ID           string    `json:"id"`
	State        string    `json:"state"` // online, offline (session kept), or none
	Remote       string    `json:"remote,omitempty"`
	Observer     bool      `json:"observer,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
	SendTo       []string  `json:"send_to,omitempty"`
	Queued       int       `json:"queued"` // in-flight plus queued messages for this agent
	Accepted     int64     `json:"accepted"`
	Rejected     int64     `json:"rejected"`
	LastSeen     time.Time `json:"last_seen,omitzero"`
}

// counters tracks decisions for Status.
type counters struct {
	mu        sync.Mutex
	decisions Decisions
	perAgent  map[string]*agentCount
}

type agentCount struct {
	accepted, rejected int64
	lastSeen           time.Time
}

func newCounters() *counters {
	return &counters{decisions: Decisions{Rejected: map[string]int64{}}, perAgent: map[string]*agentCount{}}
}

func (c *counters) agent(id string) *agentCount {
	a, ok := c.perAgent[id]
	if !ok {
		a = &agentCount{}
		c.perAgent[id] = a
	}
	return a
}

func (c *counters) accept(agent string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions.Accepted++
	a := c.agent(agent)
	a.accepted++
	a.lastSeen = now
}

func (c *counters) reject(agent, code string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.decisions.Rejected[code]++
	if agent != "" {
		a := c.agent(agent)
		a.rejected++
		a.lastSeen = now
	}
}

func (c *counters) seen(agent string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agent(agent).lastSeen = now
}

func (c *counters) inc(field *int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	*field++
}

// Status returns a snapshot of the broker.
func (b *Broker) Status() Status {
	cfg := b.hook.cfg
	info := b.server.Info.Clone()
	now := time.Now().UTC()
	st := Status{
		Version: buildVersion(), Mochi: mqtt.Version,
		Started: time.Unix(info.Started, 0).UTC(), Time: now, Uptime: now.Unix() - info.Started,
		Listen: b.Addr(), TLS: cfg.TLSCert != "", DataDir: cfg.DataDir, AuditFile: cfg.AuditFile,
		Limits: StatusLimits{
			MaxEnvelopeBytes: cfg.MaxEnvelopeBytes, RatePerSecond: cfg.RatePerSecond, RateBurst: cfg.RateBurst,
			MaxQueued: cfg.MaxQueued, SessionExpiry: time.Duration(cfg.SessionExpiry).String(),
			MaxClockSkew: time.Duration(cfg.MaxClockSkew).String(), MaxConnections: cfg.MaxConnections,
			MaxConnectionsPerIP: cfg.MaxConnectionsPerIP,
		},
		Traffic: StatusTraffic{
			ClientsConnected: info.ClientsConnected, Sessions: b.sessions(),
			Subscriptions: info.Subscriptions, Retained: info.Retained, Inflight: info.Inflight,
			InflightDropped: info.InflightDropped, MessagesReceived: info.MessagesReceived,
			MessagesSent: info.MessagesSent, BytesReceived: info.BytesReceived, BytesSent: info.BytesSent,
			MemoryAlloc: info.MemoryAlloc, Goroutines: info.Threads,
		},
	}

	c := b.hook.counters
	c.mu.Lock()
	st.Decisions = c.decisions
	st.Decisions.Rejected = make(map[string]int64, len(c.decisions.Rejected))
	for k, v := range c.decisions.Rejected {
		st.Decisions.Rejected[k] = v
	}
	for _, a := range cfg.Agents {
		as := AgentStatus{ID: a.ID, State: "none", Observer: a.Observer,
			Capabilities: slices.Clone(a.Capabilities), SendTo: slices.Clone(a.SendTo)}
		if n, ok := c.perAgent[a.ID]; ok {
			as.Accepted, as.Rejected, as.LastSeen = n.accepted, n.rejected, n.lastSeen
		}
		if cl, ok := b.server.Clients.Get(a.ID); ok {
			as.Queued = cl.State.Inflight.Len()
			if cl.Closed() {
				as.State = "offline"
			} else {
				as.State, as.Remote = "online", cl.Net.Remote
			}
		}
		st.Agents = append(st.Agents, as)
	}
	c.mu.Unlock()
	return st
}

// sessions counts agent sessions, connected or waiting, excluding the
// broker's own inline client.
func (b *Broker) sessions() int64 {
	var n int64
	for _, cl := range b.server.Clients.GetAll() {
		if !cl.Net.Inline {
			n++
		}
	}
	return n
}

// publishStatus publishes Status to StatusTopic every interval until done.
func (b *Broker) publishStatus(interval time.Duration, done <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		data, err := json.Marshal(b.Status())
		if err == nil {
			err = b.server.Publish(StatusTopic, data, true, 0)
		}
		if err != nil {
			b.log.Warn("publish broker status", "err", err)
		}
		select {
		case <-t.C:
		case <-done:
			return
		}
	}
}

// buildVersion is the agentbus module version and VCS revision, if known.
func buildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := bi.Main.Version
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			v += " (" + s.Value[:7] + ")"
		}
	}
	return v
}
