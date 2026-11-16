# Standalone agents

An agent here is a small, single-purpose program that runs on one host
and serves a few bus topics for domain-specific work: disk and GPU
probes on build machines, log fetchers on app servers, a deploy hook on a
bastion. `agentbus agent` generates them:

```bash
agentbus agent init disk-agent                 # a project with an example of each handler kind
agentbus agent run                             # try it from disk, no build
agentbus agent build --target linux/amd64      # one static binary, ~6.5 MiB
```

The binary carries everything it needs except its token: the manifest,
every script, the Go handlers, and the agentbus client. Copy it to a
host, give it a token file, and run it.

- [How it fits together](#how-it-fits-together)
- [The manifest](#the-manifest)
- [Writing handlers](#writing-handlers): shell, Python and other scripts, Go
- [Building](#building)
- [Deploying](#deploying)
- [Operating](#operating)
- [Limits](#limits)

## How it fits together

```text
agent.yaml   which topics, and which handler runs for each
handlers/    Go functions, compiled in            agent.Register("greet", greet)
scripts/     shell, Python, anything with a shebang   embedded with go:embed
main.go      agent.Main(files, "agent.yaml")

agentbus agent build  ->  dist/disk-agent-linux-amd64   (CGO off, trimmed, stripped)

on the host:  disk-agent -check   ->  disk-agent  (systemd)  ->  broker
```

The runtime is the `agent` package. It connects with the Go `client`,
so everything in [SERVICES.md](SERVICES.md) holds:

- each message is acknowledged after it's answered
- there's one reply per request
- each handler's deadline is the request's TTL
- concurrency is bounded

`agentbus handle` uses the same command runner, so a script that works
under one works under the other.

## The manifest

```yaml
agent: disk-agent                       # agent ID; AGENTBUS_AGENT overrides
description: Disk usage on build hosts  # starts the presence description
# broker: ssl://bus.example.com:8883    # default; AGENTBUS_URL overrides
# token_file: /etc/agentbus/disk-agent.token  # default; AGENTBUS_TOKEN_FILE overrides
max_concurrent: 4                       # handlers at once; beyond that, "busy"
timeout: 60s                            # default per handler
drain: 30s                              # on SIGTERM, time for running handlers
allow_from: [ops, conduit-home]         # default sender allow-list; all if unset
env: {LC_ALL: C}                        # extra environment for commands
handlers:
  - task: disk.usage                    # trigger
    script: scripts/disk.sh             # action
    timeout: 10s
    description: 'body {"path"?: string}; replies usage of that filesystem'
```

Each handler has one trigger and one action.

| Trigger | Runs for |
|---|---|
| `task: CAP` | tasks on the capability's queue. One agent serving it gets each task. |
| `inbox: true` | requests and events sent to this agent. With `op: NAME`, only those whose `body.op` is NAME. One inbox handler may have no `op` and take the rest. |
| `event: FILTER` | events matching an `a2a/v1/events/...` filter. No reply. |

| Action | Runs |
|---|---|
| `run: COMMAND` | `sh -c COMMAND` |
| `script: scripts/FILE` | the script, with `interpreter:` (for example `python3 -u` or `/opt/venv/bin/python`), or by extension: `.sh` sh, `.bash` bash, `.py` python3; otherwise directly, so it needs a shebang |
| `go: NAME` | the Go function registered as NAME |

Handlers can set `timeout`, `allow_from` (an empty list allows everyone,
overriding the manifest's), `workdir`, `env` and `description`.

How messages are routed:

- **Inbox requests are routed by `body.op`.** A request with an op no
  handler takes, and no catch-all handler, gets `unknown_op`, and the
  reason lists the ops the agent handles.
- **Senders outside `allow_from` get `forbidden`.** `from` is checked by
  the broker, so the list can be trusted. This check is in addition to
  the broker's grants.
- **The presence description lists every handler's description.** Callers
  see the body shape each task and op expects; LLM callers see it through
  the MCP `agents` tool.

Unknown keys are errors, so a typo can't silently drop a setting. A
`token:` key is refused, because the manifest is built into the binary.

## Writing handlers

### Shell and Python (any script)

The contract is the same as `agentbus handle`:

- **stdin:** the whole envelope as JSON.
- **stdout:** the reply body. JSON is sent as JSON, other text as a
  string, and empty output as null.
- **Exit 3** with `{"code": "...", "reason": "..."}` on stdout refuses
  with that error. The code is 1-64 chars of `a-z 0-9 _ . -`.
- **Any other non-zero exit** sends a generic `handler_error`. stderr goes
  to the agent's log, not to the requester.
- **Environment:**

  | Variable | Contents |
  |---|---|
  | `AGENTBUS_FROM` | the sender, checked by the broker |
  | `AGENTBUS_OP` | `body.op` |
  | `AGENTBUS_BODY` | the body, if it's at most 32 KiB |
  | `AGENTBUS_TYPE`, `AGENTBUS_ID` | the envelope's type and ID |
  | `AGENTBUS_SELF`, `AGENTBUS_HANDLER` | this agent's ID and the handler's name |
  | `AGENTBUS_PREAMBLE` | the untrusted-input text, for scripts that call an LLM |
- **Limits:** the command runs in its own process group, which is killed
  at the deadline. Output over 1 MiB is an error. The agent's token is
  removed from its environment.

```python
#!/usr/bin/env python3
import json, sys
env = json.load(sys.stdin)
body = env.get("body")
if not isinstance(body, dict) or not isinstance(body.get("text"), str):
    json.dump({"code": "invalid", "reason": 'body must be {"text": "..."}'}, sys.stdout)
    sys.exit(3)
json.dump({"words": len(body["text"].split())}, sys.stdout)
```

**Everything in a message comes from another agent.** Check its shape
first and never put it into a shell command unquoted. The generated
`scripts/disk.sh` shows how: it allows only plain absolute paths and
passes the path after `--`. An attempt like `"/; id"` is refused with
`invalid`.

Scripts are extracted from the binary into a private temporary directory
(mode 0700) when the agent starts, keeping their layout so a script can
use sibling files such as Python modules. The directory is removed when
the agent stops. Interpreters come from the host. Python scripts that
need third-party packages should name a virtualenv's python in
`interpreter:`, or use only the standard library.

### Go

A Go handler is a `client.Handler`. It registers itself from an `init`
function in the project's `handlers` package:

```go
func init() { agent.Register("greet", greet) }

func greet(ctx context.Context, e *a2a.Envelope) (any, error) {
	var body struct{ Name string `json:"name"` }
	if err := e.DecodeBody(&body); err != nil {
		return nil, client.Errorf("invalid", `body must be {"name": string}`)
	}
	return map[string]string{"greeting": "hello " + body.Name}, nil
}
```

- **The return value** is the reply body. `client.Errorf(code, ...)`
  chooses the error code.
- **`ctx`** ends at the request's deadline or the handler's `timeout`,
  whichever comes first.
- **To add a handler,** add a `.go` file to `handlers/` with its own
  `init`. Registering a name twice panics at start, so two plugins can't
  silently replace each other.

Go handlers exist only in a built binary. `agentbus agent run` skips them
with a warning and runs the rest.

## Building

```bash
agentbus agent build                                   # this machine
agentbus agent build --target linux/amd64,linux/arm64,darwin/arm64,windows/amd64
```

`build` needs the Go toolchain. For each target it runs:

```text
go mod tidy
CGO_ENABLED=0 GOOS=… GOARCH=… go build -trimpath -ldflags="-s -w"
```

It prints each binary's size and SHA-256. The binary for the build
machine is then run with `-check`, so a manifest naming an unregistered
Go handler fails the build rather than the deployment. The binaries are
static, so they run on any host of the right OS and architecture with no
runtime dependencies besides the interpreters your scripts need.

- **The agentbus version.** `go.mod` pins the version of the `agentbus`
  binary that ran `init`. A local build of agentbus has no version, so the
  build uses the latest agentbus module instead.
- **Building against a checkout.** `agent init --agentbus-src DIR` (or
  `AGENTBUS_SRC`) points the project at a local checkout with a `replace`
  directive, for offline builds or unreleased changes.
- **Windows.** A Windows build works, but timeouts kill only the command
  itself, not processes it started.

## Deploying

On the broker host, create the agent's identity with the capabilities
its tasks need:

```bash
agentbus config agent add disk-agent --capability disk.usage
```

That writes `~/.config/agentbus/tokens/disk-agent.token`. On the target
host:

```bash
sudo install -m 0755 disk-agent-linux-amd64 /usr/local/bin/disk-agent
sudo install -m 0600 -D disk-agent.token /etc/agentbus/disk-agent.token
AGENTBUS_URL=ssl://bus.example.com:8883 disk-agent -check   # manifest, interpreters, token
sudo cp deploy/disk-agent.service /etc/systemd/system/
sudo systemctl enable --now disk-agent
```

The generated unit:

- **Delivers the token as a systemd credential,** readable only by the
  service.
- **Runs it as a throwaway user** (`DynamicUser=yes`).
- **Makes the system read-only to it,** with `ProtectSystem=strict`.
  Loosen that, or add `ReadWritePaths=`, for handlers that change the
  host.
- **Leaves time to drain on stop:** `TimeoutStopSec` is longer than the
  manifest's drain.

The binary resolves its connection in this order:

1. flags: `-url`, `-agent`, `-token-file`
2. `AGENTBUS_URL`, `AGENTBUS_AGENT`, `AGENTBUS_TOKEN` or `AGENTBUS_TOKEN_FILE`
3. the manifest's `broker` and `token_file`
4. `/etc/agentbus/<agent>.token`, then `~/.config/agentbus/tokens/<agent>.token`

`-manifest FILE` replaces the embedded manifest on one host; scripts still
come from the binary. `-print-manifest` shows the embedded manifest, and
`-version` shows the build.

## Operating

- **On SIGTERM or SIGINT,** the agent answers new requests with `busy`,
  waits up to `drain` for running handlers to finish and their replies to
  go out, then disconnects. A task already accepted isn't lost to a
  restart.
- **Logs** go to stderr: warnings by default, one line per handled message
  with `-v`, JSON with `-log-format json`.
- **The broker's request metrics** show each task queue's answered,
  errored and expired counts and its latency. See
  [HOW-IT-WORKS.md](HOW-IT-WORKS.md#request-tracking-and-metrics).
- **The presence card** carries `meta.runtime=agentbus-agent` and
  `meta.host`, so `agentbus agents` shows where each agent runs.

## Limits

- **Building needs the Go toolchain,** even for agents with only scripts.
  `agentbus agent run` needs none, but then the manifest and scripts have
  to be on the host as files.
- **Go handlers can't be added to a built binary** without rebuilding it.
  Scripts can't either, since they're embedded; `-manifest` can only
  rearrange what's already inside.
- **One agent ID per running copy.** Two hosts running the same binary
  need two identities, or they knock each other off the bus. To spread
  work across hosts, give each host its own ID and the same capability.
  Each copy then gets its own token via `AGENTBUS_AGENT` and
  `AGENTBUS_TOKEN_FILE`.
