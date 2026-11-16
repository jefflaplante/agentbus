package client

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
)

// fakeMsg is a paho.Message for feeding dispatch directly.
type fakeMsg struct {
	topic   string
	payload []byte
}

func (m fakeMsg) Duplicate() bool   { return false }
func (m fakeMsg) Qos() byte         { return 1 }
func (m fakeMsg) Retained() bool    { return false }
func (m fakeMsg) Topic() string     { return m.topic }
func (m fakeMsg) MessageID() uint16 { return 0 }
func (m fakeMsg) Payload() []byte   { return m.payload }
func (m fakeMsg) Ack()              {}

// Messages are handled out of order (SetOrderMatters(false)), so an older
// presence card can arrive after a newer one. It must not win.
func TestOlderPresenceCardIgnored(t *testing.T) {
	c := &Client{log: slog.New(slog.NewTextHandler(io.Discard, nil)), agents: map[string]a2a.Presence{}}
	card := func(status a2a.Status, ts time.Time) fakeMsg {
		data, _ := json.Marshal(a2a.Presence{V: a2a.Version, Agent: "bob", Status: status, TS: ts})
		return fakeMsg{topic: a2a.PresenceTopic("bob"), payload: data}
	}
	now := time.Now().UTC()
	c.dispatch(card(a2a.StatusOffline, now))
	c.dispatch(card(a2a.StatusOnline, now.Add(-time.Second)))
	if got := c.agents["bob"].Status; got != a2a.StatusOffline {
		t.Errorf("status = %s, want the newer offline card", got)
	}
	c.dispatch(card(a2a.StatusOnline, now.Add(time.Second)))
	if got := c.agents["bob"].Status; got != a2a.StatusOnline {
		t.Errorf("status = %s, want the newest online card", got)
	}
}

// The broker clears a removed agent's card with an empty retained message.
func TestEmptyPresenceCardRemovesAgent(t *testing.T) {
	c := &Client{log: slog.New(slog.NewTextHandler(io.Discard, nil)), agents: map[string]a2a.Presence{}}
	data, _ := json.Marshal(a2a.Presence{V: a2a.Version, Agent: "bob", Status: a2a.StatusOnline, TS: time.Now()})
	c.dispatch(fakeMsg{topic: a2a.PresenceTopic("bob"), payload: data})
	c.dispatch(fakeMsg{topic: a2a.PresenceTopic("bob")})
	if _, ok := c.agents["bob"]; ok {
		t.Error("removed agent still listed")
	}
}
