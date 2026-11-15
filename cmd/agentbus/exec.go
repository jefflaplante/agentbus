package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/jefflaplante/mqtt/a2a"
	"github.com/jefflaplante/mqtt/client"
)

// maxExecOutput caps what a handler command may write to stdout or stderr.
const maxExecOutput = 1 << 20

// execHandler runs command once per envelope of the given types; see usage.
func execHandler(command string, timeout time.Duration, types map[a2a.Type]bool, log *slog.Logger) client.Handler {
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		if !types[e.Type] {
			if e.Type == a2a.TypeError {
				log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID, "body", string(e.Body))
			}
			return nil, nil
		}
		in, err := e.Marshal()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = scrubEnv(os.Environ())
		// Own process group, so a timeout kills the command's children too.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 2 * time.Second
		stdout := &capBuffer{max: maxExecOutput}
		stderr := &capBuffer{max: maxExecOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		runErr := cmd.Run()
		if stdout.over || stderr.over {
			log.Warn("handler output too large", "id", e.ID, "max", maxExecOutput)
			return nil, errors.New("handler output too large")
		}
		if runErr != nil {
			// The details stay local: stderr can hold paths, secrets or
			// stack traces that the requesting agent shouldn't see.
			log.Warn("handler command failed", "id", e.ID, "from", e.From, "err", runErr,
				"stderr", strings.TrimSpace(stderr.String()))
			if ctx.Err() != nil {
				return nil, errors.New("handler timed out")
			}
			return nil, errors.New("handler failed")
		}
		outb := bytes.TrimSpace(stdout.Bytes())
		if len(outb) == 0 {
			return nil, nil
		}
		if json.Valid(outb) {
			return json.RawMessage(outb), nil
		}
		return string(outb), nil
	}
}

// scrubEnv drops agentbus credentials from a child's environment.
func scrubEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTBUS_TOKEN=") || strings.HasPrefix(kv, "AGENTBUS_TOKEN_FILE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// capBuffer collects output up to max bytes. It wraps bytes.Buffer
// instead of embedding it: an embedded Buffer's ReadFrom would let
// io.Copy bypass the cap in Write.
type capBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil // keep draining so the child doesn't block
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *capBuffer) String() string { return b.buf.String() }
