package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

// joinBus is a broker started from a config file, with the join endpoint
// on, that reloads the file as "agentbus serve" does.
type joinBus struct {
	loc     confstore.Location
	url     string // MQTT
	joinURL string
}

func startJoinBus(t *testing.T) *joinBus {
	t.Helper()
	isolate(t)
	loc := confstore.Resolve("")
	if _, err := confstore.Bootstrap(loc); err != nil {
		t.Fatal(err)
	}
	if _, err := change(loc, func(tx *confstore.Tx) error {
		tx.Config.Listen, tx.Config.JoinListen = "127.0.0.1:0", "127.0.0.1:0"
		tx.Config.DataDir, tx.Config.AuditFile = "", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := broker.LoadConfig(loc.Path)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := broker.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go b.WatchConfig(ctx, loc.Path, 20*time.Millisecond)
	stop, addr, err := startJoinEndpoint(newJoinServer(loc, b.Addr(), false, log), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); cancel(); _ = b.Close() })
	return &joinBus{loc: loc, url: "tcp://" + b.Addr(), joinURL: "http://" + addr}
}

// connects reports whether the token in file gets id onto the bus,
// allowing a moment for the broker to reload its config.
func (j *joinBus) connects(t *testing.T, id, file string, wait time.Duration) bool {
	t.Helper()
	tok, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(wait); ; time.Sleep(30 * time.Millisecond) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := client.Connect(ctx, client.Options{BrokerURL: j.url, AgentID: id, Token: strings.TrimSpace(string(tok)),
			CleanSession: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		cancel()
		if err == nil {
			c.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

func (j *joinBus) join(t *testing.T, id, file string, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, "", append([]string{"join", "--join-url", j.joinURL, "--agent", id, "--token-file", file}, args...)...)
}

// A client asks to join, can't connect until an operator approves, and
// then has exactly what the operator granted.
func TestJoinNeedsApproval(t *testing.T) {
	j := startJoinBus(t)
	file := filepath.Join(t.TempDir(), "worker-7.token")

	out, stderr, err := j.join(t, "worker-7", file, "--capability", "summarize", "--capability", "translate",
		"--description", "build box")
	if err != nil || out != "" || !strings.Contains(stderr, "Waiting for an operator") {
		t.Fatalf("join: %v\nstdout %q\nstderr %s", err, out, stderr)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	if j.connects(t, "worker-7", file, 0) {
		t.Fatal("connected before approval")
	}
	// Asking again changes nothing and keeps the same token.
	before, _ := os.ReadFile(file)
	if _, stderr, err := j.join(t, "worker-7", file); err != nil || !strings.Contains(stderr, "Waiting") {
		t.Fatalf("second join: %v %s", err, stderr)
	}
	if after, _ := os.ReadFile(file); !bytes.Equal(before, after) {
		t.Error("the token changed on the second join")
	}
	// Someone else can't take the ID while it waits.
	if _, _, err := j.join(t, "worker-7", filepath.Join(t.TempDir(), "thief.token")); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("a second token claimed the waiting ID: %v", err)
	}
	if _, _, err := j.join(t, "admin", filepath.Join(t.TempDir(), "thief.token")); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("a join claimed an existing agent's ID: %v", err)
	}

	list, _, err := runCLI(t, "", "config", "join")
	if err != nil || !strings.Contains(list, "worker-7") || !strings.Contains(list, "summarize,translate") || !strings.Contains(list, "build box") {
		t.Fatalf("config join: %v\n%s", err, list)
	}

	if _, stderr, err := runCLI(t, "", "config", "join", "approve", "worker-7", "--capability", "summarize"); err != nil {
		t.Fatalf("approve: %v %s", err, stderr)
	}
	cfg, _ := confstore.Load(j.loc.Path)
	i := confstore.Find(cfg, "worker-7")
	if i < 0 {
		t.Fatal("approved agent is not in the config")
	}
	if a := cfg.Agents[i]; strings.Join(a.Capabilities, ",") != "summarize" || a.Observer ||
		a.TokenSHA256 != broker.HashToken(strings.TrimSpace(string(before))) {
		t.Errorf("agent = %+v", a)
	}
	if _, ok := confstore.FindPending(j.loc, "worker-7"); ok {
		t.Error("still pending after approval")
	}
	if !j.connects(t, "worker-7", file, 3*time.Second) {
		t.Fatal("could not connect after approval")
	}

	out, stderr, err = j.join(t, "worker-7", file, "--capability", "summarize", "--capability", "translate")
	if err != nil {
		t.Fatalf("join after approval: %v %s", err, stderr)
	}
	if !strings.Contains(out, "AGENTBUS_URL="+j.url) || !strings.Contains(out, "AGENTBUS_AGENT=worker-7") ||
		!strings.Contains(out, "AGENTBUS_TOKEN_FILE=") {
		t.Errorf("settings on stdout:\n%s", out)
	}
	if !strings.Contains(stderr, "granted capabilities summarize") || !strings.Contains(stderr, "not granted: translate") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// Without --capability the operator grants what was asked; --capability=
// grants nothing.
func TestJoinApproveDefaults(t *testing.T) {
	j := startJoinBus(t)
	dir := t.TempDir()
	for _, id := range []string{"asks", "none"} {
		if _, stderr, err := j.join(t, id, filepath.Join(dir, id+".token"), "--capability", "stats"); err != nil {
			t.Fatalf("%v %s", err, stderr)
		}
	}
	if _, _, err := runCLI(t, "", "config", "join", "approve", "asks", "--send-to", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "config", "join", "approve", "none", "--capability="); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "config", "join", "approve", "nobody"); err == nil {
		t.Error("approved a request that doesn't exist")
	}
	cfg, _ := confstore.Load(j.loc.Path)
	asks, none := cfg.Agents[confstore.Find(cfg, "asks")], cfg.Agents[confstore.Find(cfg, "none")]
	if strings.Join(asks.Capabilities, ",") != "stats" || strings.Join(asks.SendTo, ",") != "admin" || len(none.Capabilities) != 0 {
		t.Errorf("asks = %+v\nnone = %+v", asks, none)
	}
}

// A denied request ends a waiting client, and the token never works.
func TestJoinDenied(t *testing.T) {
	j := startJoinBus(t)
	old := joinPoll
	joinPoll = 30 * time.Millisecond
	t.Cleanup(func() { joinPoll = old })
	file := filepath.Join(t.TempDir(), "intruder.token")

	done := make(chan error, 1)
	go func() {
		_, _, err := j.join(t, "intruder", file, "--capability", "summarize", "--wait", "10s")
		done <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := confstore.FindPending(j.loc, "intruder"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never arrived")
		}
	}
	if _, _, err := runCLI(t, "", "config", "join", "deny", "intruder"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "denied") {
			t.Errorf("waiting join ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("join --wait did not notice the denial")
	}
	if cfg, _ := confstore.Load(j.loc.Path); confstore.Find(cfg, "intruder") >= 0 {
		t.Error("a denied agent is in the config")
	}
	if j.connects(t, "intruder", file, 0) {
		t.Error("a denied agent connected")
	}
}

// --wait returns once the operator approves.
func TestJoinWaitApproved(t *testing.T) {
	j := startJoinBus(t)
	old := joinPoll
	joinPoll = 30 * time.Millisecond
	t.Cleanup(func() { joinPoll = old })
	file := filepath.Join(t.TempDir(), "w.token")
	type res struct {
		out string
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, _, err := j.join(t, "w", file, "--wait", "10s")
		done <- res{out, err}
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, ok := confstore.FindPending(j.loc, "w"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never arrived")
		}
	}
	if _, _, err := runCLI(t, "", "config", "join", "approve", "w"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || !strings.Contains(r.out, "AGENTBUS_AGENT=w") {
			t.Errorf("join --wait: %v %q", r.err, r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("join --wait did not notice the approval")
	}
}

// The endpoint takes requests from anyone, so it bounds them.
func TestJoinEndpointLimits(t *testing.T) {
	j := startJoinBus(t)
	post := func(path string, body any) (int, joinReply) {
		t.Helper()
		data, _ := json.Marshal(body)
		if s, ok := body.(string); ok {
			data = []byte(s)
		}
		resp, err := http.Post(j.joinURL+path, "application/json", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var reply joinReply
		_ = json.NewDecoder(resp.Body).Decode(&reply)
		return resp.StatusCode, reply
	}
	req := func(id string, caps ...string) confstore.JoinRequest {
		_, hash := broker.NewToken()
		return confstore.JoinRequest{ID: id, TokenSHA256: hash, Capabilities: caps}
	}

	for name, body := range map[string]any{
		"not json":     "nope",
		"wildcard":     req("w", "*"),
		"bad id":       req("../etc"),
		"no hash":      confstore.JoinRequest{ID: "w"},
		"huge":         `{"id":"w","description":"` + strings.Repeat("x", joinMaxBody) + `"}`,
		"observer ask": `{"id":"w","token_sha256":"zz","observer":true}`,
	} {
		if code, _ := post("/join", body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, code)
		}
	}
	// Asking how a request stands never creates one.
	if code, reply := post("/join/status", req("ghost")); code != http.StatusNotFound || reply.Status != joinUnknown {
		t.Errorf("status of an unknown request: %d %+v", code, reply)
	}
	if _, ok := confstore.FindPending(j.loc, "ghost"); ok {
		t.Error("/join/status created a request")
	}
	// What the client claims about where and when it asked is ignored.
	spoof := req("spoof")
	spoof.Remote, spoof.RequestedAt = "10.9.9.9", time.Now().Add(-48*time.Hour)
	if code, _ := post("/join", spoof); code != http.StatusAccepted {
		t.Fatalf("join: %d", code)
	}
	if p, ok := confstore.FindPending(j.loc, "spoof"); !ok || p.Remote != "127.0.0.1" || time.Since(p.RequestedAt) > time.Minute {
		t.Errorf("stored request = %+v (%v)", p, ok)
	}
	// One address may only have a few requests waiting.
	for i := 1; i < joinMaxPendingPerIP; i++ {
		if code, _ := post("/join", req(fmt.Sprintf("n%d", i))); code != http.StatusAccepted {
			t.Fatalf("request %d: status %d", i, code)
		}
	}
	if code, _ := post("/join", req("one-too-many")); code != http.StatusTooManyRequests {
		t.Errorf("over the per-address limit: status %d", code)
	}
	if reqs, _ := confstore.Pending(j.loc); len(reqs) != joinMaxPendingPerIP {
		t.Errorf("%d requests waiting, want %d", len(reqs), joinMaxPendingPerIP)
	}
}

func TestJoinListenNeedsTLSOffHost(t *testing.T) {
	_, hash := broker.NewToken()
	cfg := &broker.Config{Listen: "127.0.0.1:1883", JoinListen: "0.0.0.0:8884", Agents: []broker.AgentConfig{{ID: "a", TokenSHA256: hash}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "join_listen") {
		t.Errorf("a plaintext join_listen off loopback validated: %v", err)
	}
	cfg.JoinListen = "127.0.0.1:8884"
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}
