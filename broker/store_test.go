package broker

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/storage/bolt"
	"github.com/mochi-mqtt/server/v2/packets"
)

// TestStoreWaitsForCommit checks that the store holds a client's packet
// until what it wrote is on disk, and no longer.
func TestStoreWaitsForCommit(t *testing.T) {
	s := newBoltStore()
	s.SetOpts(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	path := filepath.Join(t.TempDir(), "store.db")
	if err := s.Init(&bolt.Options{Path: path}); err != nil {
		t.Fatal(err)
	}
	cl := mqtt.New(nil).NewClient(nil, "test", "alice", false)

	// bbolt runs one write transaction at a time, so holding one open
	// stops the committer.
	tx, err := s.db.Begin(true)
	if err != nil {
		t.Fatal(err)
	}
	returned := make(chan struct{})
	go func() {
		pk := packets.Packet{
			FixedHeader: packets.FixedHeader{Type: packets.Publish, Qos: 1},
			TopicName:   "a2a/v1/agents/alice/inbox", PacketID: 1,
		}
		s.OnQosPublish(cl, pk, 0, 0)
		s.OnPacketProcessed(cl, pk, nil)
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("the packet didn't wait for the disk")
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the packet never returned")
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	// What the packet wrote is in the file.
	s = newBoltStore()
	s.SetOpts(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := s.Init(&bolt.Options{Path: path}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Stop() }()
	if got, err := s.StoredInflightMessages(); err != nil || len(got) != 1 || got[0].PacketID != 1 {
		t.Fatalf("stored in-flight messages: %+v, %v", got, err)
	}
}

func TestPacketIDFromKey(t *testing.T) {
	for key, want := range map[string]uint16{"IFM_alice:4": 4, "IFM_worker-2:65535": 65535} {
		if got, ok := packetIDFromKey(key); !ok || got != want {
			t.Errorf("%s: got %d %v", key, got, ok)
		}
	}
	for _, key := range []string{"IFM_alice", "IFM_alice:0", "IFM_alice:70000", "IFM_alice:x"} {
		if _, ok := packetIDFromKey(key); ok {
			t.Errorf("%s: accepted", key)
		}
	}
}
