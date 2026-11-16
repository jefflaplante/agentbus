package client

import (
	"slices"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// copyStore is paho's MemoryStore with each stored PUBLISH copied on the
// way in and on the way out.
//
// It works around a data race in paho v1.5.1
// (eclipse-paho/paho.mqtt.golang#703). With a persistent session, paho
// starts the connection's writer and then resume() walks the store,
// setting Dup on every stored QoS 1/2 PUBLISH. MemoryStore hands back the
// same *PublishPacket that Publish queued for sending, so a publish made
// right after connect (our presence card, a reply) can be on the wire,
// with the writer reading Dup and setting RemainingLength, while resume()
// writes Dup. Copying in Put means the store never holds a packet that is
// queued for writing. Copying in Get means resume() mutates, and the writer
// then serializes, a copy the store has not shared with anyone.
//
// The upstream fix (PR #800) is on paho's master but not in a release.
// Drop this once a tagged paho release includes it.
type copyStore struct {
	paho.Store
}

func newCopyStore() paho.Store { return copyStore{paho.NewMemoryStore()} }

func (s copyStore) Put(key string, m packets.ControlPacket) {
	s.Store.Put(key, copyPublish(m))
}

func (s copyStore) Get(key string) packets.ControlPacket {
	return copyPublish(s.Store.Get(key))
}

// copyPublish returns a copy of a PUBLISH packet and any other packet as
// is. resume() only mutates PUBLISH packets.
func copyPublish(m packets.ControlPacket) packets.ControlPacket {
	p, ok := m.(*packets.PublishPacket)
	if !ok || p == nil {
		return m
	}
	cp := *p
	cp.Payload = slices.Clone(p.Payload)
	return &cp
}
