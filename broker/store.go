package broker

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/storage"
	"github.com/mochi-mqtt/server/v2/hooks/storage/bolt"
	"github.com/mochi-mqtt/server/v2/packets"
	"go.etcd.io/bbolt"
)

// boltStore persists sessions, subscriptions, queued (in-flight) messages
// and retained messages in a bbolt file. It writes the same records under
// the same keys as mochi's bbolt storage hook (v2.7.9), so it opens files
// that hook wrote, and it differs from that hook in four ways:
//
//  1. Group commit. The mochi hook runs one bbolt transaction, and so one
//     fsync, for every write. bbolt runs one write transaction at a time,
//     so every client waits behind every other client's fsync. Here one
//     goroutine commits everything that queued up during the previous
//     fsync in a single transaction.
//
//     Writes are queued, and the broker waits for them once per packet
//     rather than once per write: after a client's session is stored,
//     after its subscriptions are stored, and after each PUBLISH or
//     SUBSCRIBE has been processed. A message published to a hundred
//     subscribers queues a hundred records and waits for one commit. The
//     broker still doesn't read a client's next packet until what the
//     last one wrote is on disk. A delete does not wait at all: if a
//     crash loses it, the record it would have removed comes back after
//     the restart and the message is delivered again, which the contract
//     already allows.
//
//  2. Acknowledgements aren't stored. Around every PUBACK it sends, mochi
//     stores the PUBACK in the sender's in-flight table and deletes it
//     again. Those records only matter for QoS 2, which the broker turns
//     off, and they cost two transactions per message.
//
//  3. The mochi hook doesn't save the packet ID of in-flight messages, so
//     after a restart they come back with ID 0 and redelivery fails with
//     "protocol violation: missing packet id", dropping the connection on
//     every reconnect. The ID is still in each record's key
//     ("IFM_<client>:<packet id>"), so it's restored from there on load.
//
//  4. The mochi hook's Stop sets its database handle to nil while mochi's
//     $SYS loop and disconnecting clients may still be calling it. Every
//     call here holds a read lock, Stop holds the write lock, and calls
//     after Stop do nothing.
//
// $SYS statistics and the retained broker status aren't persisted;
// they're regenerated after a restart.
type boltStore struct {
	mqtt.HookBase
	db     *bbolt.DB
	bucket []byte

	mu      sync.RWMutex
	stopped bool

	// inflight holds the keys of the in-flight records in the file, so a
	// completion for a record that was never stored costs no transaction.
	inflightMu sync.Mutex
	inflight   map[string]struct{}

	ops  chan storeOp
	done chan struct{} // closed when the committer has exited

	// putMu makes a write's number match its place in the queue. queued is
	// the number of the last write queued and committed the number of the
	// last one written to disk, so flush can tell whether it has to wait.
	putMu     sync.Mutex
	queued    atomic.Uint64
	committed atomic.Uint64
	commitMu  sync.Mutex
	commit    *sync.Cond // signalled, under commitMu, when committed moves
}

// storeOp is one put or delete waiting for the committer.
type storeOp struct {
	key []byte
	val []byte // ignored when del is set
	del bool
	seq uint64 // for a put, its number
}

const (
	storeBucket   = "mochi" // the mochi hook's default, so its files still open
	storeMaxBatch = 1000    // operations per transaction
	storeQueue    = 4096
)

func newBoltStore() *boltStore { return &boltStore{} }

func (s *boltStore) ID() string { return "bolt-db" }

func (s *boltStore) Provides(b byte) bool {
	return bytes.Contains([]byte{
		mqtt.OnSessionEstablished,
		mqtt.OnDisconnect,
		mqtt.OnPacketProcessed,
		mqtt.OnSubscribed,
		mqtt.OnUnsubscribed,
		mqtt.OnRetainMessage,
		mqtt.OnWillSent,
		mqtt.OnQosPublish,
		mqtt.OnQosComplete,
		mqtt.OnQosDropped,
		mqtt.OnClientExpired,
		mqtt.OnRetainedExpired,
		mqtt.StoredClients,
		mqtt.StoredInflightMessages,
		mqtt.StoredRetainedMessages,
		mqtt.StoredSubscriptions,
	}, []byte{b})
}

// Init opens the bbolt file named by a *bolt.Options and starts the
// committer.
func (s *boltStore) Init(config any) error {
	opts, ok := config.(*bolt.Options)
	if !ok || opts == nil {
		return mqtt.ErrInvalidConfigType
	}
	bucket := opts.Bucket
	if bucket == "" {
		bucket = storeBucket
	}
	db, err := bbolt.Open(opts.Path, 0o600, opts.Options)
	if err != nil {
		return err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucket))
		return err
	})
	if err != nil {
		_ = db.Close()
		return err
	}
	s.db, s.bucket = db, []byte(bucket)
	s.inflight = map[string]struct{}{}
	err = db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(s.bucket).Cursor()
		p := []byte(storage.InflightKey)
		for k, _ := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, _ = c.Next() {
			s.inflight[string(k)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return err
	}
	s.commit = sync.NewCond(&s.commitMu)
	s.ops = make(chan storeOp, storeQueue)
	s.done = make(chan struct{})
	go s.commitLoop()
	return nil
}

// commitLoop writes queued operations in the order they were queued. It
// never waits for more: a batch is whatever arrived while the previous
// transaction was committing.
func (s *boltStore) commitLoop() {
	defer close(s.done)
	batch := make([]storeOp, 0, storeMaxBatch)
	for op := range s.ops {
		batch = append(batch[:0], op)
	fill:
		for len(batch) < storeMaxBatch {
			select {
			case next, ok := <-s.ops:
				if !ok {
					break fill
				}
				batch = append(batch, next)
			default:
				break fill
			}
		}
		var last uint64
		err := s.db.Update(func(tx *bbolt.Tx) error {
			b := tx.Bucket(s.bucket)
			for _, op := range batch {
				var err error
				if op.del {
					err = b.Delete(op.key)
				} else {
					err = b.Put(op.key, op.val)
				}
				if err != nil {
					return err
				}
				last = max(last, op.seq)
			}
			return nil
		})
		if err != nil {
			s.Log.Error("failed to write to the store", "error", err, "operations", len(batch))
			for _, op := range batch {
				last = max(last, op.seq) // don't leave waiters stuck behind a failed write
			}
		}
		if last > 0 {
			s.commitMu.Lock()
			s.committed.Store(last)
			s.commit.Broadcast()
			s.commitMu.Unlock()
		}
	}
}

// guard runs fn under the read lock unless the store has stopped.
func (s *boltStore) guard(fn func()) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.stopped && s.db != nil {
		fn()
	}
}

// put queues v to be stored under key. It returns before the commit;
// flush waits for it.
func (s *boltStore) put(key string, v storage.Serializable) {
	data, err := v.MarshalBinary()
	if err != nil {
		s.Log.Error("failed to encode a record for the store", "error", err, "key", key)
		return
	}
	s.putMu.Lock()
	seq := s.queued.Add(1)
	s.ops <- storeOp{key: []byte(key), val: data, seq: seq}
	s.putMu.Unlock()
}

// flush returns once every write queued before the call is on disk. It
// costs nothing when there is none.
func (s *boltStore) flush() { s.waitFor(s.queued.Load()) }

// waitFor returns once write number seq is on disk.
func (s *boltStore) waitFor(seq uint64) {
	if s.committed.Load() >= seq {
		return
	}
	s.commitMu.Lock()
	for s.committed.Load() < seq {
		s.commit.Wait()
	}
	s.commitMu.Unlock()
}

// del queues the removal of key and returns before the commit.
func (s *boltStore) del(key string) { s.ops <- storeOp{key: []byte(key), del: true} }

// Stop commits what is queued and closes the file.
func (s *boltStore) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.db == nil {
		return nil
	}
	s.stopped = true
	close(s.ops)
	<-s.done
	return s.db.Close()
}

func clientKey(cl *mqtt.Client) string { return storage.ClientKey + "_" + cl.ID }

func subscriptionKey(cl *mqtt.Client, filter string) string {
	return storage.SubscriptionKey + "_" + cl.ID + ":" + filter
}

func retainedKey(topic string) string { return storage.RetainedKey + "_" + topic }

func inflightKey(cl *mqtt.Client, pk packets.Packet) string {
	return storage.InflightKey + "_" + cl.ID + ":" + pk.FormatID()
}

func (s *boltStore) OnSessionEstablished(cl *mqtt.Client, _ packets.Packet) {
	s.guard(func() { s.putClient(cl); s.flush() })
}

// OnWillSent runs after a client's will has been published to its
// subscribers, so the flush here also covers those deliveries.
func (s *boltStore) OnWillSent(cl *mqtt.Client, _ packets.Packet) {
	s.guard(func() { s.putClient(cl); s.flush() })
}

// OnPacketProcessed waits for what a PUBLISH or SUBSCRIBE queued: the
// retained message, a record for every recipient, and the retained
// messages a new subscription was sent. The broker's own publishes come
// through here too.
func (s *boltStore) OnPacketProcessed(_ *mqtt.Client, pk packets.Packet, _ error) {
	if t := pk.FixedHeader.Type; t != packets.Publish && t != packets.Subscribe {
		return
	}
	s.guard(s.flush)
}

func (s *boltStore) putClient(cl *mqtt.Client) {
	props := cl.Properties.Props.Copy(false)
	s.put(clientKey(cl), &storage.Client{
		ID:              cl.ID,
		T:               storage.ClientKey,
		Remote:          cl.Net.Remote,
		Listener:        cl.Net.Listener,
		Username:        cl.Properties.Username,
		Clean:           cl.Properties.Clean,
		ProtocolVersion: cl.Properties.ProtocolVersion,
		Properties: storage.ClientProperties{
			SessionExpiryInterval: props.SessionExpiryInterval,
			AuthenticationMethod:  props.AuthenticationMethod,
			AuthenticationData:    props.AuthenticationData,
			RequestProblemInfo:    props.RequestProblemInfo,
			RequestResponseInfo:   props.RequestResponseInfo,
			ReceiveMaximum:        props.ReceiveMaximum,
			TopicAliasMaximum:     props.TopicAliasMaximum,
			User:                  props.User,
			MaximumPacketSize:     props.MaximumPacketSize,
		},
		Will: storage.ClientWill(cl.Properties.Will),
	})
}

// OnDisconnect removes a client whose session ends with the connection.
func (s *boltStore) OnDisconnect(cl *mqtt.Client, _ error, expire bool) {
	if !expire || cl.StopCause() == packets.ErrSessionTakenOver {
		return
	}
	s.guard(func() { s.del(clientKey(cl)) })
}

func (s *boltStore) OnSubscribed(cl *mqtt.Client, pk packets.Packet, codes []byte) {
	s.guard(func() {
		for i, f := range pk.Filters {
			id := subscriptionKey(cl, f.Filter)
			s.put(id, &storage.Subscription{
				ID:                id,
				T:                 storage.SubscriptionKey,
				Client:            cl.ID,
				Qos:               codes[i],
				Filter:            f.Filter,
				Identifier:        f.Identifier,
				NoLocal:           f.NoLocal,
				RetainHandling:    f.RetainHandling,
				RetainAsPublished: f.RetainAsPublished,
			})
		}
		s.flush() // before the SUBACK goes out
	})
}

func (s *boltStore) OnUnsubscribed(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() {
		for _, f := range pk.Filters {
			s.del(subscriptionKey(cl, f.Filter))
		}
	})
}

func (s *boltStore) OnRetainMessage(cl *mqtt.Client, pk packets.Packet, r int64) {
	if strings.HasPrefix(pk.TopicName, "$SYS/") {
		return // broker status is regenerated every few seconds; don't write it to disk
	}
	s.guard(func() {
		id := retainedKey(pk.TopicName)
		if r == -1 {
			s.del(id)
			return
		}
		s.put(id, storedMessage(id, storage.RetainedKey, cl, pk, 0))
	})
}

// OnQosPublish stores a message queued for cl. mochi also calls it with
// the PUBACK it is about to send; that isn't stored.
func (s *boltStore) OnQosPublish(cl *mqtt.Client, pk packets.Packet, sent int64, _ int) {
	if pk.FixedHeader.Type != packets.Publish {
		return
	}
	s.guard(func() {
		id := inflightKey(cl, pk)
		s.inflightMu.Lock()
		s.inflight[id] = struct{}{}
		s.inflightMu.Unlock()
		s.put(id, storedMessage(id, storage.InflightKey, cl, pk, sent))
	})
}

func (s *boltStore) OnQosComplete(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.delInflight(inflightKey(cl, pk)) })
}

func (s *boltStore) OnQosDropped(cl *mqtt.Client, pk packets.Packet) {
	s.guard(func() { s.delInflight(inflightKey(cl, pk)) })
}

// delInflight removes an in-flight record if one was stored under id.
func (s *boltStore) delInflight(id string) {
	s.inflightMu.Lock()
	_, ok := s.inflight[id]
	delete(s.inflight, id)
	s.inflightMu.Unlock()
	if ok {
		s.del(id)
	}
}

func (s *boltStore) OnClientExpired(cl *mqtt.Client) {
	s.guard(func() { s.del(clientKey(cl)) })
}

func (s *boltStore) OnRetainedExpired(filter string) {
	s.guard(func() { s.del(retainedKey(filter)) })
}

// storedMessage is the record for a retained or in-flight packet.
func storedMessage(id, kind string, cl *mqtt.Client, pk packets.Packet, sent int64) *storage.Message {
	props := pk.Properties.Copy(false)
	return &storage.Message{
		ID:          id,
		T:           kind,
		Client:      cl.ID,
		Origin:      pk.Origin,
		FixedHeader: pk.FixedHeader,
		TopicName:   pk.TopicName,
		Payload:     pk.Payload,
		Sent:        sent,
		Created:     pk.Created,
		Properties: storage.MessageProperties{
			PayloadFormat:          props.PayloadFormat,
			MessageExpiryInterval:  props.MessageExpiryInterval,
			ContentType:            props.ContentType,
			ResponseTopic:          props.ResponseTopic,
			CorrelationData:        props.CorrelationData,
			SubscriptionIdentifier: props.SubscriptionIdentifier,
			TopicAlias:             props.TopicAlias,
			User:                   props.User,
		},
	}
}

// each calls visit with every stored value whose key starts with prefix.
func (s *boltStore) each(prefix string, visit func([]byte)) (err error) {
	s.guard(func() {
		err = s.db.View(func(tx *bbolt.Tx) error {
			c := tx.Bucket(s.bucket).Cursor()
			p := []byte(prefix)
			for k, v := c.Seek(p); k != nil && bytes.HasPrefix(k, p); k, v = c.Next() {
				visit(v)
			}
			return nil
		})
	})
	return err
}

func (s *boltStore) StoredClients() (out []storage.Client, err error) {
	err = s.each(storage.ClientKey, func(v []byte) {
		var c storage.Client
		if c.UnmarshalBinary(v) == nil {
			out = append(out, c)
		}
	})
	return out, err
}

func (s *boltStore) StoredSubscriptions() (out []storage.Subscription, err error) {
	err = s.each(storage.SubscriptionKey, func(v []byte) {
		var sub storage.Subscription
		if sub.UnmarshalBinary(v) == nil {
			out = append(out, sub)
		}
	})
	return out, err
}

func (s *boltStore) StoredRetainedMessages() (out []storage.Message, err error) {
	err = s.each(storage.RetainedKey, func(v []byte) {
		var m storage.Message
		if m.UnmarshalBinary(v) == nil {
			out = append(out, m)
		}
	})
	return out, err
}

func (s *boltStore) StoredInflightMessages() (out []storage.Message, err error) {
	err = s.each(storage.InflightKey, func(v []byte) {
		var m storage.Message
		if m.UnmarshalBinary(v) != nil {
			return
		}
		if m.PacketID == 0 {
			id, ok := packetIDFromKey(m.ID)
			if !ok {
				return // unrecoverable; drop rather than break the session
			}
			m.PacketID = id
		}
		out = append(out, m)
	})
	return out, err
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
