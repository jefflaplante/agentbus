# Connecting desktop MQTT clients

agentbus is a standard MQTT 3.1.1 and 5 broker, so tools like
[MQTT Explorer](https://mqtt-explorer.com), [MQTTX](https://mqttx.app) and
`mosquitto_sub`/`mosquitto_pub` can connect to it. Use them to watch
traffic and to send messages by hand. The broker enforces the
[a2a/v1 contract](../CONTRACT.md), so a few settings that are optional on
other brokers are required here.

## Three rules

1. **Client ID must equal the username,** and both are the agent ID.
   Desktop clients generate a random client ID by default (for example
   `mqtt-explorer-1a2b3c`), and the broker refuses the connection. Set the
   client ID by hand.
2. **Use a dedicated identity, never a running agent's.** MQTT allows one
   connection per client ID, so connecting as `worker` disconnects the real
   worker.
3. **To see everything, the identity must be an observer.** A normal agent
   can only subscribe to its own inbox, everyone's presence, all events, and
   task queues it's granted. The default `#` subscription is refused. An
   identity with `"observer": true` can subscribe to anything, including
   `#`, other agents' inboxes, task queues and `$SYS/#`. It still can't
   take tasks (shared subscriptions are refused) or send as anyone else.

## Get an identity

Every connection logs in as an agent listed in the broker config. The
username is the agent ID and the password is that agent's token. The
config stores only the token's SHA-256.

**With the demo (`make demo`):** an observer called `explorer` is created
for you.

```bash
cat demo/.run/tokens/explorer.token      # the password
# export AGENTBUS_URL=tcp://127.0.0.1:18830
# export AGENTBUS_AGENT=explorer
```

The demo mints new tokens on every run, so copy the token again after you
restart it.

**With your own broker:** mint a token and add an entry to the config.

```bash
bin/agentbus token
```

```json
{"id": "explorer", "token_sha256": "<token_sha256 from above>", "observer": true}
```

An observer can read every agent's inbox, so treat its token like an admin
credential. Leave out `"observer": true` for a tool that should see only
what a normal agent sees.

## MQTT Explorer

1. Click **+** next to *Connections* to create a connection.
2. Fill in the main form:

   | Field | Value |
   |---|---|
   | Name | `agentbus demo` (anything) |
   | Encryption (tls) | off. On if the broker has `tls_cert` set. |
   | Validate certificate | on for a real certificate. Off for a self-signed one. |
   | Protocol | `mqtt://` |
   | Host | `127.0.0.1` |
   | Port | `18830` for the demo; otherwise the broker's `listen` port, usually `1883` |
   | Username | `explorer` |
   | Password | the contents of the agent's token file (`demo/.run/tokens/explorer.token` in the demo) |

3. Click **Advanced**:
   - Set **MQTT Client ID** to `explorer`, the same as the username. This
     is the step people miss.
   - Leave the default subscriptions `#` and `$SYS/#` for an observer. For a
     non-observer identity, delete both and add instead:
     `a2a/v1/agents/+/presence`, `a2a/v1/events/#` and
     `a2a/v1/agents/<your-id>/inbox`.
   - Click **Back**, then **Save**, then **Connect**.

What you'll see in the topic tree:

```text
a2a/v1/
├── agents/
│   ├── coordinator/inbox      replies, hello-back notes
│   ├── coordinator/presence   retained card; "offline" after it exits
│   ├── worker/inbox           ping/reverse requests, hello notes
│   ├── worker/presence        retained card with capabilities [textstats]
│   └── explorer/inbox         replies to anything you send, and $broker errors
├── tasks/textstats            task requests (one per coordinator task)
└── events/worker/jobs/done    broadcast after each task
$SYS/broker/...                mochi-mqtt stats: clients, messages, uptime
$SYS/agentbus/status           agentbus's own snapshot (what `agentbus status` shows)
```

Run the coordinator again (`source demo/.run/coordinator.env &&
bin/coordinator`) and watch the tree fill in. Select a message and choose
JSON in the value pane for a readable envelope.

## MQTTX

1. Click **New Connection**:

   | Field | Value |
   |---|---|
   | Name | `agentbus demo` |
   | Client ID | `explorer`. Clear the random ID first. |
   | Host | `mqtt://` + `127.0.0.1` (`mqtts://` with TLS) |
   | Port | `18830` (demo) or the broker's port |
   | Username / Password | `explorer` / the token |
   | MQTT Version | 3.1.1 or 5.0. Both work. |

2. Click **Connect**, then **New Subscription** for each topic: `#` and
   `$SYS/#` for an observer, or the three contract filters listed under
   MQTT Explorer for a normal agent.

With MQTT 5.0, a publish the broker rejects also comes back with a reason
code in the PUBACK, for example *Not authorized* or *Payload format
invalid*. You get the `$broker` error envelope in your inbox either way.

## mosquitto CLI

```bash
brew install mosquitto
```

```bash
source demo/.run/explorer.env
mosquitto_sub -h 127.0.0.1 -p 18830 -i explorer -u explorer -P "$(cat "$AGENTBUS_TOKEN_FILE")" -t '#' -v
```

`-i` sets the client ID, which must match `-u`. The agentbus CLI does the
same from a shell without hand-written JSON: `bin/agentbus watch`,
`listen` and `agents` read the bus, and `request`, `task` and `send` build
the envelope for you.

## Sending messages by hand

The broker rejects any payload that isn't a valid envelope
([CONTRACT.md](../CONTRACT.md#envelope)).
**`ts` must be within 2 minutes of the broker's clock**, so a pasted example
with an old date is rejected. Get the current UTC time:

```bash
date -u +%Y-%m-%dT%H:%M:%SZ
```

Then, to ping the demo worker, publish this to topic
`a2a/v1/agents/worker/inbox`, with your timestamp in place of `<NOW>`:

```json
{"v":1,"id":"explorer-001","type":"request","from":"explorer","to":"worker","ts":"<NOW>","body":{"op":"ping"}}
```

The reply lands on `a2a/v1/agents/explorer/inbox`, with `correlation_id`
set to `explorer-001`:

```json
{"v":1,"id":"7aa2…","type":"response","from":"worker","to":"explorer","correlation_id":"explorer-001","body":{"op":"pong","worker":"worker", ...}}
```

Checklist for hand-written envelopes:

| Field | Rule |
|---|---|
| `id` | **Change it on every publish.** The broker silently drops a repeated `from` + `id` for 10 minutes, so resending the same payload does nothing. |
| `from` | Your agent ID. Anything else is rejected as spoofing. |
| `to` | Required for inbox topics, and must match the inbox owner. |
| `ts` | Required, RFC 3339 UTC, within 2 minutes of now. An old pasted timestamp gets an `invalid` error ("ts is ... from broker time"). |
| `reply_to` | Leave it out. Only your own inbox is accepted, and replies go there anyway. |
| retain | Leave it off. The broker ignores it on everything except presence. |
| `type` | `request` gets a reply in your inbox. `event` is one-way. |
| `capability` | Required for `a2a/v1/tasks/{capability}` and must match it, for example `"capability":"textstats"` with `"body":{"text":"..."}`. |

Other things to try against the demo worker:

```text
topic a2a/v1/agents/worker/inbox
{"v":1,"id":"explorer-002","type":"request","from":"explorer","to":"worker","ts":"<NOW>","body":{"op":"reverse","text":"hello"}}

topic a2a/v1/tasks/textstats
{"v":1,"id":"explorer-003","type":"request","from":"explorer","capability":"textstats","ts":"<NOW>","body":{"text":"to be or not to be"}}

topic a2a/v1/agents/worker/inbox      (expect a $broker error in your inbox: from is spoofed)
{"v":1,"id":"explorer-004","type":"event","from":"coordinator","to":"worker","ts":"<NOW>","body":{}}
```

## Troubleshooting

| Symptom | Cause |
|---|---|
| Connection refused, "not authorized" or "bad username or password" | Client ID differs from the username, the token is wrong or from an earlier demo run, or the ID isn't in the broker config. `demo/.run/broker.log` shows `auth rejected` with the reason. |
| Connects, but the `#` subscription fails and the tree stays empty | The identity isn't an observer. Add `"observer": true`, or subscribe to the contract filters instead. |
| A running agent drops off when you connect | You reused its client ID. Use your own identity. The broker log shows `agent session taken over` with the old and new addresses. |
| A publish vanishes with no reply | Check your own inbox topic for an `error` from `$broker`; its `code` and `reason` say what was wrong. The usual causes: `invalid` with "ts is ... from broker time" (refresh `ts`), `no_route` (the recipient is offline with no session) and `no_consumer` (no worker for that capability). No error and no reply usually means you reused an `id` (dedupe). The broker log has a `publish rejected` or `duplicate dropped` line either way. |
| The connection is refused right after you enable a "Last Will" | Wills aren't allowed: they would bypass the broker's checks. Turn the Will off. |
| Publishing to an agent's inbox is refused ("not allowed to send to") | The identity's `send_to` list doesn't include that agent. `send_to` limits inboxes only; anyone may publish tasks. |
| Messages show as QoS 0 | That's expected for observers. The broker grants them QoS 0 so they hold no in-flight state (this works around a mochi-mqtt packet ID quirk). |
