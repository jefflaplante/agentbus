package broker

import (
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
)

// One requester can't fill the table: past its quota it evicts its own
// oldest entries, and everyone else's stay tracked.
func TestOpenRequestsPerRequesterQuota(t *testing.T) {
	o := newOpenRequests()
	now := time.Now()
	r := openRequest{responder: "bob", start: now, expires: now.Add(time.Minute)}
	for i := range maxOpenPerRequester + 44 {
		if !o.add("mallory", a2a.NewID()+string(rune('a'+i%26)), r, now) {
			t.Fatal("add refused")
		}
	}
	if !o.add("alice", "a1", r, now) {
		t.Fatal("alice's request not tracked")
	}
	if o.len() != maxOpenPerRequester+1 {
		t.Errorf("open = %d, want %d", o.len(), maxOpenPerRequester+1)
	}
	if _, ok := o.lookup("alice", "a1", now); !ok {
		t.Error("alice's request was evicted by mallory's")
	}
	var evicted int64
	for _, s := range o.snapshot() {
		evicted += s.Evicted
	}
	if evicted != 44 {
		t.Errorf("evicted = %d, want 44", evicted)
	}
}

func TestOpenRequestsClaimOnceAndRecordOutcome(t *testing.T) {
	o := newOpenRequests()
	now := time.Now()
	o.add("alice", "r1", openRequest{capability: "work", start: now, expires: now.Add(time.Minute)}, now)
	worker := &AgentConfig{ID: "bob", Capabilities: []string{"work"}}
	other := &AgentConfig{ID: "carol"}
	if _, found, allowed := o.claim("alice", "r1", other, a2a.TypeResponse, now); !found || allowed {
		t.Errorf("non-worker claim: found=%v allowed=%v", found, allowed)
	}
	if _, found, allowed := o.claim("alice", "r1", worker, a2a.TypeError, now.Add(2*time.Second)); !found || !allowed {
		t.Fatalf("worker claim: found=%v allowed=%v", found, allowed)
	}
	if _, found, _ := o.claim("alice", "r1", worker, a2a.TypeResponse, now); found {
		t.Error("second reply accepted")
	}
	s := o.snapshot()
	if len(s) != 1 || s[0].Target != "task:work" || s[0].Errored != 1 || s[0].Open != 0 || s[0].P50 != 5 {
		t.Errorf("stats = %+v", s)
	}
}

func TestOpenRequestsSweepAndExpiry(t *testing.T) {
	o := newOpenRequests()
	now := time.Now()
	o.add("alice", "r1", openRequest{responder: "bob", start: now, expires: now.Add(time.Second)}, now)
	o.add("alice", "r2", openRequest{responder: "bob", start: now, expires: now.Add(time.Hour)}, now)
	if got := o.sweep(now.Add(2 * time.Second)); len(got) != 1 || got[0].id != "r1" {
		t.Fatalf("swept %+v", got)
	}
	if s := o.snapshot(); s[0].Expired != 1 || s[0].Open != 1 {
		t.Errorf("stats = %+v", s)
	}

	// A requester whose clock runs behind still gets its full TTL.
	env := &a2a.Envelope{TS: now.Add(-time.Minute), TTLMillis: 30_000}
	if exp := openExpiry(env, now); exp.Before(now.Add(29 * time.Second)) {
		t.Errorf("expiry %v is before now+ttl", exp.Sub(now))
	}
	if exp := openExpiry(&a2a.Envelope{TS: now}, now); exp != now.Add(maxOpenNoTTL) {
		t.Errorf("no-TTL expiry = %v", exp.Sub(now))
	}
}

func TestIPKeyGroupsIPv6By64(t *testing.T) {
	if a, b := ipKey("2001:db8::1"), ipKey("2001:db8::ffff"); a != b {
		t.Errorf("same /64 keyed apart: %s %s", a, b)
	}
	if ipKey("192.0.2.1") != "192.0.2.1" {
		t.Error("IPv4 changed")
	}
}
