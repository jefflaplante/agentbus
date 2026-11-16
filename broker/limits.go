package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// rateLimiter is a token bucket per key (usually an agent ID). Rate and
// burst are passed per call so agents can have their own limits.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newRateLimiter() *rateLimiter { return &rateLimiter{buckets: map[string]*bucket{}} }

// allow takes one token for key at now. A rate of 0 disables limiting.
func (r *rateLimiter) allow(key string, now time.Time, rate float64, burst int) bool {
	if rate == 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), last: now}
		r.buckets[key] = b
	}
	b.tokens = min(float64(burst), b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// prune drops buckets idle for longer than idle. Keys can come from
// remote addresses, so without this the map would grow without bound.
// An idle bucket has refilled anyway, so dropping it changes nothing.
func (r *rateLimiter) prune(now time.Time, idle time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, b := range r.buckets {
		if now.Sub(b.last) > idle {
			delete(r.buckets, k)
		}
	}
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
	if len(d.seen) >= maxDedupeEntries {
		return true // full: deliver rather than grow without bound
	}
	d.seen[key] = now
	return true
}

// maxDedupeEntries bounds dedupe memory. Per-agent rate limits already
// bound the insert rate; this is a backstop.
const maxDedupeEntries = 1 << 20

// save writes entries still inside the window to path atomically.
func (d *dedupe) save(path string, now time.Time) error {
	d.mu.Lock()
	keep := make(map[string]int64, len(d.seen))
	for k, t := range d.seen {
		if now.Sub(t) <= d.window {
			keep[k] = t.UnixMilli()
		}
	}
	d.mu.Unlock()
	data, err := json.Marshal(keep)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// load restores entries still inside the window. A missing file is fine.
func (d *dedupe) load(path string, now time.Time) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var saved map[string]int64
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, ms := range saved {
		if t := time.UnixMilli(ms); now.Sub(t) <= d.window && len(d.seen) < maxDedupeEntries {
			d.seen[k] = t
		}
	}
	return nil
}
