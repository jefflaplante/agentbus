package a2a

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Status is an agent's presence status.
type Status string

const (
	StatusOnline  Status = "online"
	StatusOffline Status = "offline"
)

// Presence is the retained card on an agent's presence topic. The agent
// publishes it after connecting; the broker overwrites Status and TS on
// every publish and writes an offline card when the agent disconnects.
type Presence struct {
	V            int               `json:"v"`
	Agent        string            `json:"agent"`
	Status       Status            `json:"status"`
	TS           time.Time         `json:"ts"`
	Description  string            `json:"description,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Meta         map[string]string `json:"meta,omitempty"`
	// Keys are the agent's public keys, for end-to-end signing and
	// encryption between agents (conduit-1gkv phase 3).
	Keys *Keys `json:"keys,omitempty"`
	// KeysPinned is set by the broker: true when Keys match the keys in
	// the broker's config for this agent, false when they are only what
	// the agent claims. Receivers should not encrypt to unpinned keys
	// without some other way to trust them.
	KeysPinned bool `json:"keys_pinned,omitempty"`
}

// Keys are base64 (standard, padded) 32-byte public keys.
type Keys struct {
	Sign string `json:"sign,omitempty"` // Ed25519
	KEX  string `json:"kex,omitempty"`  // X25519
}

// Validate checks that each present key decodes to 32 bytes.
func (k *Keys) Validate() error {
	for name, v := range map[string]string{"sign": k.Sign, "kex": k.KEX} {
		if v == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("%w: keys.%s must be base64 of 32 bytes", ErrInvalid, name)
		}
	}
	return nil
}

// Limits on presence cards.
const (
	MaxCapabilities  = 64
	MaxDescription   = 1024
	MaxPresenceBytes = 16 * 1024
	MaxMeta          = 32
	MaxMetaLen       = 256
)

// ParsePresence decodes and validates a presence card.
func ParsePresence(data []byte) (*Presence, error) {
	if len(data) > MaxPresenceBytes {
		return nil, fmt.Errorf("%w: presence card over %d bytes", ErrInvalid, MaxPresenceBytes)
	}
	var p Presence
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks a presence card.
func (p *Presence) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	switch {
	case p.V != Version:
		return bad("unsupported version %d", p.V)
	case !ValidName(p.Agent):
		return bad("invalid agent %q", p.Agent)
	case len(p.Description) > MaxDescription:
		return bad("description too long")
	case len(p.Capabilities) > MaxCapabilities:
		return bad("more than %d capabilities", MaxCapabilities)
	}
	for _, c := range p.Capabilities {
		if !ValidName(c) {
			return bad("invalid capability %q", c)
		}
	}
	if len(p.Meta) > MaxMeta {
		return bad("more than %d meta entries", MaxMeta)
	}
	for k, v := range p.Meta {
		if len(k)+len(v) > MaxMetaLen {
			return bad("meta %q too long", k)
		}
	}
	if p.Keys != nil {
		return p.Keys.Validate()
	}
	return nil
}
