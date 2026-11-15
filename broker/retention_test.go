package broker_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

// persistBus runs brokers over one data_dir, so a test can restart the
// broker and check what survived.
type persistBus struct {
	t      *testing.T
	dir    string
	audit  string
	tokens map[string]string
	agents []broker.AgentConfig
	srv    *broker.Broker
	url    string
}

func newPersistBus(t *testing.T) *persistBus {
	p := &persistBus{t: t, dir: t.TempDir(), tokens: map[string]string{}}
	p.audit = filepath.Join(p.dir, "audit.jsonl")
	for _, a := range []broker.AgentConfig{
		{ID: "alice"},
		{ID: "bob", Capabilities: []string{"summarize"}},
		{ID: "ops", Observer: true},
	} {
		tok, hash := broker.NewToken()
		a.TokenSHA256 = hash
		p.tokens[a.ID] = tok
		p.agents = append(p.agents, a)
	}
	p.start()
	return p
}

func (p *persistBus) config() *broker.Config {
	return &broker.Config{
		Listen: "127.0.0.1:0", DataDir: filepath.Join(p.dir, "data"), AuditFile: p.audit,
		Agents: append([]broker.AgentConfig(nil), p.agents...),
	}
}

func (p *persistBus) start() {
	p.t.Helper()
	srv, err := broker.New(p.config(), quietLog())
	if err != nil {
		p.t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		p.t.Fatal(err)
	}
	p.srv, p.url = srv, "tcp://"+srv.Addr()
	p.t.Cleanup(func() { _ = srv.Close() })
}

func (p *persistBus) restart() {
	p.t.Helper()
	if err := p.srv.Close(); err != nil {
		p.t.Fatal(err)
	}
	p.start()
}

// connect makes a persistent-session client (CleanSession false), the way
// a long-running agent connects.
func (p *persistBus) connect(id string, inbox client.Handler) *client.Client {
	return p.connectWith(id, inbox, nil)
}

func (p *persistBus) connectWith(id string, inbox client.Handler, tasks map[string]client.Handler) *client.Client {
	p.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{
		BrokerURL: p.url, AgentID: id, Token: p.tokens[id], Inbox: inbox, Tasks: tasks,
		Logger: quietLog(), Description: id + " persistent",
	})
	if err != nil {
		p.t.Fatalf("connect %s: %v", id, err)
	}
	p.t.Cleanup(c.Close)
	return c
}

func TestQueuedMessagesSurviveRestart(t *testing.T) {
	p := newPersistBus(t)
	p.connect("alice", nil).Close() // alice goes offline; her session stays
	bob := p.connect("bob", nil)
	if err := bob.Send(ctxT(t), "alice", map[string]string{"note": "while you were out"}); err != nil {
		t.Fatal(err)
	}
	bob.Close()
	time.Sleep(100 * time.Millisecond) // let the broker queue it

	p.restart()
	inbox := newRecorder()
	p.connect("alice", inbox.handler)
	got := inbox.next(t)
	var body map[string]string
	if err := got.DecodeBody(&body); err != nil || body["note"] != "while you were out" || got.From != "bob" {
		t.Fatalf("got %+v body %v", got, body)
	}
}

func TestTaskSubscriptionSurvivesRestart(t *testing.T) {
	p := newPersistBus(t)
	worker := p.connect("bob", nil)
	if err := worker.ServeTasks(ctxT(t), "summarize", func(context.Context, *a2a.Envelope) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	worker.Close()
	p.restart()

	alice := p.connect("alice", nil)
	reply := make(chan error, 1)
	go func() {
		_, err := alice.RequestTask(ctxT(t), "summarize", "queued while the worker is away")
		reply <- err
	}()
	time.Sleep(200 * time.Millisecond) // the task is queued on bob's restored session, not refused
	p.connectWith("bob", nil, map[string]client.Handler{
		"summarize": func(_ context.Context, e *a2a.Envelope) (any, error) { return "handled after restart", nil },
	})
	if err := <-reply; err != nil {
		t.Fatalf("task: %v", err)
	}
}

func TestPresenceRestoredOffline(t *testing.T) {
	p := newPersistBus(t)
	p.connect("bob", nil) // online when the broker stops
	p.restart()

	alice := p.connect("alice", nil)
	waitFor(t, func() bool {
		for _, c := range alice.Agents() {
			if c.Agent == "bob" {
				return c.Status == a2a.StatusOffline && c.Description == "bob persistent"
			}
		}
		return false
	}, "bob's restored card, offline with its description")
}

func TestDedupeAndAuditAcrossRestart(t *testing.T) {
	p := newPersistBus(t)
	bobInbox := newRecorder()
	p.connect("bob", bobInbox.handler)
	alice := p.connect("alice", nil)

	env, _ := a2a.New(a2a.TypeEvent, "alice", map[string]string{"secret": "do-not-log-me"})
	env.To = "bob"
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	bobInbox.next(t)

	p.restart()
	bobInbox = newRecorder()
	p.connect("bob", bobInbox.handler)
	alice = p.connect("alice", nil)
	env.TS = time.Now().UTC() // same id, fresh timestamp: still a duplicate
	if err := alice.Publish(ctxT(t), a2a.InboxTopic("bob"), env); err != nil {
		t.Fatal(err)
	}
	bobInbox.none(t, 300*time.Millisecond)
	_ = p.srv.Close() // flush the audit file

	recs := readAudit(t, p.audit)
	count := map[string]int{}
	for _, r := range recs {
		count[r.Action]++
		if r.ID == env.ID && r.Action == "accept" && (r.BodySHA256 == "" || r.From != "alice" || r.To != "bob") {
			t.Errorf("accept record incomplete: %+v", r)
		}
	}
	if count["accept"] < 1 || count["duplicate"] != 1 || count["connect"] < 4 || count["presence"] < 4 {
		t.Errorf("audit actions = %v", count)
	}
	raw, _ := os.ReadFile(p.audit)
	if strings.Contains(string(raw), "do-not-log-me") {
		t.Error("audit file contains a message body")
	}
}

func readAudit(t *testing.T, path string) []broker.AuditRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var recs []broker.AuditRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r broker.AuditRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad audit line %q: %v", sc.Text(), err)
		}
		recs = append(recs, r)
	}
	return recs
}

func TestAuditRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	token, hash := broker.NewToken()
	cfg := &broker.Config{
		Listen: "127.0.0.1:0", AuditFile: path, AuditMaxBytes: 1024, AuditKeep: 2,
		Agents: []broker.AgentConfig{{ID: "alice", TokenSHA256: hash}},
	}
	srv, err := broker.New(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	// Each connect writes connect, presence and disconnect records.
	for range 12 {
		c, err := client.Connect(ctxT(t), client.Options{BrokerURL: "tcp://" + srv.Addr(), AgentID: "alice", Token: token, CleanSession: true, Logger: quietLog()})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	_ = srv.Close()
	for _, name := range []string{"audit.jsonl", "audit.jsonl.1", "audit.jsonl.2"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Size() > 1024 {
			t.Errorf("%s is %d bytes, over the 1024 limit", name, st.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.3")); err == nil {
		t.Error("kept more than audit_keep old files")
	}
}

func TestSecondBrokerOnSameDataDirRefused(t *testing.T) {
	p := newPersistBus(t)
	cfg := p.config()
	if srv, err := broker.New(cfg, quietLog()); err == nil {
		_ = srv.Close()
		t.Fatal("second broker opened a data_dir that is in use")
	}
}

// With persistent sessions an offline worker keeps its share subscription.
// Tasks must go to the connected worker, not queue for the offline one.
func TestTasksPreferConnectedWorker(t *testing.T) {
	p := newPersistBus(t)
	p.agents = append(p.agents, broker.AgentConfig{ID: "carol", Capabilities: []string{"summarize"}})
	tok, hash := broker.NewToken()
	p.tokens["carol"] = tok
	p.agents[len(p.agents)-1].TokenSHA256 = hash
	p.restart() // pick up carol

	handled := map[string]int{}
	var mu sync.Mutex
	worker := func(name string) client.Handler {
		return func(context.Context, *a2a.Envelope) (any, error) {
			mu.Lock()
			handled[name]++
			mu.Unlock()
			return name, nil
		}
	}
	p.connectWith("carol", nil, map[string]client.Handler{"summarize": worker("carol")}).Close() // offline, session kept
	p.connectWith("bob", nil, map[string]client.Handler{"summarize": worker("bob")})
	alice := p.connect("alice", nil)
	for i := range 20 {
		if _, err := alice.RequestTask(ctxT(t), "summarize", i); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	if handled["bob"] != 20 {
		t.Errorf("handled = %v, want all 20 by the connected worker", handled)
	}
}
