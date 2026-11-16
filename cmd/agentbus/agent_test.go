package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jefflaplante/agentbus/agent"
)

func TestAgentInitRendersAProject(t *testing.T) {
	isolate(t)
	dir := filepath.Join(t.TempDir(), "probe")
	src, _ := filepath.Abs("../..")
	out, _, err := runCLI(t, "", "agent", "init", "probe", "--dir", dir, "--agentbus-src", src, "--description", `GPU "health" probe`)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"agent.yaml", "main.go", "go.mod", "handlers/handlers.go", "scripts/wordcount.py",
		"scripts/disk.sh", "deploy/probe.service", "README.md", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written", f)
		}
	}
	if !strings.Contains(out, "agentbus agent build") {
		t.Errorf("next steps missing:\n%s", out)
	}
	m, err := agent.LoadManifest(os.DirFS(dir), "agent.yaml")
	if err != nil {
		t.Fatalf("generated manifest: %v", err)
	}
	if m.Agent != "probe" || m.Description != `GPU "health" probe` || len(m.Handlers) != 4 {
		t.Errorf("manifest = %+v", m)
	}
	gomod, _ := os.ReadFile(filepath.Join(dir, "go.mod"))
	if !strings.Contains(string(gomod), "replace github.com/jefflaplante/agentbus => "+src) {
		t.Errorf("go.mod:\n%s", gomod)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "scripts", "disk.sh")); fi.Mode().Perm()&0o100 == 0 {
		t.Error("scripts aren't executable")
	}
	if out, _, err := runCLI(t, "", "agent", "check", "-f", filepath.Join(dir, "agent.yaml")); err != nil &&
		!strings.Contains(out, "interpreter") {
		t.Errorf("check: %v\n%s", err, out)
	}

	if _, _, err := runCLI(t, "", "agent", "init", "probe", "--dir", dir); err == nil {
		t.Error("init overwrote a non-empty directory")
	}
	if _, _, err := runCLI(t, "", "agent", "init", "Bad Name", "--dir", t.TempDir()); err == nil {
		t.Error("invalid agent ID accepted")
	}
}

func TestAgentCheckReportsProblems(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	manifest := "agent: probe\nhandlers:\n  - {task: probe.x, script: scripts/missing.sh}\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "", "agent", "check", "-f", filepath.Join(dir, "agent.yaml"))
	if err == nil || !strings.Contains(out, "scripts/missing.sh not found") {
		t.Errorf("check: %v\n%s", err, out)
	}
}
