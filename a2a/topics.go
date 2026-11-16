// Package a2a defines the agent-to-agent contract carried over MQTT: topic
// layout, the message envelope, presence cards, and validation rules.
//
// It depends only on the standard library so any Go agent can import it
// without pulling in an MQTT client or broker.
package a2a

import (
	"fmt"
	"regexp"
	"strings"
)

// Version is the contract version. It appears in every topic and envelope.
const Version = 1

// Root is the topic prefix for every contract topic.
const Root = "a2a/v1"

// SharePrefix is the MQTT shared-subscription prefix. A subscription to
// "$share/<group>/<filter>" load-balances messages across the group.
const SharePrefix = "$share/"

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// MaxTopicLen caps a topic or filter. Contract topics are short (agent IDs
// and capabilities are at most 64 chars); the cap keeps an event name from
// carrying kilobytes into every log line and audit record.
const MaxTopicLen = 512

// idRE is what an envelope id or correlation_id may contain. IDs appear in
// the text put in front of messages for LLMs (UntrustedPreamble), so they
// must not be able to carry newlines or prose.
var idRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ValidID reports whether s is a valid envelope id or correlation_id.
func ValidID(s string) bool { return idRE.MatchString(s) }

// ValidName reports whether s is a valid agent ID or capability name:
// 1-64 chars of lowercase letters, digits, '.', '_' or '-', not starting
// with punctuation.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// InboxTopic is where direct messages and replies for an agent arrive.
// Any authorized agent may publish here; only the owner may subscribe.
func InboxTopic(agent string) string { return Root + "/agents/" + agent + "/inbox" }

// PresenceTopic holds an agent's retained presence card. Only the owner
// publishes; the broker writes the offline card on disconnect.
func PresenceTopic(agent string) string { return Root + "/agents/" + agent + "/presence" }

// PresenceFilter subscribes to every agent's presence card.
const PresenceFilter = Root + "/agents/+/presence"

// TaskTopic is the work queue for a capability. Workers consume it with
// a shared subscription so each task goes to one worker.
func TaskTopic(capability string) string { return Root + "/tasks/" + capability }

// TaskShareFilter is the shared subscription a worker uses to consume
// tasks for a capability. All workers for a capability share one group.
func TaskShareFilter(capability string) string {
	return SharePrefix + capability + "/" + TaskTopic(capability)
}

// EventTopic is a broadcast topic owned by agent. name may contain '/'
// to form a hierarchy, e.g. "jobs/finished".
func EventTopic(agent, name string) string { return Root + "/events/" + agent + "/" + name }

// Kind classifies a contract topic.
type Kind int

const (
	KindUnknown Kind = iota
	KindInbox
	KindPresence
	KindTask
	KindEvent
)

func (k Kind) String() string {
	switch k {
	case KindInbox:
		return "inbox"
	case KindPresence:
		return "presence"
	case KindTask:
		return "task"
	case KindEvent:
		return "event"
	}
	return "unknown"
}

// Topic is a parsed contract topic or topic filter. For filters, Owner or
// Capability may be "+" and Rest may contain wildcards.
type Topic struct {
	Kind       Kind
	Owner      string // agent ID for inbox, presence and event topics
	Capability string // capability for task topics
	Rest       string // event name for event topics
}

// ParseTopic parses a concrete topic or a topic filter. Any "$share/<group>/"
// prefix is removed first. It returns an error for anything outside the
// contract namespace or not matching a known shape.
func ParseTopic(s string) (Topic, error) {
	if len(s) > MaxTopicLen {
		return Topic{}, fmt.Errorf("topic is %d bytes; the limit is %d", len(s), MaxTopicLen)
	}
	s = StripShare(s)
	if s != Root && !strings.HasPrefix(s, Root+"/") {
		return Topic{}, fmt.Errorf("topic %q is outside %s", s, Root)
	}
	parts := strings.Split(strings.TrimPrefix(s, Root+"/"), "/")
	bad := fmt.Errorf("topic %q does not match the a2a/v1 contract", s)
	switch {
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "inbox":
		return Topic{Kind: KindInbox, Owner: parts[1]}, segmentOK(parts[1], bad)
	case len(parts) == 3 && parts[0] == "agents" && parts[2] == "presence":
		return Topic{Kind: KindPresence, Owner: parts[1]}, segmentOK(parts[1], bad)
	case len(parts) == 2 && parts[0] == "tasks":
		return Topic{Kind: KindTask, Capability: parts[1]}, segmentOK(parts[1], bad)
	case len(parts) >= 2 && parts[0] == "events":
		t := Topic{Kind: KindEvent, Owner: parts[1], Rest: strings.Join(parts[2:], "/")}
		if parts[1] == "#" && len(parts) == 2 {
			t.Owner, t.Rest = "+", "#"
			return t, nil
		}
		if len(parts) < 3 {
			return Topic{}, bad
		}
		return t, segmentOK(parts[1], bad)
	}
	return Topic{}, bad
}

// segmentOK accepts a valid name or a single-level wildcard.
func segmentOK(seg string, bad error) error {
	if seg == "+" || ValidName(seg) {
		return nil
	}
	return bad
}

// StripShare removes a "$share/<group>/" prefix, if present.
func StripShare(filter string) string {
	if len(filter) > len(SharePrefix) && strings.EqualFold(filter[:len(SharePrefix)], SharePrefix) {
		rest := filter[len(SharePrefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			return rest[i+1:]
		}
	}
	return filter
}

// IsShared reports whether filter is a shared subscription.
func IsShared(filter string) bool { return StripShare(filter) != filter }

// HasWildcard reports whether s contains an MQTT wildcard.
func HasWildcard(s string) bool { return strings.ContainsAny(s, "+#") }
