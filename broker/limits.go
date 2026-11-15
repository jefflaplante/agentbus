package broker

import (
	"sync"
	"time"
)

// rateLimiter is a token bucket per agent.
type rateLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter(perSecond float64, burst int) *rateLimiter {
	return &rateLimiter{rate: perSecond, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes one token for agent at now. A rate of 0 disables limiting.
func (r *rateLimiter) allow(agent string, now time.Time) bool {
	if r.rate == 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[agent]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[agent] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// dedupe remembers (sender, envelope ID) pairs for a window so a
// retransmitted envelope is delivered once.
type dedupe struct {
	mu        sync.Mutex
	window    time.Duration
	seen      map[string]time.Time
	lastSweep time.Time
}

func newDedupe(window time.Duration) *dedupe {
	return &dedupe{window: window, seen: map[string]time.Time{}}
}

// firstSeen records key and reports whether it was not already present.
func (d *dedupe) firstSeen(key string, now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.lastSweep) > d.window/4 {
		for k, t := range d.seen {
			if now.Sub(t) > d.window {
				delete(d.seen, k)
			}
		}
		d.lastSweep = now
	}
	if t, ok := d.seen[key]; ok && now.Sub(t) <= d.window {
		return false
	}
	d.seen[key] = now
	return true
}
