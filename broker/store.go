package broker

import (
	"strconv"
	"strings"
	"sync"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/storage"
	"github.com/mochi-mqtt/server/v2/hooks/storage/bolt"
	"github.com/mochi-mqtt/server/v2/packets"
)

// boltStore wraps mochi's bbolt storage hook (v2.7.9) to fix two bugs:
//
//  1. The hook doesn't save the packet ID of in-flight (queued) messages,
//     so after a restart they come back with ID 0 and redelivery fails
//     with "protocol violation: missing packet id", dropping the
//     connection on every reconnect. The ID is still in each record's key
//     ("IFM_<client>:<packet id>"), so it's restored from there on load.
//  2. Stop sets the hook's database handle to nil while mochi's $SYS loop
//     and disconnecting clients may still be calling the hook. Every call
//     here holds a read lock, Stop holds the write lock, and calls after
//     Stop do nothing.
//
// $SYS statistics and the retained broker status aren't persisted;
// they're regenerated after a restart.
type boltStore struct {
	*bolt.Hook
	mu      sync.RWMutex
	stopped bool
}

func newBoltStore() *boltStore { return &boltStore{Hook: new(bolt.Hook)} }

func (s *boltStore) Provides(b byte) bool {
	if b == mqtt.OnSysInfoTick || b == mqtt.StoredSysInfo {
		return false
	}
	return s.Hook.Provides(b)
}

// guard runs fn under the read lock unless the store has stopped.
func (s *boltStore) guard(fn func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.stopped {
		fn()
	}
}

func (s *boltStore) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil
	}
	s.stopped = true
	return s.Hook.Stop()
}

func (s *boltStore) OnSessionEstablished(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.Hook.OnSessionEstablished(cl, pk) })
}

func (s *boltStore) OnDisconnect(cl *mqtt.Client, err error, expire bool) {
	s.guard(func() { s.Hook.OnDisconnect(cl, err, expire) })
}

func (s *boltStore) OnSubscribed(cl *mqtt.Client, pk packets.Packet, codes []byte) {
	s.guard(func() { s.Hook.OnSubscribed(cl, pk, codes) })
}

func (s *boltStore) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.Hook.OnUnsubscribed(cl, pk) })
}

func (s *boltStore) OnRetainMessage(cl *mqtt.Client, pk packets.Packet, r int64) {
	if strings.HasPrefix(pk.TopicName, "$SYS/") {
		return // broker status is regenerated every few seconds; don't write it to disk
	}
	s.guard(func() { s.Hook.OnRetainMessage(cl, pk, r) })
}

func (s *boltStore) OnWillSent(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.Hook.OnWillSent(cl, pk) })
}

func (s *boltStore) OnQosPublish(cl *mqtt.Client, pk packets.Packet, sent int64, resends int) {
	s.guard(func() { s.Hook.OnQosPublish(cl, pk, sent, resends) })
}

func (s *boltStore) OnQosComplete(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.Hook.OnQosComplete(cl, pk) })
}

func (s *boltStore) OnQosDropped(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.Hook.OnQosDropped(cl, pk) })
}

func (s *boltStore) OnClientExpired(cl *mqtt.Client) {
	s.guard(func() { s.Hook.OnClientExpired(cl) })
}

func (s *boltStore) OnRetainedExpired(filter string) {
	s.guard(func() { s.Hook.OnRetainedExpired(filter) })
}

func (s *boltStore) StoredClients() (v []storage.Client, err error) {
	s.guard(func() { v, err = s.Hook.StoredClients() })
	return
}

func (s *boltStore) StoredSubscriptions() (v []storage.Subscription, err error) {
	s.guard(func() { v, err = s.Hook.StoredSubscriptions() })
	return
}

func (s *boltStore) StoredRetainedMessages() (v []storage.Message, err error) {
	s.guard(func() { v, err = s.Hook.StoredRetainedMessages() })
	return
}

func (s *boltStore) StoredInflightMessages() ([]storage.Message, error) {
	var msgs []storage.Message
	var err error
	s.guard(func() { msgs, err = s.Hook.StoredInflightMessages() })
	if err != nil {
		return nil, err
	}
	out := msgs[:0]
	for _, m := range msgs {
		if m.PacketID == 0 {
			id, ok := packetIDFromKey(m.ID)
			if !ok {
				continue // unrecoverable; drop rather than break the session
			}
			m.PacketID = id
		}
		out = append(out, m)
	}
	return out, nil
}

// packetIDFromKey parses the packet ID from "IFM_<client>:<id>". Agent
// IDs can't contain ':', so the last colon separates the two.
func packetIDFromKey(key string) (uint16, bool) {
	i := strings.LastIndexByte(key, ':')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseUint(key[i+1:], 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint16(n), true
}
