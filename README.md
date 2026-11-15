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
  agent, so no agent can impersonate another.
- **Contract ACLs.** Only the owner can read an inbox. `send_to` lists who
  each agent may message. Task queues require a granted capability. An
  agent can write only its own presence and events.
- **Rejections are reported, not just dropped.** A refused publish is
  acknowledged, so it isn't retried, and the sender gets a correlated
  `error` envelope. Pending requests fail fast instead of timing out.
- **Per-agent rate limits, dedupe by envelope ID, and TTL checks.** These
  matter when every message can start a paid LLM call.
- **Presence.** Agents publish retained cards. The broker stamps them, and
  when an agent disconnects it writes the offline card itself, keeping the
  agent's description and capabilities.
- **Observers.** An identity marked `observer` can subscribe to anything,
  including `#` and other agents' inboxes, for debugging tools. It still
  can't take tasks or send as another agent.
- **Audit log.** One structured line per envelope, without bodies.

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
- **conduit-23a (Unified Inbox) fits this contract.** Publishers send
  `event` envelopes to `a2a/v1/agents/conduit-home/inbox`, and
  `headers.priority` drives the spawn/queue/log routing.

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
docs/             how it works; desktop MQTT clients
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

- Persistent sessions and retained messages live in memory, so a broker
  restart loses queued inbox messages. mochi has bolt, badger and redis
  storage hooks to wire in.
- No mTLS client auth yet, only tokens over optional server TLS.
- Late replies to a Request that already timed out go to the Inbox handler.
- No metrics endpoint yet.
- mochi shares one in-flight packet ID table across both directions, so a
  SUBSCRIBE can be refused with 0x91. The Go client retries. Other clients
  should retry too.
