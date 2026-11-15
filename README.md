# agentbus

An agent-to-agent message bus over MQTT for conduit and other AI agents.
One Go binary runs the broker and also works as a CLI client.

- **[CONTRACT.md](CONTRACT.md)** is the language-neutral spec: topics, the
  envelope, request/reply, presence and rejection rules. Any MQTT client
  that follows it can join.
- **`a2a`** is the contract as Go types and validation. It uses only the
  standard library.
- **`client`** is a Go client. It uses `paho.mqtt.golang` over MQTT 3.1.1,
  the same library conduit already depends on.
- **`broker`** is a broker that enforces the contract, built on
  [mochi-mqtt](https://github.com/mochi-mqtt/server). It can run standalone
  or be embedded.
- **`cmd/agentbus`** is the binary: `serve`, plus client commands for
  scripts and for agents that aren't written in Go.

## Try it

```bash
make demo
```

This starts a broker and two Go agents, a worker and a coordinator. They
pass messages through inboxes, a shared task queue and broadcast events,
then the bus stays up for you to explore. See [demo/README.md](demo/README.md)
for the walkthrough, [docs/HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) for
the diagrams, and [docs/DESKTOP-CLIENTS.md](docs/DESKTOP-CLIENTS.md) to watch
the bus in MQTT Explorer, MQTTX or `mosquitto_sub`.

## Quick start (your own agents)

```bash
go build -o bin/agentbus ./cmd/agentbus
bin/agentbus token            # once per agent; put token_sha256 in the config
cp examples/broker.json broker.json   # then edit agents and hashes
bin/agentbus serve -config broker.json
```

```bash
export AGENTBUS_URL=tcp://localhost:1883 AGENTBUS_AGENT=researcher AGENTBUS_TOKEN=...
bin/agentbus handle -capability summarize -exec './summarize.sh'   # stdin: envelope, stdout: reply body
```

```bash
export AGENTBUS_AGENT=cli-ops AGENTBUS_TOKEN=...
bin/agentbus task -capability summarize -body '{"text":"..."}'
bin/agentbus agents
```

From Go:

```go
c, err := client.Connect(ctx, client.Options{
    BrokerURL: "tcp://localhost:1883", AgentID: "conduit-home", Token: tok,
    Description: "Home gateway",
    Inbox: func(ctx context.Context, e *a2a.Envelope) (any, error) {
        // requests: return the reply body; events: return values ignored
        return map[string]string{"ok": "yes"}, nil
    },
})
reply, err := c.Request(ctx, "researcher", map[string]string{"q": "..."})
reply, err = c.RequestTask(ctx, "summarize", doc)   // any worker with the capability
err = c.ServeTasks(ctx, "summarize", handler)       // become a worker
err = c.Emit(ctx, "jobs/finished", result)          // broadcast
```

## What the broker enforces

Mosquitto can't do these things on its own:

- **Sender identity.** The envelope's `from` must match the authenticated
  agent, so no agent can impersonate another. The broker forwards its own
  re-encoding of each envelope, and refuses MQTT Will messages, so neither
  JSON tricks nor side channels get around this.
- **Contract ACLs.** Only the owner can read an inbox. `send_to` lists who
  each agent may message. Task queues require a granted capability and the
  exact shared subscription, so each task reaches one worker. An agent can
  write only its own presence and events.
- **Rejections are reported, not just dropped.** A refused publish is
  acknowledged, so it isn't retried, and the sender gets a correlated
  `error` envelope with a stable code. That includes `no_route` (the
  recipient is offline with no session) and `no_consumer` (nobody serves
  the task). Pending requests fail fast instead of timing out.
- **Limits on everything.** Per-agent rate limits (overridable per agent),
  dedupe by envelope ID, clock-skew and TTL checks, connection caps and a
  CONNECT deadline, and bounded offline queues. These matter when every
  message can start a paid LLM call. See [docs/SECURITY.md](docs/SECURITY.md).
- **Presence.** Agents publish retained cards. The broker stamps them,
  drops ungranted capabilities, checks public keys against pinned ones
  (`keys_pinned`), and writes the offline card itself when an agent
  disconnects.
- **Observers.** An identity marked `observer` can subscribe to anything,
  including `#` and other agents' inboxes, for debugging tools. It still
  can't take tasks or send as another agent.
- **Retention and audit (optional).** With `data_dir`, sessions, queued
  messages, subscriptions, presence and the dedupe window survive restarts
  (bbolt, embedded). With `audit_file`, every decision goes to a rotated JSONL
  log with a hash of each body but never the body itself. See
  [docs/RETENTION.md](docs/RETENTION.md).

`body` is never inspected, so payloads can be end-to-end encrypted (`enc`),
as conduit-1gkv plans.

## Why not port Mosquitto

The protocol core (codec, sessions, QoS, retained messages, wildcard
matching) is solved, and it's where the subtle bugs live. mochi-mqtt
provides it with hooks for everything agent-specific. Mosquitto is C under
EPL/EDL, so a port would be a derivative work. Use it as a reference for
behavior instead.

## Conduit integration notes

From conduit at `5626e55`:

- **Agents today are sessions inside one gateway process.** On the bus, one
  conduit instance is one agent ID. To address a specific conduit session,
  use `headers` (for example `"session": "tg:12345"`).
- **Conduit already uses `paho.mqtt.golang`,** so importing `client` adds
  nothing new. Request/reply and TTL travel in the envelope, so MQTT 5
  isn't needed.
- **Inbox handlers map onto conduit's existing pieces.** The `mqttevents`
  spawner already starts a sub-agent session from an untrusted payload, and
  an inbox handler can do the same with a request body. Its "evidence, not
  instructions" preamble applies here too.
- **conduit-23a (Unified Inbox) maps onto the envelope.** Publishers send
  `event` envelopes to `a2a/v1/agents/conduit-home/inbox`:

  | 23a field | Envelope field |
  |---|---|
  | `source` | `from`, checked by the broker |
  | `event` | `event` |
  | `priority` | `priority`, which drives conduit's spawn/queue/log routing |
  | `message`, `context` | `body` |

  23a's security list is mostly broker-enforced: ACLs, schema validation,
  size limits, per-source rate limits, dedupe, and audit lines with an
  `action` field. conduit still owns its global spawn budget and circuit
  breaker. The client's `MaxConcurrent` and `busy` replies are the agent-side
  backstop.
- **Approval gating.** `client.Options.BeforePublish` runs before every
  publish. conduit can route agent-initiated sends through its approval
  flow there, the way it gates the `mqtt` tool today.
- **Prompt injection.** Put `a2a.UntrustedPreamble(env)` in front of any
  envelope an LLM sees, matching conduit's MQTT-event preamble.
- **Federation keys (conduit-1gkv phase 3).** Presence cards carry
  `keys.sign` (Ed25519) and `keys.kex` (X25519), and the broker pins them
  from config. Signing and sealed bodies are the next step; see
  [docs/SECURITY.md](docs/SECURITY.md#not-done-yet).

## Layout

```text
a2a/              contract types and validation (stdlib only)
broker/           contract-enforcing broker on mochi-mqtt
client/           Go client (paho, MQTT 3.1.1)
cmd/agentbus/     the binary: serve + CLI client
demo/agents/      worker and coordinator agents (library)
demo/cmd/         their binaries
test/e2e/         functional tests over real processes
scripts/demo.sh   the demo
CONTRACT.md       the spec; contract/envelope.schema.json
docs/             how it works; security; retention and audit; desktop clients
```

## Testing

| Command | What runs | Time |
|---|---|---|
| `make test` | Unit tests (`a2a`) and in-process broker tests (`broker/e2e_test.go`: auth, ACLs, spoofing, dedupe, TTL, rate limits, task queues, presence) | ~2s |
| `make test-e2e` | Functional test (`test/e2e`). Builds the three binaries and runs the broker, worker and coordinator as separate processes over TCP. Checks the coordinator's six steps, CLI `task` and `request` against the live worker, a `send_to` rejection, a bad-token login, and the worker showing online then offline in `agentbus agents`. | ~5s |
| `make test-all` | `go vet` and everything, with `-race` | ~10s |
| `make demo-once` | The demo conversation, non-interactive. The exit status is the result. | ~3s |

`go test` without `-short` includes the functional test. It needs the Go
toolchain on `PATH` and permission to listen on localhost.

The Makefile sets `GOWORK=off`, because `~/git/claude/go.work` covers this
directory but doesn't list this module. Set it yourself when running `go`
directly.

## Not done yet

- End-to-end signatures and sealed (encrypted) bodies. Keys are
  distributed and pinned, but nothing signs or encrypts yet.
- Searchable message history with bodies (an opt-in SQLite store). The
  audit log records decisions, not contents.
- No token rotation or config reload without a restart.
- No mTLS client auth yet, only tokens over TLS.
- Late replies to a Request that already timed out go to the Inbox handler.
- No metrics endpoint yet.
- mochi shares one in-flight packet ID table across both directions, so a
  SUBSCRIBE can be refused with 0x91. The Go client retries. Other clients
  should retry too.
