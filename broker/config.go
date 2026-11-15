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
	"os"
	"slices"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
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
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
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
}

// Validate checks the config. It is called by LoadConfig and New.
func (c *Config) Validate() error {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return errors.New("tls_cert and tls_key must be set together")
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
