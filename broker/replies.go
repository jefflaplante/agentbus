package broker

import (
	"sync"
	"time"
)

// maxOpenRequests bounds the open-request table. When it's full and holds
// nothing expired, new requests aren't tracked and their replies fall back
// to the responder's send_to.
const maxOpenRequests = 65536

// openRequests remembers accepted requests until they are answered, so a
// reply can reach the requester even when the requester is outside the
// responder's send_to. send_to limits who an agent may contact; answering
// a request someone sent you is not contacting them.
//
// An entry allows exactly one reply: a response or error to the
// requester's inbox, with correlation_id set to the request's id, from
// the agent the request was addressed to, or for a task from an agent
// granted its capability.
type openRequests struct {
	mu   sync.Mutex
	m    map[string]openRequest
	full bool // logged once per fill-up
}

type openRequest struct {
	responder  string // inbox requests: the addressed agent
	capability string // task requests: any agent that may consume it
	expires    time.Time
}

func newOpenRequests() *openRequests {
	return &openRequests{m: map[string]openRequest{}}
}

func openKey(requester, id string) string { return requester + "/" + id }

// add records a request. It reports false when the table is full.
func (o *openRequests) add(requester, id string, r openRequest, now time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.m) >= maxOpenRequests {
		for k, v := range o.m {
			if now.After(v.expires) {
				delete(o.m, k)
			}
		}
		if len(o.m) >= maxOpenRequests {
			return false
		}
	}
	o.m[openKey(requester, id)] = r
	return true
}

// lookup returns requester's open request id, if it hasn't expired.
func (o *openRequests) lookup(requester, id string, now time.Time) (openRequest, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.m[openKey(requester, id)]
	if !ok || now.After(r.expires) {
		return openRequest{}, false
	}
	return r, true
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

// match reports whether agent may answer requester's request id. It
// doesn't consume the entry; take does, once the reply is accepted.
func (o *openRequests) match(requester, id string, agent *AgentConfig, now time.Time) bool {
	r, ok := o.lookup(requester, id, now)
	return ok && r.allows(agent)
}

func (o *openRequests) take(requester, id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.m, openKey(requester, id))
}

func (o *openRequests) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.m)
}
