package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
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
