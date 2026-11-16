package agent

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
)

// Settings are the connection settings for one host, resolved from flags,
// the environment and the manifest.
type Settings struct {
	URL, AgentID, Token string
	TokenSource         string // where the token came from, for messages
}

// Resolve picks connection settings: explicit values first, then
// AGENTBUS_URL / AGENTBUS_AGENT / AGENTBUS_TOKEN / AGENTBUS_TOKEN_FILE,
// then the manifest's broker and token_file, then the standard token
// locations: $AGENTBUS_CONFIG_DIR/tokens/<agent>.token when that's set,
// /etc/agentbus/<agent>.token and ~/.config/agentbus/tokens/<agent>.token.
func Resolve(m *Manifest, url, agentID, tokenFile string) (Settings, error) {
	s := Settings{URL: first(url, os.Getenv("AGENTBUS_URL"), m.Broker, "tcp://127.0.0.1:1883"),
		AgentID: first(agentID, os.Getenv("AGENTBUS_AGENT"), m.Agent)}
	if s.AgentID == "" {
		return s, errors.New("no agent ID: set agent in the manifest, AGENTBUS_AGENT, or -agent")
	}
	if tokenFile == "" {
		if tok := os.Getenv("AGENTBUS_TOKEN"); tok != "" {
			s.Token, s.TokenSource = strings.TrimSpace(tok), "AGENTBUS_TOKEN"
			return s, nil
		}
	}
	candidates := []string{tokenFile, os.Getenv("AGENTBUS_TOKEN_FILE"), m.TokenFile}
	explicit := first(candidates...) != ""
	if !explicit {
		candidates = nil
		if d := os.Getenv("AGENTBUS_CONFIG_DIR"); d != "" {
			candidates = append(candidates, filepath.Join(d, "tokens", s.AgentID+".token"))
		}
		candidates = append(candidates, "/etc/agentbus/"+s.AgentID+".token")
		if home, err := os.UserHomeDir(); err == nil {
			candidates = append(candidates, filepath.Join(home, ".config", "agentbus", "tokens", s.AgentID+".token"))
		}
	}
	for _, f := range candidates {
		if f == "" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			if explicit {
				return s, fmt.Errorf("read token file: %w", err)
			}
			continue
		}
		s.Token, s.TokenSource = strings.TrimSpace(string(data)), f
		return s, nil
	}
	return s, fmt.Errorf("no token for %s: put it in /etc/agentbus/%s.token (mode 0600), or set AGENTBUS_TOKEN_FILE "+
		"or -token-file. Create it on the broker host with: agentbus config agent add %s", s.AgentID, s.AgentID, s.AgentID)
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Main is the entry point of a generated agent binary: files holds the
// embedded manifest (at name) and scripts. It parses flags, resolves the
// connection, and runs until SIGINT or SIGTERM.
func Main(files fs.FS, name string) {
	os.Exit(main(files, name, os.Args[1:]))
}

func main(files fs.FS, name string, args []string) int {
	fl := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	url := fl.String("url", "", "broker URL (env AGENTBUS_URL; default: the manifest's broker)")
	agentID := fl.String("agent", "", "agent ID (env AGENTBUS_AGENT; default: the manifest's)")
	tokenFile := fl.String("token-file", "", "file holding the agent's token (env AGENTBUS_TOKEN_FILE)")
	manifestPath := fl.String("manifest", "", "use this manifest file instead of the embedded one (scripts still come from the binary)")
	check := fl.Bool("check", false, "check the manifest and this host, print the result, and exit")
	show := fl.Bool("print-manifest", false, "print the embedded manifest and exit")
	version := fl.Bool("version", false, "print the version and exit")
	logFormat := fl.String("log-format", "text", "log format: text or json")
	verbose := fl.Bool("v", false, "log every handled message")
	if err := fl.Parse(args); err != nil {
		return 2
	}

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelInfo
	}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	if *logFormat == "json" {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	}
	log := slog.New(h)

	if *version {
		fmt.Println(Version())
		return 0
	}
	raw, err := fs.ReadFile(files, name)
	if *manifestPath != "" {
		raw, err = os.ReadFile(*manifestPath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "read manifest:", err)
		return 1
	}
	if *show {
		os.Stdout.Write(raw)
		return 0
	}
	m, err := ParseManifest(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *check {
		problems := Check(m, files, true)
		st, rerr := Resolve(m, *url, *agentID, *tokenFile)
		if rerr != nil {
			problems = append(problems, rerr.Error())
		}
		fmt.Printf("agent    %s\nbroker   %s\ntoken    %s\ntasks    %s\nhandlers %d\ngo funcs %s\n",
			st.AgentID, st.URL, first(st.TokenSource, "not found"), strings.Join(m.Capabilities(), ", "),
			len(m.Handlers), strings.Join(Registered(), ", "))
		if len(problems) > 0 {
			fmt.Println("problems:\n  " + strings.Join(problems, "\n  "))
			return 1
		}
		fmt.Println("ok")
		return 0
	}
	st, err := Resolve(m, *url, *agentID, *tokenFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := Run(ctx, Config{Manifest: m, Files: files, URL: st.URL, AgentID: st.AgentID, Token: st.Token, Logger: log}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// Version is the agent binary's module version and VCS revision, if known.
func Version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := bi.Main.Path + " " + bi.Main.Version
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			v += " (" + s.Value[:7] + ")"
		}
	}
	for _, d := range bi.Deps {
		if d.Path == "github.com/jefflaplante/agentbus" {
			v += ", agentbus " + d.Version
		}
	}
	return v
}
