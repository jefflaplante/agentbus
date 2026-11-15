package a2a

import (
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
}

// Limits on presence cards.
const (
	MaxCapabilities  = 64
	MaxDescription   = 1024
	MaxPresenceBytes = 16 * 1024
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
	return nil
}
