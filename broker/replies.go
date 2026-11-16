package broker

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
)

const (
	// maxOpenRequests bounds the whole open-request table. When it's full
	// even after sweeping expired entries, new requests aren't tracked and
	// their replies fall back to the responder's send_to.
	maxOpenRequests = 65536
	// maxOpenPerRequester bounds one agent's share of the table. A
	// requester over it loses its own oldest entry, so no single agent can
	// fill the table and switch reply checking off for everyone.
	maxOpenPerRequester = 256
	// maxOpenNoTTL is how long a request without ttl_ms stays open.
	maxOpenNoTTL = 24 * time.Hour
)

// latencyBounds are the upper bounds, in seconds, of the request latency
// histogram buckets. A final bucket catches everything slower.
var latencyBounds = []float64{0.1, 0.5, 1, 5, 15, 60, 300}

// openRequests remembers accepted requests until they are answered, so a
// reply can reach the requester even when the requester is outside the
// responder's send_to, and so only the request's answerer can reply.
// send_to limits who an agent may contact; answering a request someone
// sent you is not contacting them.
//
// An entry allows exactly one reply: a response or error to the
// requester's inbox, with correlation_id set to the request's id, from
// the agent the request was addressed to, or for a task from an agent
// granted its capability.
//
// It also records what happened to each request (answered, errored,
// expired) and how long answers took, per target, for Status and
// /metrics.
type openRequests struct {
	mu    sync.Mutex
	by    map[string]*requesterOpen // requester -> its open requests
	total int
	stats map[string]*requestStats // target ("task:<cap>" or "agent:<id>") -> outcomes
}

type requesterOpen struct {
	m     map[string]openRequest
	order []string // insertion order; may hold IDs already removed
}

type openRequest struct {
	responder  string // inbox requests: the addressed agent
	capability string // task requests: any agent that may consume it
	start      time.Time
	expires    time.Time
}

// target names what the request was sent to.
func (r openRequest) target() string {
	if r.capability != "" {
		return "task:" + r.capability
	}
	return "agent:" + r.responder
}

// allows reports whether agent may answer the request: the agent it was
// addressed to, or for a task, any agent granted the capability.
func (r openRequest) allows(agent *AgentConfig) bool {
	if r.capability != "" {
		return agent.mayConsume(r.capability)
	}
	return r.responder == agent.ID
}

// answerer describes who may answer the request, for refusal messages.
func (r openRequest) answerer() string {
	if r.capability != "" {
		return "an agent serving " + r.capability
	}
	return r.responder
}

type requestStats struct {
	open                                int
	answered, errored, expired, evicted int64
	buckets                             [8]int64 // len(latencyBounds)+1
	latencySum                          float64
}

// RequestStats is what happened to requests sent to one target since the
// broker started.
type RequestStats struct {
	Target   string `json:"target"` // "task:<capability>" or "agent:<id>"
	Open     int    `json:"open"`
	Answered int64  `json:"answered"`
	Errored  int64  `json:"errored"`
	Expired  int64  `json:"expired"`
	// Evicted requests were dropped from tracking because their requester
	// had more than 256 open; their replies fall back to send_to.
	Evicted int64 `json:"evicted"`
	// P50 and P95 are reply latencies in seconds, as the upper bound of
	// the histogram bucket the quantile falls in. 0 with no replies yet.
	P50 float64 `json:"p50_seconds"`
	P95 float64 `json:"p95_seconds"`
	// Buckets are cumulative counts for each bound in LatencyBounds, then
	// +Inf, in Prometheus histogram order.
	Buckets    []int64 `json:"-"`
	LatencySum float64 `json:"-"`
}

func newOpenRequests() *openRequests {
	return &openRequests{by: map[string]*requesterOpen{}, stats: map[string]*requestStats{}}
}

func (o *openRequests) stat(target string) *requestStats {
	s, ok := o.stats[target]
	if !ok {
		s = &requestStats{}
		o.stats[target] = s
	}
	return s
}

// add records a request. It reports false when the table is full.
func (o *openRequests) add(requester, id string, r openRequest, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	ro, ok := o.by[requester]
	if !ok {
		ro = &requesterOpen{m: map[string]openRequest{}}
		o.by[requester] = ro
	}
	if _, dup := ro.m[id]; dup {
		ro.m[id] = r // dedupe normally prevents this; keep counts unchanged
		return true
	}
	for len(ro.m) >= maxOpenPerRequester && len(ro.order) > 0 {
		oldest := ro.order[0]
		ro.order = ro.order[1:]
		if old, ok := ro.m[oldest]; ok {
			delete(ro.m, oldest)
			o.total--
			s := o.stat(old.target())
			s.open--
			s.evicted++
		}
	}
	if o.total >= maxOpenRequests {
		o.sweepLocked(now)
		if o.total >= maxOpenRequests {
			return false
		}
	}
	ro.m[id] = r
	ro.order = append(ro.order, id)
	if len(ro.order) > 2*len(ro.m)+16 {
		ro.compact()
	}
	o.total++
	o.stat(r.target()).open++
	return true
}

func (ro *requesterOpen) compact() {
	kept := ro.order[:0]
	for _, id := range ro.order {
		if _, ok := ro.m[id]; ok {
			kept = append(kept, id)
		}
	}
	ro.order = kept
}

// lookup returns requester's open request id, if it hasn't expired.
func (o *openRequests) lookup(requester, id string, now time.Time) (openRequest, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lookupLocked(requester, id, now)
}

func (o *openRequests) lookupLocked(requester, id string, now time.Time) (openRequest, bool) {
	ro, ok := o.by[requester]
	if !ok {
		return openRequest{}, false
	}
	r, ok := ro.m[id]
	if !ok || now.After(r.expires) {
		return openRequest{}, false
	}
	return r, true
}

// claim settles a reply to requester's request id from agent, atomically,
// so two replies can't both be accepted. found is false when the request
// isn't open (never tracked, already answered, or expired). When found
// and agent may answer, the entry is closed and its outcome recorded;
// when found and agent may not, nothing changes and r says who may.
func (o *openRequests) claim(requester, id string, agent *AgentConfig, typ a2a.Type, now time.Time) (r openRequest, found, allowed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, found = o.lookupLocked(requester, id, now)
	if !found {
		return r, false, false
	}
	if !r.allows(agent) {
		return r, true, false
	}
	delete(o.by[requester].m, id)
	o.dropIfEmpty(requester)
	o.total--
	s := o.stat(r.target())
	s.open--
	if typ == a2a.TypeError {
		s.errored++
	} else {
		s.answered++
	}
	secs := max(now.Sub(r.start).Seconds(), 0)
	s.latencySum += secs
	s.buckets[sort.SearchFloat64s(latencyBounds, secs)]++
	return r, true, true
}

func (o *openRequests) dropIfEmpty(requester string) {
	if ro := o.by[requester]; ro != nil && len(ro.m) == 0 {
		delete(o.by, requester)
	}
}

// expiredRequest is a request that expired unanswered.
type expiredRequest struct {
	requester, id string
	req           openRequest
}

// sweep removes expired requests, counts them, and returns them.
func (o *openRequests) sweep(now time.Time) []expiredRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sweepLocked(now)
}

func (o *openRequests) sweepLocked(now time.Time) []expiredRequest {
	var out []expiredRequest
	for requester, ro := range o.by {
		for id, r := range ro.m {
			if now.After(r.expires) {
				delete(ro.m, id)
				o.total--
				s := o.stat(r.target())
				s.open--
				s.expired++
				out = append(out, expiredRequest{requester, id, r})
			}
		}
		if len(ro.m) == 0 {
			delete(o.by, requester)
		} else if len(ro.order) > 2*len(ro.m)+16 {
			ro.compact()
		}
	}
	return out
}

func (o *openRequests) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.total
}

// snapshot returns per-target stats, sorted by target.
func (o *openRequests) snapshot() []RequestStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]RequestStats, 0, len(o.stats))
	for target, s := range o.stats {
		rs := RequestStats{Target: target, Open: s.open, Answered: s.answered, Errored: s.errored,
			Expired: s.expired, Evicted: s.evicted, LatencySum: s.latencySum}
		var total int64
		for _, n := range s.buckets {
			total += n
		}
		var cum int64
		rs.Buckets = make([]int64, len(s.buckets))
		for i, n := range s.buckets {
			cum += n
			rs.Buckets[i] = cum
		}
		rs.P50, rs.P95 = quantile(rs.Buckets, total, 0.5), quantile(rs.Buckets, total, 0.95)
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// quantile returns the upper bound of the bucket holding quantile q, from
// cumulative bucket counts. The overflow bucket reports +Inf as the last
// finite bound doubled, which is enough for a status display.
func quantile(cum []int64, total int64, q float64) float64 {
	if total == 0 {
		return 0
	}
	rank := int64(math.Ceil(q * float64(total)))
	for i, c := range cum {
		if c >= rank {
			if i < len(latencyBounds) {
				return latencyBounds[i]
			}
			return 2 * latencyBounds[len(latencyBounds)-1]
		}
	}
	return 0
}

// openExpiry is when a request accepted at now stops accepting replies:
// its TTL counted from the later of now and its ts (a requester whose
// clock runs behind shouldn't lose its reply window), or maxOpenNoTTL
// without a TTL.
func openExpiry(env *a2a.Envelope, now time.Time) time.Time {
	if env.TTLMillis <= 0 {
		return now.Add(maxOpenNoTTL)
	}
	base := env.TS
	if now.After(base) {
		base = now
	}
	return base.Add(time.Duration(env.TTLMillis) * time.Millisecond)
}
