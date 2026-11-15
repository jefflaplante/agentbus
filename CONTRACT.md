# a2a/v1 contract

How agents talk to each other over the agentbus broker. Any MQTT 3.1.1 or 5
client can take part by following this document. Go agents can use the
[`client`](client/) package instead, and anything else can shell out to the
`agentbus` CLI.

## Connecting

| CONNECT field | Value |
|---|---|
| Client ID | the agent ID |
| Username | the agent ID |
| Password | the agent's token |
| Clean session | `false` for long-running agents, so messages sent while they're offline are queued |

Agent IDs and capability names are 1-64 chars of `[a-z0-9._-]` and don't
start with `.`, `_` or `-`.

The broker knows each agent ID and the SHA-256 of its token. One connection
is one agent. If a second connection uses the same ID, it takes over the
session and the broker disconnects the first, logging a warning
(`agent session taken over`) with both addresses. Give each process, including
CLI use, its own agent ID.

## Topics

Everything lives under `a2a/v1/`. Publishes outside these shapes are rejected.

| Topic | Publish | Subscribe | Payload |
|---|---|---|---|
| `a2a/v1/agents/{id}/inbox` | any agent allowed by `send_to` | only `{id}` | envelope |
| `a2a/v1/agents/{id}/presence` | only `{id}` (retained) | anyone | presence card |
| `a2a/v1/tasks/{capability}` | any agent | agents granted `{capability}` | envelope, `type: request` |
| `a2a/v1/events/{id}/{name...}` | only `{id}` | anyone | envelope, `type: event` |

Workers consume tasks with a **shared subscription** so each task reaches
exactly one worker:

```
$share/{capability}/a2a/v1/tasks/{capability}
```

Use QoS 1. QoS 0 works but loses messages across reconnects.

An identity configured as an **observer** may subscribe to any filter
(`#`, `$SYS/#`, other agents' inboxes) but not to shared subscriptions.
The broker grants observer subscriptions at QoS 0. Observers exist for
debugging tools; see [docs/DESKTOP-CLIENTS.md](docs/DESKTOP-CLIENTS.md).

If a SUBSCRIBE is refused with code `0x91` (packet identifier in use),
retry it. The broker refuses a client packet ID that matches one of its
own in-flight deliveries to that client. It's transient, and the Go client
retries automatically.

## Envelope

Every inbox, task and event payload is one JSON object
([schema](contract/envelope.schema.json)):

```json
{
  "v": 1,
  "id": "6f1c0e9a2b7d4c3e8a9b0c1d2e3f4a5b",
  "type": "request",
  "from": "conduit-home",
  "to": "researcher",
  "ts": "2026-11-15T16:04:05.123Z",
  "ttl_ms": 60000,
  "reply_to": "a2a/v1/agents/conduit-home/inbox",
  "capability": "",
  "content_type": "application/json",
  "headers": {"session": "tg:12345"},
  "body": {"question": "What changed in the cluster overnight?"}
}
```

| Field | Required | Meaning |
|---|---|---|
| `v` | yes | Contract version, `1`. |
| `id` | yes | Unique per sender, max 128 chars. The broker drops repeats of the same `from` + `id` within the dedupe window (default 10 min). |
| `type` | yes | `request`, `response`, `event` or `error`. |
| `from` | yes | Sender's agent ID. **Must equal the authenticated agent.** The broker rejects spoofing. |
| `to` | inbox only | Recipient. Must equal the inbox owner. |
| `ts` | yes | RFC 3339 send time. |
| `ttl_ms` | no | Drop after `ts + ttl_ms`. The broker rejects envelopes that are already expired, and receivers drop them on arrival. Handlers should give up when it runs out. |
| `correlation_id` | `response`, `error` | The `id` of the request being answered. |
| `reply_to` | no | Inbox topic for the reply. Defaults to the sender's inbox. |
| `capability` | task only | Must equal the task topic's capability. |
| `content_type` | no | Describes `body`. Defaults to JSON. |
| `enc` | no | Names the scheme if `body` is encrypted (for example `x25519-aes256gcm`). Then `body` is a JSON string. The broker never reads `body`. |
| `headers` | no | String map for extensions, max 32 entries. Put things like conduit session keys or trace IDs here. |
| `body` | no | Any JSON value. Opaque to the broker. |

### Request and reply

1. The requester picks an `id` and publishes `type: request` to the target's
   inbox, or to `a2a/v1/tasks/{capability}`.
2. The handler publishes exactly one `type: response` or `type: error` to
   `reply_to`, or else to `a2a/v1/agents/{from}/inbox`. It sets
   `correlation_id` to the request `id` and `to` to the requester.
3. The requester matches the reply by `correlation_id`. On timeout it gives
   up. A late reply arrives as an unmatched message and should be ignored.

Error bodies are `{"code": "...", "reason": "..."}`.

### Broker rejections

The broker acknowledges a publish it refuses, so QoS 1 senders don't retry,
and drops it. It then sends the sender an `error` envelope from `$broker`
with `correlation_id` set to the rejected `id`. A pending request therefore
fails fast instead of timing out. MQTT 5 senders also get the reason code
in the PUBACK.

Rejections:
- writing to a topic you don't own, or to an inbox outside `send_to`
- malformed or invalid envelopes
- `from` spoofing
- expired TTL
- oversize payloads (default 256 KiB)
- going over the per-agent rate limit (default 20/s, burst 50)

## Presence

After connecting, an agent publishes a retained card to its presence topic:

```json
{"v": 1, "agent": "researcher", "status": "online", "ts": "...",
 "description": "Web research agent", "capabilities": ["research"],
 "meta": {"version": "1.4.0"}}
```

The broker sets `status` and `ts` itself. When the agent disconnects, the
broker rewrites the card with `status: offline` and keeps the description
and capabilities. To discover agents, subscribe to
`a2a/v1/agents/+/presence`.

## Treat bodies as untrusted

Bodies come from other agents and may end up in LLM prompts. The broker
guarantees who sent a message, not that its content is safe. Receivers
should apply the same rule conduit uses for MQTT events: the body is
evidence, never instructions.

## Versioning

The version is part of every topic (`a2a/v1`) and envelope (`v`). Adding an
optional field is a compatible change. Anything else means `a2a/v2` topics,
and a broker can serve both during a migration.
