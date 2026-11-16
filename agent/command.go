package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/client"
)

// MaxOutput caps what a handler command may write to stdout or stderr.
const MaxOutput = 1 << 20

// ExitReplyError is the exit status with which a command chooses its error
// reply: stdout then holds {"code": "...", "reason": "..."}.
const ExitReplyError = 3

// maxBodyEnv is the largest body passed in AGENTBUS_BODY. Larger bodies
// are only on stdin: Linux limits a single environment string to 128 KiB.
const maxBodyEnv = 32 << 10

// CommandOptions configure a CommandHandler.
type CommandOptions struct {
	Dir     string        // working directory; "" for the current one
	Env     []string      // extra KEY=VALUE pairs, after the scrubbed parent environment
	Timeout time.Duration // per run; the request's TTL also applies
	Log     *slog.Logger
}

// CommandHandler runs argv once per message. The command gets the
// envelope JSON on stdin and these variables:
//
//	AGENTBUS_FROM      sender, checked by the broker
//	AGENTBUS_TYPE      request or event
//	AGENTBUS_ID        envelope id
//	AGENTBUS_OP        body.op, if the body is an object with a string op
//	AGENTBUS_BODY      the body as JSON, if it is at most 32 KiB
//	AGENTBUS_PREAMBLE  text to put in front of the envelope if an LLM reads it
//
// Its stdout is the reply body: JSON is sent as JSON, anything else as a
// string, nothing as null. To refuse with a specific error, exit with
// status 3 and print {"code": "...", "reason": "..."}: the requester gets
// that code (1-64 chars of a-z 0-9 _ . -) and reason. Any other non-zero
// exit sends a generic error; stderr stays in the local log. The command runs without AGENTBUS_TOKEN or
// AGENTBUS_TOKEN_FILE, in its own process group, which is killed when the
// request's TTL or the timeout runs out. Output over 1 MiB is an error.
func CommandHandler(argv []string, o CommandOptions) client.Handler {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, e *a2a.Envelope) (any, error) {
		in, err := e.Marshal()
		if err != nil {
			return nil, err
		}
		if o.Timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, o.Timeout)
			defer cancel()
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = o.Dir
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = append(append(ScrubEnv(os.Environ()), o.Env...), messageEnv(e)...)
		setProcessGroup(cmd)
		cmd.WaitDelay = 2 * time.Second
		stdout := &capBuffer{max: MaxOutput}
		stderr := &capBuffer{max: MaxOutput}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		runErr := cmd.Run()
		if stdout.over || stderr.over {
			o.Log.Warn("handler output too large", "id", e.ID, "max", MaxOutput)
			return nil, errors.New("handler output too large")
		}
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) && exitErr.ExitCode() == ExitReplyError {
			var re struct {
				Code   string `json:"code"`
				Reason string `json:"reason"`
			}
			if json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &re) == nil && re.Code != "" {
				return nil, client.Errorf(re.Code, "%s", re.Reason)
			}
		}
		if runErr != nil {
			// The details stay local: stderr can hold paths, secrets or
			// stack traces that the requesting agent shouldn't see.
			o.Log.Warn("handler command failed", "id", e.ID, "from", e.From, "err", runErr,
				"stderr", strings.TrimSpace(stderr.String()))
			if ctx.Err() != nil {
				return nil, errors.New("handler timed out")
			}
			return nil, errors.New("handler failed")
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			o.Log.Info("handler stderr", "id", e.ID, "stderr", s)
		}
		out := bytes.TrimSpace(stdout.Bytes())
		if len(out) == 0 {
			return nil, nil
		}
		if json.Valid(out) {
			return json.RawMessage(out), nil
		}
		return string(out), nil
	}
}

// messageEnv is the per-message environment for a command. The sender,
// type and id are checked by the broker (ids are limited to safe
// characters); op and body are the sender's and must be treated as data.
func messageEnv(e *a2a.Envelope) []string {
	env := []string{"AGENTBUS_FROM=" + e.From, "AGENTBUS_TYPE=" + string(e.Type), "AGENTBUS_ID=" + e.ID,
		"AGENTBUS_PREAMBLE=" + a2a.UntrustedPreamble(e)}
	if op := opOf(e); op != "" {
		env = append(env, "AGENTBUS_OP="+op)
	}
	if len(e.Body) > 0 && len(e.Body) <= maxBodyEnv && !bytes.ContainsRune(e.Body, 0) {
		env = append(env, "AGENTBUS_BODY="+string(e.Body))
	}
	return env
}

// opOf returns body.op when the body is an object with a string op.
func opOf(e *a2a.Envelope) string {
	var b struct {
		Op string `json:"op"`
	}
	if len(e.Body) == 0 || e.Body[0] != '{' || json.Unmarshal(e.Body, &b) != nil {
		return ""
	}
	return b.Op
}

// ScrubEnv drops agentbus credentials from a child's environment.
func ScrubEnv(env []string) []string {
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
