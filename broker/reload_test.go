package broker_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/broker"
	"github.com/jefflaplante/mqtt/client"
)

// reloadBus runs a broker from a config file the test can rewrite.
type reloadBus struct {
	t      *testing.T
	path   string
	cfg    broker.Config
	tokens map[string]string
	srv    *broker.Broker
	url    string
}

func newReloadBus(t *testing.T, agents ...broker.AgentConfig) *reloadBus {
	t.Helper()
	r := &reloadBus{t: t, path: filepath.Join(t.TempDir(), "broker.json"), tokens: map[string]string{}}
	r.cfg = broker.Config{Listen: "127.0.0.1:0"}
	for _, a := range agents {
		r.add(a)
	}
	r.write()
	cfg, err := broker.LoadConfig(r.path)
	if err != nil {
		t.Fatal(err)
	}
	if r.srv, err = broker.New(cfg, quietLog()); err != nil {
		t.Fatal(err)
	}
	if err := r.srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.srv.Close() })
	r.url = "tcp://" + r.srv.Addr()
	return r
}

func (r *reloadBus) add(a broker.AgentConfig) {
	tok, hash := broker.NewToken()
	a.TokenSHA256, r.tokens[a.ID] = hash, tok
	r.cfg.Agents = append(r.cfg.Agents, a)
}

func (r *reloadBus) write() {
	r.t.Helper()
	data, _ := json.Marshal(r.cfg)
	if err := os.WriteFile(r.path, data, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reloadBus) reload() broker.ReloadResult {
	r.t.Helper()
	r.write()
	res, err := r.srv.ReloadFrom(r.path)
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *reloadBus) connect(id string, tasks map[string]client.Handler) (*client.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := client.Connect(ctx, client.Options{BrokerURL: r.url, AgentID: id, Token: r.tokens[id],
		CleanSession: true, Logger: quietLog(), Tasks: tasks})
	if err == nil {
		r.t.Cleanup(c.Close)
	}
	return c, err
}

func TestReloadAddsAndRemovesAgents(t *testing.T) {
	r := newReloadBus(t, broker.AgentConfig{ID: "alice"}, broker.AgentConfig{ID: "ops", Observer: true})
	lost := make(chan struct{}, 1)
	opts := paho.NewClientOptions().AddBroker(r.url).SetClientID("alice").SetUsername("alice").
		SetPassword(r.tokens["alice"]).SetAutoReconnect(false).
		SetConnectionLostHandler(func(paho.Client, error) { lost <- struct{}{} })
	alice := paho.NewClient(opts)
	if tok := alice.Connect(); !tok.WaitTimeout(3*time.Second) || tok.Error() != nil {
		t.Fatal(tok.Error())
	}
	if _, err := r.connect("bob", nil); err == nil {
		t.Fatal("bob connected before being added")
	}

	r.cfg.Agents = slices.DeleteFunc(r.cfg.Agents, func(a broker.AgentConfig) bool { return a.ID == "alice" })
	r.add(broker.AgentConfig{ID: "bob"})
	res := r.reload()
	if !slices.Equal(res.Added, []string{"bob"}) || !slices.Equal(res.Removed, []string{"alice"}) ||
		!slices.Equal(res.Disconnected, []string{"alice"}) {
		t.Errorf("result = %+v", res)
	}
	select {
	case <-lost:
	case <-time.After(3 * time.Second):
		t.Error("removed agent was not disconnected")
	}
	if _, err := r.connect("bob", nil); err != nil {
		t.Errorf("added agent cannot connect: %v", err)
	}
	if st := r.srv.Status(); st.Config.Reloads != 1 || st.Config.Path != r.path || len(st.Agents) != 2 {
		t.Errorf("status config = %+v, %d agents", st.Config, len(st.Agents))
	}
}

func TestReloadRotatedTokenLocksOutOldToken(t *testing.T) {
	r := newReloadBus(t, broker.AgentConfig{ID: "alice"})
	old := r.tokens["alice"]
	tok, hash := broker.NewToken()
	r.cfg.Agents[0].TokenSHA256, r.tokens["alice"] = hash, tok
	res := r.reload()
	if !slices.Equal(res.Changed, []string{"alice"}) {
		t.Errorf("result = %+v", res)
	}
	if _, err := r.connect("alice", nil); err != nil {
		t.Errorf("new token rejected: %v", err)
	}
	r.tokens["alice"] = old
	if _, err := r.connect("alice", nil); err == nil {
		t.Error("old token still accepted")
	}
}

func TestReloadRevokedCapabilityStopsTasks(t *testing.T) {
	r := newReloadBus(t, broker.AgentConfig{ID: "alice"}, broker.AgentConfig{ID: "bob", Capabilities: []string{"summarize"}})
	handled := make(chan struct{}, 4)
	if _, err := r.connect("bob", map[string]client.Handler{"summarize": func(context.Context, *a2a.Envelope) (any, error) {
		handled <- struct{}{}
		return "ok", nil
	}}); err != nil {
		t.Fatal(err)
	}
	alice, err := r.connect("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := alice.RequestTask(ctxT(t), "summarize", "before"); err != nil {
		t.Fatalf("before revoke: %v", err)
	}

	r.cfg.Agents[1].Capabilities = nil
	r.reload()
	_, err = alice.RequestTask(ctxT(t), "summarize", "after")
	if rc := remoteCode(t, err); rc != "no_consumer" {
		t.Errorf("after revoke: %s", rc)
	}
	if len(handled) != 1 {
		t.Errorf("revoked worker handled %d tasks", len(handled))
	}
}

func TestReloadReportsRestartAndKeepsGoodConfig(t *testing.T) {
	r := newReloadBus(t, broker.AgentConfig{ID: "alice"})
	r.cfg.Listen = "127.0.0.1:1"
	r.cfg.RatePerSecond = 5
	res := r.reload()
	if !slices.Contains(res.RestartNeeded, "listen") || slices.Contains(res.RestartNeeded, "rate_per_second") {
		t.Errorf("restart needed = %v", res.RestartNeeded)
	}
	if st := r.srv.Status(); st.Limits.RatePerSecond != 5 || st.Listen == "127.0.0.1:1" {
		t.Errorf("live rate %v, listen %s", st.Limits.RatePerSecond, st.Listen)
	}

	if err := os.WriteFile(r.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.srv.ReloadFrom(r.path); err == nil {
		t.Fatal("bad config accepted")
	}
	if st := r.srv.Status(); st.Config.LastError == "" {
		t.Error("reload error not reported in status")
	}
	if _, err := r.connect("alice", nil); err != nil {
		t.Errorf("running config lost after a bad reload: %v", err)
	}
}

func TestWatchConfigReloadsOnChange(t *testing.T) {
	r := newReloadBus(t, broker.AgentConfig{ID: "alice"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.srv.WatchConfig(ctx, r.path, 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	r.add(broker.AgentConfig{ID: "bob"})
	r.write()
	waitFor(t, func() bool { return r.srv.Status().Config.Reloads == 1 }, "the watcher to reload")
	if _, err := r.connect("bob", nil); err != nil {
		t.Errorf("agent added by file edit cannot connect: %v", err)
	}
}
