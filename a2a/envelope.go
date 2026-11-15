package a2a

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Type is the envelope message type.
type Type string

const (
	// TypeRequest expects exactly one TypeResponse or TypeError reply,
	// sent to ReplyTo (or the sender's inbox) with CorrelationID = ID.
	TypeRequest Type = "request"
	// TypeResponse answers a request.
	TypeResponse Type = "response"
	// TypeEvent is a notification; no reply is expected.
	TypeEvent Type = "event"
	// TypeError reports that a request failed or a message was rejected.
	// The broker also sends these to a sender whose publish it drops.
	TypeError Type = "error"
)

func (t Type) valid() bool {
	switch t {
	case TypeRequest, TypeResponse, TypeEvent, TypeError:
		return true
	}
	return false
}

// BrokerID is the From value on envelopes the broker itself sends.
const BrokerID = "$broker"

// Limits on header fields. The body limit is broker configuration.
const (
	MaxIDLen      = 128
	MaxHeaders    = 32
	MaxHeaderLen  = 1024
	MaxContentLen = 128
)

// Envelope is the unit of every message on a2a/v1 inbox, task and event
// topics. Header fields stay in clear text so the broker can route,
// authorize and rate-limit. Body is opaque to the broker and may be
// encrypted (see Enc).
type Envelope struct {
	V             int               `json:"v"`
	ID            string            `json:"id"`
	Type          Type              `json:"type"`
	From          string            `json:"from"`
	To            string            `json:"to,omitempty"`
	TS            time.Time         `json:"ts"`
	TTLMillis     int64             `json:"ttl_ms,omitempty"`
	CorrelationID string            `json:"correlation_id,omitempty"`
	ReplyTo       string            `json:"reply_to,omitempty"`
	Capability    string            `json:"capability,omitempty"`
	ContentType   string            `json:"content_type,omitempty"`
	Enc           string            `json:"enc,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	Body          json.RawMessage   `json:"body,omitempty"`
}

// NewID returns a random 128-bit hex message ID.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// New builds an envelope with V, ID and TS filled in. body is marshaled to
// JSON unless it is already a json.RawMessage or nil.
func New(typ Type, from string, body any) (*Envelope, error) {
	e := &Envelope{V: Version, ID: NewID(), Type: typ, From: from, TS: time.Now().UTC()}
	if err := e.SetBody(body); err != nil {
		return nil, err
	}
	return e, nil
}

// SetBody marshals body into the envelope.
func (e *Envelope) SetBody(body any) error {
	switch b := body.(type) {
	case nil:
		e.Body = nil
	case json.RawMessage:
		e.Body = b
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		e.Body = raw
	}
	return nil
}

// DecodeBody unmarshals the body into v.
func (e *Envelope) DecodeBody(v any) error {
	if len(e.Body) == 0 {
		return errors.New("envelope has no body")
	}
	return json.Unmarshal(e.Body, v)
}

// Reply builds a response (or error, if typ is TypeError) addressed back to
// the sender of e, from agent from.
func (e *Envelope) Reply(typ Type, from string, body any) (*Envelope, error) {
	r, err := New(typ, from, body)
	if err != nil {
		return nil, err
	}
	r.To = e.From
	r.CorrelationID = e.ID
	return r, nil
}

// ReplyTopic is where a reply to e should be published.
func (e *Envelope) ReplyTopic() string {
	if e.ReplyTo != "" {
		return e.ReplyTo
	}
	return InboxTopic(e.From)
}

// Expired reports whether the envelope's TTL has passed at now.
func (e *Envelope) Expired(now time.Time) bool {
	return e.TTLMillis > 0 && now.After(e.TS.Add(time.Duration(e.TTLMillis)*time.Millisecond))
}

// Marshal encodes the envelope as JSON.
func (e *Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }

// Parse decodes and validates an envelope.
func Parse(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// Validation errors. Validate wraps one of these.
var (
	ErrMalformed = errors.New("malformed envelope")
	ErrInvalid   = errors.New("invalid envelope")
)

// Validate checks the envelope on its own, without reference to the topic
// it arrived on or who sent it. See ValidateFor for those checks.
func (e *Envelope) Validate() error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	switch {
	case e.V != Version:
		return bad("unsupported version %d", e.V)
	case e.ID == "" || len(e.ID) > MaxIDLen:
		return bad("id must be 1-%d chars", MaxIDLen)
	case !e.Type.valid():
		return bad("unknown type %q", e.Type)
	case e.From != BrokerID && !ValidName(e.From):
		return bad("invalid from %q", e.From)
	case e.To != "" && !ValidName(e.To):
		return bad("invalid to %q", e.To)
	case e.TS.IsZero():
		return bad("ts is required")
	case e.TTLMillis < 0:
		return bad("ttl_ms must not be negative")
	case (e.Type == TypeResponse || e.Type == TypeError) && e.CorrelationID == "":
		return bad("%s requires correlation_id", e.Type)
	case len(e.CorrelationID) > MaxIDLen:
		return bad("correlation_id too long")
	case e.Capability != "" && !ValidName(e.Capability):
		return bad("invalid capability %q", e.Capability)
	case len(e.ContentType) > MaxContentLen || len(e.Enc) > MaxContentLen:
		return bad("content_type or enc too long")
	case len(e.Headers) > MaxHeaders:
		return bad("more than %d headers", MaxHeaders)
	}
	for k, v := range e.Headers {
		if len(k)+len(v) > MaxHeaderLen {
			return bad("header %q too long", k)
		}
	}
	if e.ReplyTo != "" {
		t, err := ParseTopic(e.ReplyTo)
		if err != nil || HasWildcard(e.ReplyTo) || IsShared(e.ReplyTo) || t.Kind != KindInbox {
			return bad("reply_to must be an inbox topic")
		}
	}
	if len(e.Body) > 0 && !json.Valid(e.Body) {
		return bad("body is not valid JSON")
	}
	return nil
}

// ValidateFor checks that an envelope may be published to topic by the
// authenticated agent sender. The broker enforces this on every publish.
func (e *Envelope) ValidateFor(topic Topic, sender string) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
	}
	if e.From != sender {
		return bad("from %q does not match authenticated agent %q", e.From, sender)
	}
	switch topic.Kind {
	case KindInbox:
		if e.To != topic.Owner {
			return bad("to %q does not match inbox owner %q", e.To, topic.Owner)
		}
	case KindTask:
		if e.Type != TypeRequest {
			return bad("task topics accept only requests")
		}
		if e.Capability != topic.Capability {
			return bad("capability %q does not match task topic %q", e.Capability, topic.Capability)
		}
	case KindEvent:
		if e.Type != TypeEvent {
			return bad("event topics accept only events")
		}
	default:
		return bad("envelopes are not accepted on %s topics", topic.Kind)
	}
	return nil
}
