package broker

import "testing"

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
