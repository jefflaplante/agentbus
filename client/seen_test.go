package client

import (
	"testing"
	"time"
)

func TestSeenIDs(t *testing.T) {
	s := newSeenIDs(time.Minute, 3)
	now := time.Now()
	if !s.first("a", now) || s.first("a", now) {
		t.Fatal("duplicate within window not caught")
	}
	if !s.first("a", now.Add(2*time.Minute)) {
		t.Error("key not forgotten after window")
	}
	for _, k := range []string{"b", "c", "d", "e"} {
		s.first(k, now.Add(2*time.Minute))
	}
	if len(s.at) > 3 {
		t.Errorf("grew past max: %d", len(s.at))
	}
}
