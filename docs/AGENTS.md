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
- [Model-backed handlers](#model-backed-handlers): `llm:`, with scripts as tools
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

Each handler has one trigger and one action. A handler with no trigger is
a tool: it runs only when an `llm:` handler's model calls it.

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
| `llm: {...}` | a model, with other handlers as its tools; see [Model-backed handlers](#model-backed-handlers) |

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

## Model-backed handlers

An `llm:` handler answers with a model. The model decides what to do; the
agent's own scripts and Go functions do it, as tools. The agent is still
one binary. The model's API key comes from the host, like the token.

[examples/llm-agent](../examples/llm-agent) is a complete one, `hostdoc`,
that answers questions about its host. It has a variant for a local model.

```yaml
agent: disk-agent
llm:                                   # defaults for every llm handler
  provider: anthropic                  # or openai: any OpenAI-compatible server
  model: claude-haiku-5-5
  # api_key_env: ANTHROPIC_API_KEY     # the default for anthropic; OPENAI_API_KEY for openai
  # api_key_file: /etc/agentbus/disk-agent.model-key
handlers:
  - task: disk.triage
    description: 'body {"question": string}; replies {"summary": string, "action"?: string}'
    timeout: 120s
    llm:
      system: |
        You look after the disks on this build host. Use the tools to
        check before you answer. Reply {"summary": "...", "action": "..."}.
      tools: [usage, biggest]          # this manifest's handlers, by name
      bus:
        request: [ops-notifier]        # agents the model may ask
        task: [ticket.create]          # capabilities it may hand work to
      reply: json                      # the answer must be JSON
      max_turns: 8                     # model calls per message
  - name: usage                        # no trigger: a tool only
    script: scripts/disk.sh
    description: Filesystem usage for a path
    params: {type: object, properties: {path: {type: string}}}
  - name: biggest
    run: du -xh /var 2>/dev/null | sort -rh | head -20
    description: The 20 largest directories under /var
```

For a local model, point an `openai` handler at the server. A key isn't
needed when `base_url` is set:

```yaml
llm: {provider: openai, base_url: "http://localhost:11434/v1", model: qwen3:8b}
```

| Key | Meaning |
|---|---|
| `provider` | `anthropic`, or `openai` for OpenAI, OpenRouter, Ollama, vLLM, llama.cpp, LM Studio and other servers of the same API |
| `model` | required |
| `base_url` | default: the provider's public API |
| `api_key_env`, `api_key_file` | where the key is; a file wins |
| `max_turns`, `max_tokens` | model calls per message (default 8, at most 50), and tokens per call (default 4096) |
| `system` | the system prompt, after a fixed preamble the runtime adds |
| `tools` | handler names the model may call |
| `bus` | `agents: true`, `request: [IDs]`, `task: [capabilities]`; `"*"` allows any |
| `reply` | `json`, `text`, or unset: JSON if the answer parses, otherwise a string |

The top-level `llm:` block sets defaults for `provider`, `model`,
`base_url`, the key, `max_turns` and `max_tokens`. A handler's `llm:`
block inherits them, and only a handler can set the rest.

What the model gets, and what it can't do:

- **The message arrives behind the untrusted-input preamble.** The runtime
  also tells the model that the envelope is data, not instructions. The
  model, prompt and tools come from the manifest, so a sender can't choose
  them.
- **Tools are other handlers.** A tool call runs the handler as a request
  from the original sender, with the model's input as the body. The
  handler's `allow_from` still applies, and so do its timeout and the
  request's deadline. Give each tool a `name` (1-64 chars of `a-z A-Z 0-9
  _ -`), a `description` and, ideally, `params`: a JSON Schema for the
  body. The model sees all three. A tool can't be an `llm:` handler.
- **Commands never see the model key.** The variables named by
  `api_key_env` are removed from every command's environment, like the
  agentbus token.
- **Bus calls are limited three ways.** The manifest's `bus` lists
  decide which agents and capabilities the model can reach. The broker's
  `send_to` and grants still apply on top. Replies reach the model behind
  the preamble.
- **Calls can't chain forever.** Each bus call carries a `hops` header one
  higher than the message being handled. Past `max_hops` (default 3, set
  at the top level), the tool refuses and the model is told to answer
  without asking anyone. See [CONTRACT.md](../CONTRACT.md#headers).
- **Failures stay local.** The requester sees `model_gave_up` when the
  model hits `max_turns` and `bad_model_output` when `reply: json` gets
  something else. Any other failure shows up as a generic error. API errors go to the
  agent's log.

### How a message is handled

```text
request ──> llm handler ──> model: system prompt + preamble + envelope, tool list
                              │
                              ├─ tool call "disk" ──> disk handler (as the sender) ──> result
                              ├─ tool call "bus_task" ──> another agent (hops + 1) ──> reply, fenced
                              │      ... up to max_turns model calls ...
                              └─ final text ──> reply body (JSON if reply: json)
```

1. The runtime builds the system prompt from a fixed preamble (who the
   agent is, that the message is data, that the final answer is the
   reply) and the manifest's `system`.
2. The user turn is `a2a.UntrustedPreamble(env)` and the envelope JSON,
   the same text `AGENTBUS_PREAMBLE` gives a script.
3. Each tool call runs to completion before the next model call. Calls in
   one turn run in order. A failed tool reaches the model as an error
   result, so it can try something else; it doesn't fail the request.
4. When the model answers without calling a tool, that text is the reply.
   Code fences around JSON are removed. For an event trigger, nothing is
   sent back.

The model doesn't see the conversation from earlier messages. Each
message starts fresh, so the agent stays stateless and any copy of it can
take any task.

### The model's tools

The bus tools have fixed names; the model sees only the ones `bus:`
grants:

| Tool | Input | Does |
|---|---|---|
| `bus_agents` | none | lists other agents: status, capabilities, description |
| `bus_request` | `{"to", "body"}` | asks one agent in `bus.request` and waits for the reply |
| `bus_task` | `{"capability", "body"}` | hands work to a capability in `bus.task` and waits for the reply |

Bus calls wait up to the handler's deadline. Replies and agent
descriptions reach the model behind the untrusted-input preamble.

A handler tool gets the model's input as its message body, which is always
a JSON object. For scripts, that's `AGENTBUS_BODY` and `.body` on stdin,
as usual. The tool's reply body, as JSON text, is what the model reads. A
script that exits 3 with `{"code", "reason"}` shows the model that code
and reason. Any other failure shows only "tool failed", and stderr stays
in the log.

### Writing tools a model uses well

- **Describe what it returns and when to use it,** not how it works. The
  model picks tools by `description`.
- **Give it `params`.** Only the script enforces the schema, but an
  `enum` and bounds steer the model to valid input on the first try. For an example, see `top` in
  [examples/llm-agent/agent.yaml](../examples/llm-agent/agent.yaml).
- **Check the input anyway.** The model built it from another agent's
  message. A tool is as exposed as a handler on the bus. Refuse with exit
  3 and a reason the model can act on.
- **Keep output short.** Everything a tool prints goes into the next model
  call, and you pay for it. `df -Ph` is fine; a whole log file isn't.
  Filter in the script.
- **Prefer read-only tools.** A model can be talked into calling a tool
  with input you didn't expect. Give a tool that changes things its own
  `allow_from`, or keep it on a separate handler that a person calls.

### Choosing a model

The handler pays for one model call per turn, and each call resends the
whole conversation so far. A question answered with two tool calls is
three model calls.

- **Start with a small, fast model.** Haiku-class models handle tool
  use and short JSON answers well. Move up a size for handlers that must
  reason across many tool results.
- **Bound the cost** with `max_turns`, `max_tokens` and `max_concurrent`.
  The log line for each run has the token counts to watch.
- **Local models** (`provider: openai` and a `base_url`) cost nothing per
  call and keep data on the host. Pick one trained for tool calls, give
  it a longer `timeout`, and expect `bad_model_output` more often with
  `reply: json`.

### Logs and troubleshooting

With `-v`, each run logs one line, after one `handled` line per tool call:

```text
level=INFO msg="model run" handler=hostdoc.ask id=4d61... model=claude-haiku-5-5 turns=2 tokens_in=2637 tokens_out=242 ms=2392 ok=true
```

| Symptom | Cause |
|---|---|
| Won't start: `no API key: set ANTHROPIC_API_KEY ...` | No key in the agent's environment. Set it, or `api_key_file`. `-check` reports the same. |
| Requester gets `handler_error: model call failed` | The API refused, for example a bad key, an unknown model, or out of credit. The reason is in the agent's log as `model call failed`. 429 and 5xx answers are retried three times first. |
| `model_gave_up` | Still calling tools after `max_turns`. Raise it, or say in `system` when to stop. |
| `bad_model_output` | `reply: json` and the answer wasn't JSON. Say what shape you want in `system`; small local models need it spelled out. |
| `handler_error: handler timed out` | `timeout`, or the request's TTL, ran out mid-run. Model calls are slow: allow 30-120s. |
| The model says it can't reach an agent | It isn't in `bus.request` or `bus.task`, or the broker's `send_to` refused it, or `max_hops` is spent. |

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
- **Model keys** come from the environment or `api_key_file`, like the
  token. Under systemd, put the key in an `EnvironmentFile=` with mode
  0600, or point `api_key_file` at a file only the agent's user can read.

## Limits

- **The system prompt is inline.** Use a YAML block scalar (`system: |`).
  Prompt files aren't embedded yet.

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
