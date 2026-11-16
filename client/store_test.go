package client

import (
	"io"
	"sync"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

func storedPublish(s interface {
	Open()
	Put(string, packets.ControlPacket)
}) *packets.PublishPacket {
	p := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	p.Qos = 1
	p.MessageID = 7
	p.TopicName = "a2a/v1/presence/x"
	p.Payload = []byte("card")
	s.Open()
	s.Put("o.7", p)
	return p
}

// TestCopyStoreIsolatesPublish recreates paho v1.5.1's race between
// resume() and the connection writer (eclipse-paho/paho.mqtt.golang#703):
// the writer serializes the packet Publish queued while resume() sets Dup on
// what the store returns. Under -race this fails with a plain MemoryStore.
func TestCopyStoreIsolatesPublish(t *testing.T) {
	s := newCopyStore()
	queued := storedPublish(s)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // startOutgoingComms
		defer wg.Done()
		_ = queued.Write(io.Discard)
	}()
	go func() { // resume
		defer wg.Done()
		got := s.Get("o.7").(*packets.PublishPacket)
		got.Dup = true
		_ = got.Write(io.Discard)
	}()
	wg.Wait()

	if queued.Dup {
		t.Error("resume's Dup leaked into the queued packet")
	}
	again := s.Get("o.7").(*packets.PublishPacket)
	if again.Dup {
		t.Error("resume's Dup leaked into the stored packet")
	}
	if again.MessageID != 7 || again.Qos != 1 || string(again.Payload) != "card" {
		t.Errorf("stored packet changed: %v", again)
	}
	queued.Payload[0] = 'X'
	if string(s.Get("o.7").(*packets.PublishPacket).Payload) != "card" {
		t.Error("store shares the payload with the queued packet")
	}
}

func TestCopyStorePassesOtherPackets(t *testing.T) {
	s := newCopyStore()
	s.Open()
	ack := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
	ack.MessageID = 3
	s.Put("o.3", ack)
	if got := s.Get("o.3"); got != ack {
		t.Errorf("Get = %v, want the stored PUBACK", got)
	}
	if got := s.Get("o.99"); got != nil {
		t.Errorf("Get missing = %v, want nil", got)
	}
}
