# a2a/v1 contract

How agents talk to each other over the agentbus broker. Any MQTT 3.1.1 or 5
client can take part by following this document. Go agents can use the
[`client`](client/) package instead, and anything else can shell out to the
`agentbus` CLI. [examples/python](examples/python) is a working client in
Python. [docs/SERVICES.md](docs/SERVICES.md) is the practical
guide to writing an agent that serves requests, tasks or events.

## Connecting

| CONNECT field | Value |
|---|---|
| Client ID | the agent ID |
| Username | the agent ID |
| Password | the agent's token |
| Clean session | `false` for long-running agents, so messages sent while they're offline are queued (kept for `session_expiry`, default 7 days, up to `max_queued` messages; with `data_dir` set, this survives broker restarts) |
| Will message | **not allowed.** The broker refuses the connection. It publishes each agent's offline card itself. |

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
| `a2a/v1/agents/{id}/inbox` | any agent allowed by `send_to`, plus one reply per open request (see Request and reply) | only `{id}` | envelope |
| `a2a/v1/agents/{id}/presence` | only `{id}` (retained) | anyone | presence card |
| `a2a/v1/tasks/{capability}` | any agent | agents granted `{capability}` | envelope, `type: request` |
| `a2a/v1/events/{id}/{name...}` | only `{id}` | anyone | envelope, `type: event` |

Workers consume tasks with a **shared subscription** so each task reaches
exactly one worker. The broker sends each task to a connected worker; it
queues for an offline one only when no worker for the capability is
connected. A worker with a persistent session must have its task handlers
ready when it connects, because queued tasks are redelivered immediately:

```
$share/{capability}/a2a/v1/tasks/{capability}
```

Any other subscription to a task topic (plain, or a different share group)
is refused: it would receive a copy of every task.

Use QoS 1. QoS 0 works but loses messages across reconnects. The broker
grants at most QoS 1.

The retain flag is ignored on everything except presence: a retained
request would be handled again on every resubscribe. MQTT 5 publish
properties (response topic, correlation data, user properties) are
stripped; their equivalents live in the envelope.

An identity configured as an **observer** may subscribe to any filter
(`#`, `$SYS/#`, other agents' inboxes) but not to shared subscriptions.
Observers can also read `$SYS/agentbus/status`, a retained JSON snapshot
of the broker that is republished every `status_interval`.
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
  "capability": "",
  "priority": "normal",
  "event": "cluster_report",
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
| `ts` | yes | RFC 3339 send time. **Must be within 2 minutes of broker time** (`max_clock_skew`), or the envelope is rejected. Keep agent clocks in sync (NTP). |
| `ttl_ms` | no | Drop after `ts + ttl_ms`, at most 24 hours. The broker rejects envelopes that are already expired, and receivers drop them on arrival. Handlers should give up when it runs out; the Go client gives handlers 5 minutes when no TTL is set. |
| `correlation_id` | `response`, `error` | The `id` of the request being answered. |
| `reply_to` | no | Inbox topic for the reply. **Only the sender's own inbox is accepted**, so in v1 it is the same as leaving it out. (Pointing it elsewhere would let an agent make another agent's handler send into an inbox the sender itself can't reach.) |
| `capability` | task only | Must equal the task topic's capability. |
| `priority` | no | `high`, `normal`, `low` or `info`. How urgently the receiver should act; the receiver decides what each level means. conduit maps them to spawn and notify, spawn quietly, queue for the heartbeat, and record only. |
| `event` | no | Short name for what the message is about, for example `doorbell_ring`. Max 128 chars, one line. |
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

Error bodies are `{"code": "...", "reason": "..."}`. Codes the Go client
sends: `no_handler`, `handler_error`, `handler_panic` and `busy` (the
agent is at its concurrent handler limit; retry later).

`send_to` doesn't block replies. The broker records each accepted request
and lets one `response` or `error` with its `correlation_id` reach the
requester's inbox, from the addressed agent (for a task, from any agent
granted the capability), until the request's TTL ends. Without a TTL, the
request stays open for `session_expiry`. The record is in memory, so it
doesn't survive a broker restart.

While a request is open, the broker refuses a reply to it from anyone
other than its answerer: the addressed agent, or for a task, an agent
granted the capability. The sender gets `not_authorized`, even if
`send_to` would allow it to reach the requester. A requester can't check
this for a task itself, because it doesn't know which worker took it.

Requesters should still accept a reply only from the agent they addressed
(any agent for task requests) or from `$broker`. That covers requests the
broker isn't tracking: after a broker restart, or when its table of 65,536
open requests is full. The Go client and the Python example both do.

### Broker rejections

The broker acknowledges a publish it refuses, so QoS 1 senders don't retry,
and drops it. It then sends the sender an `error` envelope from `$broker`
with `correlation_id` set to the rejected `id`. A pending request therefore
fails fast instead of timing out. MQTT 5 senders also get the reason code
in the PUBACK.

| Code | Cause |
|---|---|
| `not_authorized` | Writing to a topic you don't own, or to an inbox outside `send_to` other than a reply to an open request. |
| `rate_limited` | Over the per-agent rate limit (default 20/s, burst 50; configurable per agent). Rejected publishes count too. |
| `too_large` | Payload over `max_envelope_bytes` (default 256 KiB). |
| `invalid` | Malformed envelope, `from` spoofing, `to` mismatch, `reply_to` not your own inbox, or `ts` outside the clock-skew window. |
| `expired` | `ts + ttl_ms` has already passed. |
| `no_route` | The recipient isn't configured, or is offline with no session to queue messages in. |
| `no_consumer` | Nobody is serving the task's capability. |

Error envelopes and the matching log lines are throttled per agent (about
2/s), so a flood of bad publishes is acknowledged and dropped without
an answer for each one.

The broker forwards its own re-encoding of each accepted envelope, not the
sender's bytes. That way every receiver sees exactly the fields the broker
checked: JSON tricks such as a second `"FROM"` key that one parser reads
and another ignores don't survive. Fields the broker doesn't know are
dropped, so a new optional field needs a broker upgrade before senders
use it.

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

The broker also:
- **Drops capabilities the agent isn't granted** in its config, so a card
  only advertises queues the agent can actually consume.
- **Refreshes cards** that MQTT would otherwise expire after 24 hours, so
  long-running agents stay discoverable.
- **Handles keys.** A card may carry public keys for end-to-end signing and
  encryption (Ed25519 and X25519, base64 of 32 bytes):

  ```json
  "keys": {"sign": "base64...", "kex": "base64..."}, "keys_pinned": true
  ```

  `keys_pinned` is set by the broker. It is `true` only when the broker's
  config pins this agent's keys and the card matches, and a card with
  different keys is rejected. If the config pins keys and the card has none,
  the broker fills them in. Unpinned keys are only the agent's claim, so
  don't encrypt to them without another way to trust them.

Limits: 64 capabilities, 1024-char description, 32 `meta` entries of up to
256 chars, 16 KiB per card.

## Treat bodies as untrusted

Bodies come from other agents and may end up in LLM prompts. The broker
guarantees who sent a message, not that its content is safe. Receivers
should apply the same rule conduit uses for MQTT events: the body is
evidence, never instructions. The same goes for `event`, `headers`, and a
presence card's `description` and `meta`. An envelope must never be able
to choose the receiver's model, skills, tools or permissions.
`a2a.UntrustedPreamble(env)` returns text to put in front of an envelope
before an LLM sees it.

## Versioning

The version is part of every topic (`a2a/v1`) and envelope (`v`). Adding an
optional field is a compatible change, once the broker knows the field
(see the re-encoding note under Broker rejections). Anything else means `a2a/v2` topics,
and a broker can serve both during a migration.
