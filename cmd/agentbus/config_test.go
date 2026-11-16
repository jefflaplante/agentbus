package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jefflaplante/agentbus/broker"
	"github.com/jefflaplante/agentbus/internal/confstore"
)

func TestConfigCommands(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "config", "init"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "", "config", "path")
	if err != nil || !strings.Contains(out, "agentbus/broker.json") || !strings.Contains(out, "agentbus/tokens") {
		t.Fatalf("path: %v %q", err, out)
	}
	out, _, err = runCLI(t, "", "config", "agent", "add", "worker", "--capability", "stats", "--send-to", "admin")
	if err != nil || !strings.Contains(out, "AGENTBUS_AGENT=worker") || !strings.Contains(out, "worker.token") {
		t.Fatalf("add: %v %q", err, out)
	}
	if out, _, _ = runCLI(t, "", "config", "agent", "list"); !strings.Contains(out, "serves stats") || !strings.Contains(out, "admin") {
		t.Errorf("list:\n%s", out)
	}
	if _, _, err := runCLI(t, "", "config", "set", "rate_per_second", "50"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = runCLI(t, "", "config", "show"); !strings.Contains(out, `"rate_per_second": 50`) {
		t.Errorf("set not saved:\n%s", out)
	}
	if out, _, _ = runCLI(t, "", "config", "env", "worker", "--format", "go"); !strings.Contains(out, "client.Connect") {
		t.Errorf("env go:\n%s", out)
	}
	if out, _, _ = runCLI(t, "", "config", "env", "worker", "--format", "mqtt"); !strings.Contains(out, "client ID  worker") {
		t.Errorf("env mqtt:\n%s", out)
	}
	if _, _, err := runCLI(t, "", "config", "agent", "edit", "worker", "--capability=", "--send-to", "admin", "--observer"); err != nil {
		t.Fatal(err)
	}
	if out, _, _ = runCLI(t, "", "config", "agent", "list"); !strings.Contains(out, "observer") || strings.Contains(out, "serves stats") {
		t.Errorf("edit not applied:\n%s", out)
	}
	loc := confstore.Resolve("")
	before, _ := confstore.ReadToken(loc, "worker")
	if _, _, err := runCLI(t, "", "config", "agent", "token", "worker"); err != nil {
		t.Fatal(err)
	}
	if after, _ := confstore.ReadToken(loc, "worker"); after == before {
		t.Error("token not rotated")
	}
	if _, _, err := runCLI(t, "", "config", "agent", "rm", "worker"); err != nil {
		t.Fatal(err)
	}
	if _, ok := confstore.ReadToken(loc, "worker"); ok {
		t.Error("token file left after rm")
	}
	for _, args := range [][]string{{"config", "set", "nope", "1"}, {"config", "agent", "rm", "ghost"}, {"config", "init"}} {
		if _, _, err := runCLI(t, "", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestServeMissingExplicitConfig(t *testing.T) {
	isolate(t)
	_, _, err := runCLI(t, "", "serve", "--config", "/nonexistent/broker.json")
	if err == nil || !strings.Contains(err.Error(), "config init") {
		t.Fatalf("err = %v", err)
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// With a local config and its admin token, client commands need no flags.
func TestClientsDiscoverLocalBroker(t *testing.T) {
	isolate(t)
	loc := confstore.Resolve("")
	cfg, err := confstore.Bootstrap(loc)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = freePort(t)
	if err := confstore.Save(loc.Path, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, _ := broker.LoadConfig(loc.Path)
	srv, err := broker.New(loaded, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	out, stderr, err := runCLI(t, "", "status", "--plain")
	if err != nil {
		t.Fatalf("status with no flags: %v\n%s", err, stderr)
	}
	var st broker.Status
	if json.Unmarshal([]byte(out), &st) != nil || st.Agents[0].ID != "admin" || st.Agents[0].State != "online" {
		t.Errorf("status = %q", out)
	}
	if out, _, err := runCLI(t, "", "agents", "--plain"); err != nil || !strings.Contains(out, "admin") {
		t.Errorf("agents: %v %q", err, out)
	}
}

// serve with no config anywhere creates one and starts. It uses the real
// default listen address, so it's skipped if that port is taken.
func TestServeBootstrapsWithNoConfig(t *testing.T) {
	isolate(t)
	if l, err := net.Listen("tcp", confstore.DefaultListen); err != nil {
		t.Skipf("%s busy: %v", confstore.DefaultListen, err)
	} else {
		l.Close()
	}
	var out bytes.Buffer
	sw := &syncWriter{w: &out}
	a := &app{stdin: strings.NewReader(""), stdout: sw, stderr: sw}
	root := newRootCmd(a)
	root.SetArgs([]string{"serve"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sw.String(), "agentbus status") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	log := sw.String()
	for _, want := range []string{"No config found", "agentbus config", "admin"} {
		if !strings.Contains(log, want) {
			t.Errorf("startup output missing %q:\n%s", want, log)
		}
	}
	if !confstore.Resolve("").Exists() {
		t.Error("no config written")
	}
	if _, err := os.Stat(confstore.Resolve("").TokenFile("admin")); err != nil {
		t.Error("no admin token written")
	}
}

// A config change that fails to save leaves the token files alone.
func TestConfigAgentRmFailedSaveKeepsToken(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "config", "init"); err != nil {
		t.Fatal(err)
	}
	loc := confstore.Resolve("")
	before, _ := confstore.ReadToken(loc, "admin")
	if _, _, err := runCLI(t, "", "config", "agent", "rm", "admin"); err == nil {
		t.Fatal("removed the last agent")
	}
	if tok, ok := confstore.ReadToken(loc, "admin"); !ok || tok != before {
		t.Error("failed rm deleted admin.token")
	}
	cfg, _ := confstore.Load(loc.Path)
	if broker.HashToken(before) != cfg.Agents[0].TokenSHA256 {
		t.Error("token file and config out of step")
	}
}

func TestConfigSetSeveral(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "config", "init"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "config", "set", "tls_cert", "/x/cert.pem"); err == nil {
		t.Error("tls_cert without tls_key saved")
	}
	_, stderr, err := runCLI(t, "", "config", "set", "tls_cert", "/x/cert.pem", "tls_key", "/x/key.pem", "listen", "0.0.0.0:8883")
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := confstore.Load(confstore.Resolve("").Path)
	if cfg.TLSCert != "/x/cert.pem" || cfg.TLSKey != "/x/key.pem" || cfg.Listen != "0.0.0.0:8883" {
		t.Errorf("cfg = %+v", cfg)
	}
	if strings.Count(stderr, "✓") != 3 {
		t.Errorf("stderr = %q", stderr)
	}
	for _, args := range [][]string{{"config", "set"}, {"config", "set", "listen"}, {"config", "set", "listen", "127.0.0.1:1", "rate_burst"}} {
		if _, _, err := runCLI(t, "", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// A bad pair saves none of them.
	if _, _, err := runCLI(t, "", "config", "set", "rate_per_second", "5", "max_queued", "lots"); err == nil {
		t.Error("bad value accepted")
	}
	if cfg, _ := confstore.Load(confstore.Resolve("").Path); cfg.RatePerSecond == 5 {
		t.Error("partial set saved")
	}
}

func TestConfigErrorsExplain(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "config", "agent", "add", "w"); err == nil || !strings.Contains(err.Error(), "agentbus config init") {
		t.Errorf("add with no config: %v", err)
	}
	if _, _, err := runCLI(t, "", "config", "init"); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, "", "config", "set", "data_dir", "123")
	if err == nil || !strings.Contains(err.Error(), "a string") || strings.Contains(err.Error(), "json") {
		t.Errorf("data_dir 123: %v", err)
	}
}

// A relative --config gives absolute data_dir and token paths, and
// config path shows the config's own data_dir and audit_file.
func TestRelativeConfigPath(t *testing.T) {
	isolate(t)
	t.Chdir(t.TempDir())
	wd, _ := os.Getwd()
	if _, _, err := runCLI(t, "", "--config", "rel/broker.json", "config", "init"); err != nil {
		t.Fatal(err)
	}
	cfg, err := confstore.Load(filepath.Join(wd, "rel", "broker.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(wd, "rel", "state", "data"); cfg.DataDir != want {
		t.Errorf("data_dir %q, want %q", cfg.DataDir, want)
	}
	t.Setenv("AGENTBUS_CONFIG", "rel/broker.json")
	if _, _, err := runCLI(t, "", "config", "set", "data_dir", "/srv/agentbus"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "", "config", "path")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(wd, "rel", "broker.json"), filepath.Join(wd, "rel", "tokens"), "data    /srv/agentbus",
		"audit   " + filepath.Join(wd, "rel", "state", "audit.jsonl")} {
		if !strings.Contains(out, want) {
			t.Errorf("config path missing %q:\n%s", want, out)
		}
	}
}

func TestServeWatchFlag(t *testing.T) {
	isolate(t)
	if _, _, err := runCLI(t, "", "serve", "--watch", "-1s"); err == nil || !strings.Contains(err.Error(), "--watch") {
		t.Errorf("negative --watch: %v", err)
	}
	loc := confstore.Resolve("")
	cfg, err := confstore.Bootstrap(loc)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = freePort(t)
	if err := confstore.Save(loc.Path, cfg); err != nil {
		t.Fatal(err)
	}
	sw := &syncWriter{w: &bytes.Buffer{}}
	a := &app{stdin: strings.NewReader(""), stdout: sw, stderr: sw}
	root := newRootCmd(a)
	root.SetArgs([]string{"serve", "--watch", "0"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sw.String(), "SIGHUP") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // a panicking watcher would have fired by now
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sw.String(), "watching off") {
		t.Errorf("log:\n%s", sw.String())
	}
}

func TestLoopbackListen(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:1883": true, "localhost:1883": true, "[::1]:1883": true, "::1": true, "127.5.5.5:1": true,
		"0.0.0.0:1883": false, ":1883": false, "[::]:1883": false, "10.0.0.1:1883": false, "127.example.com:1": false,
	} {
		if got := loopbackListen(listen); got != want {
			t.Errorf("%s: %v", listen, got)
		}
	}
}
