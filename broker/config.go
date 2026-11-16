// Package broker runs an MQTT broker that enforces the a2a/v1 contract.
// It embeds mochi-mqtt and adds agent authentication, contract ACLs,
// envelope validation, per-agent rate limits, dedupe and presence.
package broker

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
)

// Config is the broker configuration, loaded from JSON.
type Config struct {
	// Listen is the TCP address for MQTT, e.g. ":1883".
	Listen string `json:"listen"`
	// TLSCert and TLSKey enable TLS on the listener when both are set.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`

	// MaxEnvelopeBytes caps the size of one envelope (the whole MQTT payload).
	MaxEnvelopeBytes int `json:"max_envelope_bytes"`
	// RatePerSecond and RateBurst bound how fast one agent may publish.
	RatePerSecond float64 `json:"rate_per_second"`
	RateBurst     int     `json:"rate_burst"`
	// DedupeWindow is how long envelope IDs are remembered per sender.
	DedupeWindow Duration `json:"dedupe_window"`
	// MaxClockSkew is how far an envelope's ts may be from broker time.
	MaxClockSkew Duration `json:"max_clock_skew"`

	// AllowInsecure permits a plaintext (non-TLS) listener on a
	// non-loopback address. Tokens then cross the network in clear.
	AllowInsecure bool `json:"allow_insecure,omitempty"`
	// MetricsListen, if set, serves /metrics (Prometheus text), /healthz
	// and /readyz over plain HTTP on this address. Like listen, it must be
	// loopback unless allow_insecure is set: the metrics name every agent.
	MetricsListen string `json:"metrics_listen,omitempty"`
	// MCPListen, if set, makes `agentbus serve` also serve MCP over HTTP
	// (Streamable HTTP) on this address, at /mcp: remote MCP clients use
	// the bus as an agent by presenting that agent's token. It uses
	// tls_cert and tls_key when they're set, and like listen must be
	// loopback without them unless allow_insecure is set.
	MCPListen string `json:"mcp_listen,omitempty"`
	// MCPURL is the URL clients use to reach the MCP endpoint, when a proxy
	// or DNS name fronts it; `agentbus config mcp` prints it. Default: built
	// from mcp_listen.
	MCPURL string `json:"mcp_url,omitempty"`
	// ConnectTimeout is how long a new connection may take to send CONNECT.
	ConnectTimeout Duration `json:"connect_timeout"`
	// MaxConnections and MaxConnectionsPerIP cap open sockets, including
	// ones that have not authenticated yet.
	MaxConnections      int `json:"max_connections"`
	MaxConnectionsPerIP int `json:"max_connections_per_ip"`
	// SessionExpiry is how long the broker keeps a disconnected agent's
	// persistent session and its queued messages.
	SessionExpiry Duration `json:"session_expiry"`
	// MaxQueued caps in-flight plus queued messages per agent session.
	MaxQueued int `json:"max_queued"`

	// DataDir, if set, makes broker state survive restarts: persistent
	// sessions, subscriptions, queued messages and presence cards (in a
	// bbolt file), and the dedupe window. Created with mode 0700.
	DataDir string `json:"data_dir,omitempty"`

	// AuditFile, if set, receives one JSON line per broker decision
	// (accept, reject, duplicate, connect, disconnect). Bodies are never
	// written, only their SHA-256. Rotated at AuditMaxBytes, keeping
	// AuditKeep old files.
	AuditFile string `json:"audit_file,omitempty"`
	// StatusInterval is how often the broker publishes its Status to
	// $SYS/agentbus/status for observers (agentbus status, dashboard).
	StatusInterval Duration `json:"status_interval"`
	AuditMaxBytes  int64    `json:"audit_max_bytes"`
	AuditKeep      int      `json:"audit_keep"`

	Agents []AgentConfig `json:"agents"`
}

// AgentConfig is one agent identity allowed on the bus.
type AgentConfig struct {
	ID string `json:"id"`
	// TokenSHA256 is the hex SHA-256 of the agent's token. Agents connect
	// with username = ID, client ID = ID, password = token.
	TokenSHA256 string `json:"token_sha256"`
	// Capabilities lists the task topics this agent may consume. "*" allows all.
	Capabilities []string `json:"capabilities,omitempty"`
	// SendTo lists the agents whose inbox this agent may publish to.
	// Empty or "*" allows all.
	SendTo []string `json:"send_to,omitempty"`
	// RatePerSecond and RateBurst override the broker-wide publish rate
	// for this agent, e.g. a higher limit for a busy gateway.
	RatePerSecond float64 `json:"rate_per_second,omitempty"`
	RateBurst     int     `json:"rate_burst,omitempty"`
	// Keys pins this agent's public keys. When set, the broker rejects
	// presence cards whose keys differ and marks matching cards
	// keys_pinned, so other agents can trust them for encryption.
	Keys *a2a.Keys `json:"keys,omitempty"`
	// Observer lets this agent subscribe to any topic, including other
	// agents' inboxes, "#" and $SYS, for debugging tools such as MQTT
	// Explorer. It is read-only: shared subscriptions are still refused,
	// so an observer can't take tasks, and its publishes follow the
	// normal rules. Grant it only to identities used by operators.
	Observer bool `json:"observer,omitempty"`
}

// Duration is a time.Duration that reads from JSON as "10m", "30s", etc.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// Defaults.
const (
	DefaultListen           = ":1883"
	DefaultMaxEnvelopeBytes = 256 * 1024
	DefaultRatePerSecond    = 20
	DefaultRateBurst        = 50
	DefaultDedupeWindow     = 10 * time.Minute
	DefaultMaxClockSkew     = 2 * time.Minute
	DefaultConnectTimeout   = 10 * time.Second
	DefaultMaxConnections   = 1024
	DefaultMaxConnsPerIP    = 64
	DefaultSessionExpiry    = 7 * 24 * time.Hour
	DefaultMaxQueued        = 1024
	DefaultAuditMaxBytes    = 64 << 20
	DefaultAuditKeep        = 3
	DefaultStatusInterval   = 2 * time.Second
)

// LoadConfig reads, defaults and validates a JSON config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// ApplyDefaults fills unset fields with their defaults.
func (c *Config) ApplyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.MaxEnvelopeBytes == 0 {
		c.MaxEnvelopeBytes = DefaultMaxEnvelopeBytes
	}
	if c.RatePerSecond == 0 {
		c.RatePerSecond = DefaultRatePerSecond
	}
	if c.RateBurst == 0 {
		c.RateBurst = DefaultRateBurst
	}
	if c.DedupeWindow == 0 {
		c.DedupeWindow = Duration(DefaultDedupeWindow)
	}
	if c.MaxClockSkew == 0 {
		c.MaxClockSkew = Duration(DefaultMaxClockSkew)
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = Duration(DefaultConnectTimeout)
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	if c.MaxConnectionsPerIP == 0 {
		c.MaxConnectionsPerIP = DefaultMaxConnsPerIP
	}
	if c.SessionExpiry == 0 {
		c.SessionExpiry = Duration(DefaultSessionExpiry)
	}
	if c.MaxQueued == 0 {
		c.MaxQueued = DefaultMaxQueued
	}
	if c.AuditMaxBytes == 0 {
		c.AuditMaxBytes = DefaultAuditMaxBytes
	}
	if c.AuditKeep == 0 {
		c.AuditKeep = DefaultAuditKeep
	}
	if c.StatusInterval == 0 {
		c.StatusInterval = Duration(DefaultStatusInterval)
	}
}

// isLoopback reports whether a listen address binds only to loopback.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false // ":1883" binds every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate checks the config. It is called by LoadConfig and New.
func (c *Config) Validate() error {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
	}
	if c.TLSCert == "" && !c.AllowInsecure && !isLoopback(c.Listen) {
		return fmt.Errorf("listen %q is not loopback and TLS is off: tokens would cross the network in clear; "+
			"set tls_cert/tls_key, bind to 127.0.0.1, or set allow_insecure", c.Listen)
	}
	if c.MetricsListen != "" && !c.AllowInsecure && !isLoopback(c.MetricsListen) {
		return fmt.Errorf("metrics_listen %q is not loopback: the metrics endpoint has no authentication; "+
			"bind it to 127.0.0.1 (and scrape locally or through a proxy), or set allow_insecure", c.MetricsListen)
	}
	if c.MCPListen != "" && c.TLSCert == "" && !c.AllowInsecure && !isLoopback(c.MCPListen) {
		return fmt.Errorf("mcp_listen %q is not loopback and TLS is off: tokens would cross the network in clear; "+
			"set tls_cert/tls_key, bind to 127.0.0.1, or set allow_insecure", c.MCPListen)
	}
	if c.MCPURL != "" {
		if u, err := url.Parse(c.MCPURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("mcp_url %q: want an http:// or https:// URL", c.MCPURL)
		}
	}
	if c.MaxConnections < 1 || c.MaxConnectionsPerIP < 1 || c.MaxQueued < 1 {
		return errors.New("max_connections, max_connections_per_ip and max_queued must be positive")
	}
	if time.Duration(c.StatusInterval) < 100*time.Millisecond {
		return errors.New("status_interval must be at least 100ms")
	}
	if c.AuditMaxBytes < 1024 || c.AuditKeep < 1 {
		return errors.New("audit_max_bytes must be at least 1024 and audit_keep at least 1")
	}
	if c.MaxEnvelopeBytes < 1024 {
		return errors.New("max_envelope_bytes must be at least 1024")
	}
	if c.RatePerSecond < 0 || c.RateBurst < 1 {
		return errors.New("rate_per_second must be >= 0 and rate_burst >= 1")
	}
	if len(c.Agents) == 0 {
		return errors.New("no agents configured")
	}
	seen := map[string]bool{}
	for _, a := range c.Agents {
		if !a2a.ValidName(a.ID) {
			return fmt.Errorf("agent id %q is invalid", a.ID)
		}
		if seen[a.ID] {
			return fmt.Errorf("agent %q listed twice", a.ID)
		}
		seen[a.ID] = true
		if b, err := hex.DecodeString(a.TokenSHA256); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("agent %q: token_sha256 must be 64 hex chars", a.ID)
		}
		for _, cap := range a.Capabilities {
			if cap != "*" && !a2a.ValidName(cap) {
				return fmt.Errorf("agent %q: invalid capability %q", a.ID, cap)
			}
		}
		if a.RatePerSecond < 0 || a.RateBurst < 0 {
			return fmt.Errorf("agent %q: rate overrides must not be negative", a.ID)
		}
		if a.Keys != nil {
			if err := a.Keys.Validate(); err != nil {
				return fmt.Errorf("agent %q: %w", a.ID, err)
			}
		}
		for _, to := range a.SendTo {
			if to != "*" && !a2a.ValidName(to) {
				return fmt.Errorf("agent %q: invalid send_to %q", a.ID, to)
			}
		}
	}
	return nil
}

func (a *AgentConfig) mayConsume(capability string) bool {
	if slices.Contains(a.Capabilities, "*") {
		return true
	}
	return capability != "+" && slices.Contains(a.Capabilities, capability)
}

func (a *AgentConfig) maySendTo(agent string) bool {
	if len(a.SendTo) == 0 || slices.Contains(a.SendTo, "*") {
		return true
	}
	return slices.Contains(a.SendTo, agent)
}

// NewToken returns a random token and its hex SHA-256 for the config file.
func NewToken() (token, hash string) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	token = hex.EncodeToString(b[:])
	return token, HashToken(token)
}

// HashToken returns the hex SHA-256 of token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
