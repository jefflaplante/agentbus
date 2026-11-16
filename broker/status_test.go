package broker_test

import (
	"encoding/json"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/broker"
)

func TestStatusPublishedToObserversOnly(t *testing.T) {
	b := startBus(t, func(c *broker.Config) { c.StatusInterval = broker.Duration(100 * time.Millisecond) })
	b.connect("bob", nil)
	inbox := newRecorder()
	alice := b.connect("alice", inbox.handler)
	if err := alice.Send(ctxT(t), "bob", "hi"); err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(ctxT(t), "nobody", "hi"); err != nil { // rejected: no_route
		t.Fatal(err)
	}
	inbox.next(t)

	if code := suback(t, b.rawConnect("carol", func(paho.Client, paho.Message) {}), broker.StatusTopic); code < 0x80 {
		t.Error("non-observer allowed to read broker status")
	}

	statuses := make(chan broker.Status, 16)
	ops := b.rawConnect("ops", func(_ paho.Client, m paho.Message) {
		var st broker.Status
		if json.Unmarshal(m.Payload(), &st) == nil {
			statuses <- st
		}
	})
	suback(t, ops, broker.StatusTopic)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case st := <-statuses:
			agents := map[string]broker.AgentStatus{}
			for _, a := range st.Agents {
				agents[a.ID] = a
			}
			if st.Decisions.Accepted >= 1 && st.Decisions.Rejected["no_route"] == 1 &&
				agents["alice"].State == "online" && agents["alice"].Accepted >= 1 && agents["alice"].Rejected == 1 &&
				agents["carol"].State == "online" && agents["ops"].Observer && st.Mochi != "" && st.Limits.MaxQueued > 0 {
				return
			}
		case <-deadline:
			t.Fatal("no status with the expected counts arrived")
		}
	}
}

func TestStatusAgentStates(t *testing.T) {
	p := newPersistBus(t)
	p.connect("alice", nil).Close() // persistent session kept -> offline
	bob := p.connect("bob", nil)
	_ = bob.Send(ctxT(t), "alice", "queued for you")
	time.Sleep(100 * time.Millisecond)

	st := p.srv.Status()
	states := map[string]broker.AgentStatus{}
	for _, a := range st.Agents {
		states[a.ID] = a
	}
	if states["alice"].State != "offline" || states["alice"].Queued < 1 {
		t.Errorf("alice = %+v", states["alice"])
	}
	if states["bob"].State != "online" || states["ops"].State != "none" {
		t.Errorf("bob = %+v ops = %+v", states["bob"], states["ops"])
	}
	if st.DataDir == "" || st.AuditFile == "" || st.Traffic.Sessions != 2 {
		t.Errorf("status = %+v", st)
	}
	_ = a2a.Version
}
