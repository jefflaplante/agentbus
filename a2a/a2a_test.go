package a2a

import (
	"errors"
	"testing"
	"time"
)

func TestParseTopic(t *testing.T) {
	tests := []struct {
		in      string
		kind    Kind
		owner   string
		cap     string
		wantErr bool
	}{
		{in: "a2a/v1/agents/alice/inbox", kind: KindInbox, owner: "alice"},
		{in: "a2a/v1/agents/+/presence", kind: KindPresence, owner: "+"},
		{in: "a2a/v1/tasks/summarize", kind: KindTask, cap: "summarize"},
		{in: "$share/summarize/a2a/v1/tasks/summarize", kind: KindTask, cap: "summarize"},
		{in: "a2a/v1/events/alice/jobs/done", kind: KindEvent, owner: "alice"},
		{in: "a2a/v1/events/#", kind: KindEvent, owner: "+"},
		{in: "a2a/v1/events/+/#", kind: KindEvent, owner: "+"},
		{in: "a2a/v1/events/alice", wantErr: true},
		{in: "a2a/v1/agents/Alice/inbox", wantErr: true},
		{in: "a2a/v1/agents/#", wantErr: true},
		{in: "a2a/#", wantErr: true},
		{in: "#", wantErr: true},
		{in: "zigbee2mqtt/kitchen", wantErr: true},
		{in: "a2a/v1/tasks/a/b", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseTopic(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseTopic(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && (got.Kind != tt.kind || got.Owner != tt.owner || got.Capability != tt.cap) {
			t.Errorf("ParseTopic(%q) = %+v", tt.in, got)
		}
	}
}

func TestStripShare(t *testing.T) {
	if got := StripShare("$share/g/a2a/v1/tasks/x"); got != "a2a/v1/tasks/x" {
		t.Errorf("got %q", got)
	}
	if got := StripShare("$SHARE/g/a2a/v1/tasks/x"); got != "a2a/v1/tasks/x" {
		t.Errorf("case-insensitive prefix: got %q", got)
	}
	if got := StripShare("a2a/v1/tasks/x"); got != "a2a/v1/tasks/x" {
		t.Errorf("got %q", got)
	}
}

func validEnvelope() *Envelope {
	e, _ := New(TypeRequest, "alice", map[string]string{"q": "hi"})
	e.To = "bob"
	return e
}

func TestEnvelopeValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Envelope)
		ok     bool
	}{
		{"valid", func(*Envelope) {}, true},
		{"bad version", func(e *Envelope) { e.V = 2 }, false},
		{"no id", func(e *Envelope) { e.ID = "" }, false},
		{"bad type", func(e *Envelope) { e.Type = "shout" }, false},
		{"bad from", func(e *Envelope) { e.From = "Alice!" }, false},
		{"broker from", func(e *Envelope) { e.From = BrokerID }, true},
		{"no ts", func(e *Envelope) { e.TS = time.Time{} }, false},
		{"response without correlation", func(e *Envelope) { e.Type = TypeResponse }, false},
		{"reply_to inbox", func(e *Envelope) { e.ReplyTo = InboxTopic("carol") }, true},
		{"reply_to task", func(e *Envelope) { e.ReplyTo = TaskTopic("x") }, false},
		{"reply_to wildcard", func(e *Envelope) { e.ReplyTo = "a2a/v1/agents/+/inbox" }, false},
		{"body not json", func(e *Envelope) { e.Body = []byte("{nope") }, false},
	}
	for _, tt := range tests {
		e := validEnvelope()
		tt.mutate(e)
		err := e.Validate()
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want ok=%v", tt.name, err, tt.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error %v does not wrap ErrInvalid", tt.name, err)
		}
	}
}

func TestValidateFor(t *testing.T) {
	inbox, _ := ParseTopic(InboxTopic("bob"))
	task, _ := ParseTopic(TaskTopic("summarize"))
	event, _ := ParseTopic(EventTopic("alice", "done"))

	e := validEnvelope()
	if err := e.ValidateFor(inbox, "alice"); err != nil {
		t.Errorf("valid inbox publish: %v", err)
	}
	if err := e.ValidateFor(inbox, "mallory"); err == nil {
		t.Error("spoofed from accepted")
	}
	e.To = "carol"
	if err := e.ValidateFor(inbox, "alice"); err == nil {
		t.Error("to/inbox mismatch accepted")
	}

	e = validEnvelope()
	e.To = ""
	if err := e.ValidateFor(task, "alice"); err == nil {
		t.Error("task without capability accepted")
	}
	e.Capability = "summarize"
	if err := e.ValidateFor(task, "alice"); err != nil {
		t.Errorf("valid task publish: %v", err)
	}
	if err := e.ValidateFor(event, "alice"); err == nil {
		t.Error("request on event topic accepted")
	}
}

func TestRoundTrip(t *testing.T) {
	e := validEnvelope()
	e.TTLMillis = 5000
	data, err := e.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]string
	if err := got.DecodeBody(&body); err != nil || body["q"] != "hi" {
		t.Fatalf("body = %v, err %v", body, err)
	}
	r, _ := got.Reply(TypeResponse, "bob", "ok")
	if r.To != "alice" || r.CorrelationID != e.ID || got.ReplyTopic() != InboxTopic("alice") {
		t.Errorf("reply = %+v", r)
	}
	if got.Expired(e.TS.Add(time.Second)) || !got.Expired(e.TS.Add(6*time.Second)) {
		t.Error("Expired wrong")
	}
}

func TestParsePresence(t *testing.T) {
	if _, err := ParsePresence([]byte(`{"v":1,"agent":"bob","capabilities":["summarize"]}`)); err != nil {
		t.Errorf("valid card: %v", err)
	}
	if _, err := ParsePresence([]byte(`{"v":1,"agent":"bob","capabilities":["Bad Cap"]}`)); err == nil {
		t.Error("bad capability accepted")
	}
}
