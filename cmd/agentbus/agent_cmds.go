package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"text/template"

	"github.com/spf13/cobra"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/agent"
	"github.com/jefflaplante/agentbus/client"
	"github.com/jefflaplante/agentbus/internal/tui"
)

//go:embed all:agenttmpl
var agentTemplates embed.FS

const agentbusModule = "github.com/jefflaplante/agentbus"

func newAgentCmd(a *app) *cobra.Command {
	c := &cobra.Command{
		Use:   "agent",
		Short: "Build and run standalone agents that serve topics with shell, Python or Go handlers",
		Long: `An agent is a small, single-purpose program for one host. It connects as
one agent ID and serves the topics its manifest (agent.yaml) lists, each
with a shell command, a script (sh, bash, python3, ...) or a Go function.

  agentbus agent init disk-agent       scaffold a project with an example of each
  agentbus agent run -f agent.yaml     run a manifest from disk, no build needed
  agentbus agent build                 compile the project to one static binary per
                                       target, with the manifest and scripts inside
  agentbus agent check                 check a manifest against this host

See docs/AGENTS.md.`,
	}
	c.AddCommand(newAgentInitCmd(), newAgentRunCmd(a), newAgentCheckCmd(a), newAgentBuildCmd(a))
	return c
}

func loadManifestFile(file string) (*agent.Manifest, string, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Dir(abs)
	m, err := agent.LoadManifest(os.DirFS(dir), filepath.Base(abs))
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", file, err)
	}
	return m, dir, nil
}

// useManifestConnection fills unset connection settings from a manifest,
// before the local broker config is consulted.
func (a *app) useManifestConnection(m *agent.Manifest) {
	if a.agent == "" {
		a.agent = m.Agent
	}
	if a.url == "" && m.Broker != "" {
		a.url = m.Broker
	}
	if a.token == "" && a.tokenFile == "" && m.TokenFile != "" {
		a.tokenFile = m.TokenFile
	}
}

func newAgentRunCmd(a *app) *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "run",
		Short: "Run an agent manifest from disk",
		Long: `Run the manifest's handlers as the agent it names. Scripts run in place,
from the manifest's directory. Go handlers need a built binary (agent
build); only those compiled into this agentbus binary are available here.

The connection comes from the flags and AGENTBUS_* variables, then the
manifest's broker and token_file, then the local broker config, the same
as the other client commands. SIGINT or SIGTERM stops taking new work and
waits for running handlers (the manifest's drain, default 30s).`,
		Example: `  agentbus agent run -f agent.yaml
  agentbus agent run -f agent.yaml --url ssl://bus.example.com:8883 --token-file /etc/agentbus/disk-agent.token`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, dir, err := loadManifestFile(file)
			if err != nil {
				return err
			}
			a.useManifestConnection(m)
			token, err := a.credentials()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return agent.Run(ctx, agent.Config{
				Manifest: m, Files: os.DirFS(dir), Dir: dir,
				URL: a.url, AgentID: a.agent, Token: token, Logger: a.logger(false), SkipMissingGo: true,
				Ready: func(*client.Client) {
					fmt.Fprintf(a.stderr, "%s serving as %s on %s; ctrl+c to stop\n",
						tui.OKStyle.Render("●"), a.agent, a.url)
				},
			})
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "agent.yaml", "manifest file")
	return c
}

func newAgentCheckCmd(a *app) *cobra.Command {
	var file string
	c := &cobra.Command{
		Use:   "check",
		Short: "Check an agent manifest and its scripts against this host",
		Long: `Validate the manifest, check that its scripts exist and their
interpreters are installed on this host, and list Go handlers, which only
a built binary can check (run it with -check on the target host).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, dir, err := loadManifestFile(file)
			if err != nil {
				return err
			}
			var problems, goFuncs []string
			for _, p := range agent.Check(m, os.DirFS(dir), true) {
				if strings.Contains(p, "isn't in this binary") {
					continue // registered in the built binary, not this one
				}
				problems = append(problems, p)
			}
			for _, h := range m.Handlers {
				if h.Go != "" {
					goFuncs = append(goFuncs, h.Go)
				}
			}
			out := a.stdout
			fmt.Fprintf(out, "agent     %s\ntasks     %s\nhandlers  %d\n", m.Agent, strings.Join(m.Capabilities(), ", "), len(m.Handlers))
			if len(goFuncs) > 0 {
				fmt.Fprintf(out, "go funcs  %s (checked by the built binary's -check)\n", strings.Join(goFuncs, ", "))
			}
			if len(problems) > 0 {
				fmt.Fprintln(out, "problems:\n  "+strings.Join(problems, "\n  "))
				return errors.New("manifest has problems")
			}
			fmt.Fprintln(out, tui.OKStyle.Render("ok"))
			return nil
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "agent.yaml", "manifest file")
	return c
}

// templateData fills the project templates.
type templateData struct {
	Name, Module, Description string
	GoVersion                 string
	Require, Replace          string
}

var moduleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~/-]*$`)

func newAgentInitCmd() *cobra.Command {
	var dir, module, description, src string
	c := &cobra.Command{
		Use:   "init NAME",
		Short: "Create an agent project with an example of each handler kind",
		Long: `Create a project directory for an agent named NAME (also its agent ID):

  agent.yaml           the manifest: what it serves and which handler runs for each
  handlers/handlers.go a Go handler, compiled in
  scripts/             a Python and a shell handler, embedded
  main.go, go.mod      the program; agent build compiles it
  deploy/NAME.service  a hardened systemd unit
  README.md

The project depends on the agentbus module. --agentbus-src points it at a
local checkout instead (a replace directive), for building offline or
against unreleased changes.`,
		Example: `  agentbus agent init disk-agent
  agentbus agent init gpu-probe --description "GPU health on render nodes" --dir ~/src/gpu-probe`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !a2a.ValidName(name) {
				return fmt.Errorf("%q isn't a valid agent ID: 1-64 chars of a-z 0-9 . _ -, starting with a letter or digit", name)
			}
			if dir == "" {
				dir = name
			}
			if module == "" {
				module = name
			}
			if !moduleRE.MatchString(module) {
				return fmt.Errorf("%q isn't a valid Go module path", module)
			}
			if description == "" {
				description = name + " agent"
			}
			if src == "" {
				src = os.Getenv("AGENTBUS_SRC")
			}
			data := templateData{Name: name, Module: module, Description: description, GoVersion: goVersion()}
			switch {
			case src != "":
				abs, err := filepath.Abs(src)
				if err != nil {
					return err
				}
				if _, err := os.Stat(filepath.Join(abs, "agent", "runtime.go")); err != nil {
					return fmt.Errorf("--agentbus-src %s doesn't look like an agentbus checkout", src)
				}
				data.Require, data.Replace = "v0.0.0-00010101000000-000000000000", abs
			default:
				data.Require = moduleVersion()
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
				return fmt.Errorf("%s already exists and isn't empty", dir)
			}
			files, err := renderProject(dir, data)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s created %s:\n", tui.OKStyle.Render("✓"), dir)
			for _, f := range files {
				fmt.Fprintln(out, "  "+f)
			}
			fmt.Fprintf(out, "\nNext:\n  cd %s\n  agentbus config agent add %s --capability %s.wordcount --capability %s.disk\n"+
				"  agentbus agent run            # try it here; Go handlers need a build\n"+
				"  agentbus agent build --target linux/amd64\n", dir, name, name, name)
			if data.Require == "" {
				fmt.Fprintln(out, "\nThis agentbus binary has no release version, so go.mod doesn't pin one: "+
					"agent build will use the latest agentbus module. Use --agentbus-src to build against a checkout.")
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "project directory (default: ./NAME)")
	c.Flags().StringVar(&module, "module", "", "Go module path for the project (default: NAME)")
	c.Flags().StringVar(&description, "description", "", "one line for the agent's presence card")
	c.Flags().StringVar(&src, "agentbus-src", "", "build against this agentbus checkout (env AGENTBUS_SRC)")
	return c
}

// renderProject writes the templates to dir and returns the files written.
func renderProject(dir string, data templateData) ([]string, error) {
	var written []string
	err := fs.WalkDir(agentTemplates, "agenttmpl", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "agenttmpl/"), ".tmpl")
		switch rel {
		case "gitignore":
			rel = ".gitignore"
		case "deploy/agent.service":
			rel = "deploy/" + data.Name + ".service"
		}
		raw, err := agentTemplates.ReadFile(p)
		if err != nil {
			return err
		}
		t, err := template.New(rel).Option("missingkey=error").Parse(string(raw))
		if err != nil {
			return fmt.Errorf("template %s: %w", p, err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return fmt.Errorf("template %s: %w", p, err)
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if path.Dir(rel) == "scripts" {
			mode = 0o755
		}
		if err := os.WriteFile(dst, buf.Bytes(), mode); err != nil {
			return err
		}
		written = append(written, rel)
		return nil
	})
	return written, err
}

func goVersion() string {
	v := strings.TrimPrefix(runtime.Version(), "go")
	if i := strings.IndexAny(v, " -"); i >= 0 {
		v = v[:i]
	}
	return v
}

// moduleVersion is the agentbus version this binary was built from, if it
// is a real release or pseudo-version; "" for a local build.
func moduleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(bi.Main.Version, "v") {
		return bi.Main.Version
	}
	return ""
}

func newAgentBuildCmd(a *app) *cobra.Command {
	var targets []string
	var outDir string
	c := &cobra.Command{
		Use:   "build [DIR]",
		Short: "Compile an agent project into standalone binaries",
		Long: `Compile the project in DIR (default .) into one static binary per target
(CGO off, trimmed, stripped), with agent.yaml and scripts/ embedded and the
Go handlers compiled in. Binaries go to DIR/dist as NAME-OS-ARCH. Needs the
Go toolchain.

The binary built for this machine, if any, is run with -check to confirm
every Go handler the manifest names is registered. Tokens are never built
in: each host supplies its own (AGENTBUS_TOKEN_FILE, -token-file, or
/etc/agentbus/NAME.token).`,
		Example: `  agentbus agent build
  agentbus agent build ./disk-agent --target linux/amd64,linux/arm64,darwin/arm64`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return buildAgent(cmd, a, dir, targets, outDir)
		},
	}
	c.Flags().StringSliceVar(&targets, "target", []string{runtime.GOOS + "/" + runtime.GOARCH}, "OS/ARCH to build for (repeatable or comma-separated)")
	c.Flags().StringVarP(&outDir, "out", "o", "", "output directory (default DIR/dist)")
	return c
}

func buildAgent(cmd *cobra.Command, a *app, dir string, targets []string, outDir string) error {
	gobin, err := exec.LookPath("go")
	if err != nil {
		return errors.New("agent build needs the Go toolchain on PATH (https://go.dev/dl)")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	m, err := agent.LoadManifest(os.DirFS(dir), "agent.yaml")
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(dir, "agent.yaml"), err)
	}
	for _, h := range m.Handlers {
		if h.Script == "" {
			continue
		}
		if path.Dir(path.Clean(h.Script)) != "scripts" && !strings.HasPrefix(path.Clean(h.Script), "scripts/") {
			return fmt.Errorf("handler %s: script %s must be under scripts/ to be embedded", h.Name, h.Script)
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(h.Script))); err != nil {
			return fmt.Errorf("handler %s: script %s not found", h.Name, h.Script)
		}
	}
	name := m.Agent
	if name == "" {
		name = filepath.Base(dir)
	}
	if outDir == "" {
		outDir = filepath.Join(dir, "dist")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	errOut := cmd.ErrOrStderr()
	goCmd := func(env []string, args ...string) error {
		c := exec.CommandContext(cmd.Context(), gobin, args...)
		c.Dir = dir
		c.Env = append(os.Environ(), append([]string{"GOWORK=off", "GOFLAGS=-mod=mod"}, env...)...)
		c.Stdout, c.Stderr = errOut, errOut
		return c.Run()
	}
	if err := goCmd(nil, "mod", "tidy"); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	var built []string
	for _, t := range targets {
		goos, goarch, ok := strings.Cut(t, "/")
		if !ok || goos == "" || goarch == "" {
			return fmt.Errorf("target %q: want OS/ARCH, e.g. linux/amd64", t)
		}
		out := filepath.Join(outDir, fmt.Sprintf("%s-%s-%s", name, goos, goarch))
		if goos == "windows" {
			out += ".exe"
		}
		fmt.Fprintf(errOut, "building %s\n", filepath.Base(out))
		if err := goCmd([]string{"CGO_ENABLED=0", "GOOS=" + goos, "GOARCH=" + goarch},
			"build", "-trimpath", "-ldflags=-s -w", "-o", out, "."); err != nil {
			return fmt.Errorf("go build %s: %w", t, err)
		}
		built = append(built, out)
		if goos == runtime.GOOS && goarch == runtime.GOARCH {
			if err := selfCheck(cmd, out); err != nil {
				return err
			}
		}
	}
	w := a.stdout
	for _, b := range built {
		sum, size, err := fileSum(b)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%s  %s  %.1f MiB  sha256 %s\n", tui.OKStyle.Render("✓"), b, float64(size)/(1<<20), sum)
	}
	return nil
}

// selfCheck runs a freshly built binary with -check. A dummy token stands
// in so the check covers the manifest and Go handlers, not this machine's
// credentials.
func selfCheck(cmd *cobra.Command, bin string) error {
	c := exec.CommandContext(cmd.Context(), bin, "-check")
	c.Env = append(agent.ScrubEnv(os.Environ()), "AGENTBUS_TOKEN=build-check")
	out, err := c.CombinedOutput()
	// Only a missing Go handler is a build problem. Interpreters and
	// commands are checked on the target host, where they have to exist.
	if err != nil && bytes.Contains(out, []byte("isn't in this binary")) {
		return fmt.Errorf("%s -check failed:\n%s", filepath.Base(bin), out)
	}
	return nil
}

func fileSum(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
