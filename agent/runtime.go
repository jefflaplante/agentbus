package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// Config is everything Run needs besides the manifest.
type Config struct {
	Manifest *Manifest
	// Files holds the scripts the manifest names. Dir, if set, is the same
	// directory on disk and scripts run from it in place; otherwise Files
	// (for example an embed.FS) is copied to a private temp directory.
	Files fs.FS
	Dir   string

	URL, AgentID, Token string
	Logger              *slog.Logger
	// Ready, if set, is called once the agent is connected and serving.
	Ready func(*client.Client)
	// SkipMissingGo drops handlers whose Go function isn't in this binary,
	// with a warning, instead of refusing to start. agentbus agent run
	// uses it: Go handlers only exist in a built agent.
	SkipMissingGo bool
}

// Run connects as the agent and serves its handlers until ctx ends, then
// stops taking new work, waits up to the manifest's drain time for running
// handlers, and disconnects.
func Run(ctx context.Context, cfg Config) error {
	m, log := cfg.Manifest, cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.AgentID == "" || cfg.Token == "" || cfg.URL == "" {
		return errors.New("agent ID, token and broker URL are all required")
	}
	if cfg.SkipMissingGo {
		kept := *m
		kept.Handlers = nil
		for _, h := range m.Handlers {
			if _, ok := lookup(h.Go); h.Go != "" && !ok {
				log.Warn("skipping Go handler: it only exists in a built agent (agentbus agent build)", "handler", h.Name, "go", h.Go)
				continue
			}
			kept.Handlers = append(kept.Handlers, h)
		}
		if len(kept.Handlers) == 0 {
			return errors.New("no handlers left to run: every handler is a Go function; build the agent")
		}
		m = &kept
	}
	if problems := Check(m, cfg.Files, false); len(problems) > 0 {
		return fmt.Errorf("manifest problems:\n  %s", strings.Join(problems, "\n  "))
	}

	dir := cfg.Dir
	if dir == "" && usesScripts(m) {
		tmp, err := extract(cfg.Files)
		if err != nil {
			return fmt.Errorf("unpack scripts: %w", err)
		}
		defer os.RemoveAll(tmp)
		dir = tmp
	}

	rt := &runtime{m: m, log: log, self: cfg.AgentID}
	tasks := map[string]client.Handler{}
	var inbox []routed
	type watch struct {
		filter string
		h      client.Handler
		name   string
	}
	var watches []watch
	for _, h := range m.Handlers {
		fn, err := rt.action(h, dir)
		if err != nil {
			return err
		}
		fn = rt.guard(h, fn)
		switch {
		case h.Task != "":
			tasks[h.Task] = fn
		case h.Inbox:
			inbox = append(inbox, routed{op: h.Op, name: h.Name, h: fn})
		case h.Event != "":
			watches = append(watches, watch{h.Event, fn, h.Name})
		}
	}

	host, _ := os.Hostname()
	opts := client.Options{
		BrokerURL: cfg.URL, AgentID: cfg.AgentID, Token: cfg.Token,
		Description:   m.PresenceDescription(),
		Meta:          map[string]string{"runtime": "agentbus-agent", "host": host},
		Tasks:         tasks, // registered before connecting: queued tasks arrive at once
		MaxConcurrent: m.MaxConcurrent,
		Logger:        log,
	}
	if len(inbox) > 0 {
		opts.Inbox = rt.inbox(inbox)
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := client.Connect(cctx, opts)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to %s as %s: %w", cfg.URL, cfg.AgentID, err)
	}
	for _, w := range watches {
		h, name := w.h, w.name
		err := c.WatchEvents(ctx, w.filter, func(e *a2a.Envelope) {
			if _, err := h(ctx, e); err != nil {
				log.Warn("event handler failed", "handler", name, "from", e.From, "id", e.ID, "err", err)
			}
		})
		if err != nil {
			c.Close()
			return fmt.Errorf("watch %s: %w", w.filter, err)
		}
	}
	log.Info("agent serving", "agent", cfg.AgentID, "broker", cfg.URL, "tasks", m.Capabilities(),
		"inbox_ops", inboxOps(inbox), "events", len(watches))
	if cfg.Ready != nil {
		cfg.Ready(c)
	}

	<-ctx.Done()
	rt.closing.Store(true) // new requests get busy from here on
	dctx, dcancel := context.WithTimeout(context.Background(), time.Duration(m.Drain))
	if err := c.Drain(dctx); err != nil {
		log.Warn("drain timed out; stopping with handlers still running", "drain", time.Duration(m.Drain))
	}
	dcancel()
	c.Close()
	log.Info("agent stopped", "agent", cfg.AgentID)
	return nil
}

type runtime struct {
	m       *Manifest
	log     *slog.Logger
	self    string
	closing atomic.Bool
}

type routed struct {
	op, name string
	h        client.Handler
}

// action builds the handler function for h's run, script or go.
func (rt *runtime) action(h Handler, dir string) (client.Handler, error) {
	if h.Go != "" {
		fn, ok := lookup(h.Go)
		if !ok {
			return nil, fmt.Errorf("handler %s: no Go handler %q in this binary (registered: %s)",
				h.Name, h.Go, strings.Join(Registered(), ", "))
		}
		return fn, nil
	}
	var argv []string
	if h.Run != "" {
		argv = []string{"sh", "-c", h.Run}
	} else {
		argv = append(interpreterFor(h), filepath.Join(dir, filepath.FromSlash(path.Clean(h.Script))))
	}
	env := []string{"AGENTBUS_SELF=" + rt.self, "AGENTBUS_HANDLER=" + h.Name}
	merged := maps.Clone(rt.m.Env)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, h.Env)
	for _, k := range slices.Sorted(maps.Keys(merged)) {
		env = append(env, k+"="+merged[k])
	}
	workdir := h.Workdir
	if workdir == "" {
		workdir = rt.m.Workdir
	}
	return CommandHandler(argv, CommandOptions{Dir: workdir, Env: env, Timeout: rt.m.timeout(h), Log: rt.log}), nil
}

// guard wraps a handler with the sender allow-list, drain tracking, a
// timeout for Go handlers, and a log line per run.
func (rt *runtime) guard(h Handler, fn client.Handler) client.Handler {
	allow := rt.m.allowFrom(h)
	timeout := rt.m.timeout(h)
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		if len(allow) > 0 && !slices.Contains(allow, e.From) {
			rt.log.Warn("sender not allowed", "handler", h.Name, "from", e.From, "id", e.ID)
			return nil, client.Errorf("forbidden", "%s may not use %s", e.From, h.Name)
		}
		if rt.closing.Load() {
			return nil, client.Errorf("busy", "agent is shutting down; retry later")
		}
		if h.Go != "" && timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		start := time.Now()
		out, err := fn(ctx, e)
		rt.log.Info("handled", "handler", h.Name, "from", e.From, "id", e.ID, "type", e.Type,
			"ms", time.Since(start).Milliseconds(), "ok", err == nil)
		return out, err
	}
}

// inbox routes inbox messages by body.op. A handler without an op takes
// everything else; with none, an unknown op is refused with the list of
// ops this agent handles.
func (rt *runtime) inbox(handlers []routed) client.Handler {
	byOp := map[string]client.Handler{}
	var fallback client.Handler
	for _, r := range handlers {
		if r.op == "" {
			fallback = r.h
		} else {
			byOp[r.op] = r.h
		}
	}
	ops := inboxOps(handlers)
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		switch e.Type {
		case a2a.TypeRequest, a2a.TypeEvent:
		case a2a.TypeError:
			rt.log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
			return nil, nil
		default:
			return nil, nil // a late reply to something this agent asked
		}
		if h, ok := byOp[opOf(e)]; ok {
			return h(ctx, e)
		}
		if fallback != nil {
			return fallback(ctx, e)
		}
		if e.Type == a2a.TypeEvent {
			return nil, nil
		}
		return nil, client.Errorf("unknown_op", `this agent handles body.op: %s`, strings.Join(ops, ", "))
	}
}

func inboxOps(rs []routed) []string {
	var ops []string
	for _, r := range rs {
		if r.op != "" {
			ops = append(ops, r.op)
		}
	}
	slices.Sort(ops)
	return ops
}

func usesScripts(m *Manifest) bool {
	for _, h := range m.Handlers {
		if h.Script != "" {
			return true
		}
	}
	return false
}

// extract copies fsys into a new private directory so scripts can be
// executed, keeping its layout so scripts can find their siblings.
func extract(fsys fs.FS) (string, error) {
	dir, err := os.MkdirTemp("", "agentbus-agent-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o700)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// Check reports problems that would stop the manifest from running here:
// Go handlers missing from this binary, scripts missing from files, and
// (when host is true) interpreters or commands missing from PATH.
func Check(m *Manifest, files fs.FS, host bool) []string {
	var out []string
	for _, h := range m.Handlers {
		switch {
		case h.Go != "":
			if _, ok := lookup(h.Go); !ok {
				reg := strings.Join(Registered(), ", ")
				if reg == "" {
					reg = "none"
				}
				out = append(out, fmt.Sprintf("handler %s: Go handler %q isn't in this binary (registered: %s); "+
					"Go handlers need `agentbus agent build`", h.Name, h.Go, reg))
			}
		case h.Script != "":
			if files == nil {
				out = append(out, fmt.Sprintf("handler %s: no files to find %s in", h.Name, h.Script))
			} else if _, err := fs.Stat(files, path.Clean(h.Script)); err != nil {
				out = append(out, fmt.Sprintf("handler %s: script %s not found", h.Name, h.Script))
			}
			if interp := interpreterFor(h); host && len(interp) > 0 {
				if _, err := exec.LookPath(interp[0]); err != nil {
					out = append(out, fmt.Sprintf("handler %s: interpreter %s not found on this host", h.Name, interp[0]))
				}
			}
		case h.Run != "" && host:
			if _, err := exec.LookPath("sh"); err != nil {
				out = append(out, fmt.Sprintf("handler %s: sh not found on this host", h.Name))
			}
		}
	}
	return out
}
