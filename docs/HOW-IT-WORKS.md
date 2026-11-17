# How agentbus works

This page describes how the broker, the client and an exchange between
two agents fit together. [CONTRACT.md](../CONTRACT.md) is the normative
spec. This page explains the mechanics behind it.

Terms used below:

- An agent is any program that connects to the broker with an agent ID
  and a token.
- An envelope is the JSON object every message is wrapped in: `id`,
  `type`, `from`, `to`, `ts`, `body` and a few optional fields.
- A capability is a named kind of work, such as `textstats`. Each
  capability has one task queue.
- a2a/v1 is the name of the contract: the topics, the envelope and the
  rules the broker enforces.

## Layers

```text
 agents (conduit, demo/cmd/worker, demo/cmd/coordinator, agentbus CLI, agent runtime)
        |  MQTT 3.1.1 or 5; username = client ID = agent ID, password = token
        v
+--------------------------------------------------------------+
| agentbus broker (broker/)                                    |
|  hook.go   contractHook: the a2a/v1 rules                    |
|    OnConnectAuthenticate  OnACLCheck (reads)  OnPublish      |
|    OnSessionEstablished   OnDisconnect                       |
|  limits.go rateLimiter, dedupe       config.go  agents, limits|
|--------------------------------------------------------------|
| mochi-mqtt: sessions, QoS, retained, $share, topic routing   |
+--------------------------------------------------------------+
```

[mochi-mqtt](https://github.com/mochi-mqtt/server) is an embeddable Go
MQTT broker. It handles the MQTT protocol: sessions, QoS, retained
messages and topic routing. agentbus adds one hook, `contractHook`, which
checks every connection, subscription and publish against the a2a/v1
rules. The agents on the top line are examples of clients; conduit is the
AI agent agentbus was first written for.

## Topics

```text
a2a/v1/agents/{id}/inbox       direct messages and replies   read: owner only
a2a/v1/agents/{id}/presence    retained presence card        write: owner only
a2a/v1/tasks/{capability}      work queue ($share/{cap}/...) read: granted agents
a2a/v1/events/{id}/{name...}   broadcasts                    write: owner only
```

`{id}` is an agent ID, and the owner is the agent with that ID. The
right-hand column is the restriction the broker enforces on each topic.
`$share/{cap}/...` is an MQTT shared subscription: the broker delivers
each message to one subscriber in the group, so each task goes to one
worker.

## Every publish goes through OnPublish

The checks run in the order shown. The first one that fails rejects the
publish, and the steps under "on any failure" run instead of the rest.

```text
OnConnect                              # refuse a CONNECT that carries a Will
OnConnectAuthenticate                  # client ID == username == agent; token hash

OnPublish(client, packet)
  st = state()                         # one config snapshot for the whole decision
  rate.allow(agent)                    # first, before any parsing
  authorizeWrite(agent, topic)         # own presence/events; inbox allowed by send_to,
                                       #   or (after parsing) one reply to an open request
  strip MQTT 5 properties
  if presence topic
    validate card, agent == client
    drop ungranted capabilities; check keys against pinned keys
    stamp status=online, ts=now, force retain
    remember card                      # reused for offline and 24h refresh
    deliver
  clear retain                         # only presence is retained
  size <= max_envelope_bytes
  Parse + Validate envelope
  ValidateFor(topic, agent)            # from == agent, to == inbox owner,
                                       # reply_to == own inbox, task/event types
  ts within max_clock_skew; not expired
  route: recipient configured, has a session, queue not full; task has a worker
  dedupe(from/id)  -> duplicate: ack, drop silently
  if reply: claim the open request     # atomic: only its answerer, only once
  payload = re-encoded envelope        # receivers see exactly what was checked
  if request: open an entry            # for reply checks and request metrics
  log one line (action=accept, no body), deliver

on any failure: reject(code)
  ack and drop                         # a QoS 1 sender won't retry
  throttled per agent (the payload is only peeked at here):
    log action=reject with the code
    send an error envelope from $broker to the sender's inbox,
    with correlation_id = the rejected id
```

Reads are checked in `OnACLCheck`, at subscribe time and again for each
delivery. `OnSubscribe` adds two rules the ACL can't express: task queues
only through the exact `$share/{cap}/...` filter, and at most 256
subscriptions per client.

Observers are identities with `"observer": true` in the config. They pass
every read check except shared subscriptions, and `OnSubscribe` grants
them QoS 0. This is how MQTT Explorer can show the whole bus
([DESKTOP-CLIENTS.md](DESKTOP-CLIENTS.md)).

Writes are authorized in `OnPublish` and not in the ACL hook. mochi
disconnects an MQTT 3.1.1 client whose QoS 1 publish fails the ACL, and
the client would then resend that message on every reconnect.

## Request and reply

What the `ping` step of the demo (`make demo`) does. The coordinator
sends a request to the worker's inbox, and the worker's reply comes back
to the coordinator's inbox carrying the request's ID as `correlation_id`:

```text
coordinator                   broker                        worker
  | Request(worker, ping)       |                             |
  | PUB agents/worker/inbox     |                             |
  | {type:request, id:X}        |                             |
  |---------------------------->| checks pass                 |
  |                             |---------------------------->| Inbox handler
  |                             |  PUB agents/coordinator/    |
  |                             |<--- inbox {type:response,   |
  |                             |      correlation_id:X}      |
  |<----------------------------|                             |
  | client matches X to the     |                             |
  | pending Request; returns    |                             |
```

If any check fails, the coordinator instead gets
`{type:error, from:$broker, correlation_id:X}`, and `Request` returns a
`*client.RemoteError` right away rather than timing out. The handler's
context carries a deadline taken from the request's `ttl_ms`.

## Task queues

What the demo's `tasks` step does, here with two workers. Both workers
subscribe with the same `$share` filter. The coordinator publishes to the
capability's topic without naming a worker, and the broker picks one
connected worker for each task:

```text
coordinator               broker                    worker      worker-2
  |                         | <- SUB $share/textstats/a2a/v1/tasks/textstats
  |                         | <- SUB $share/textstats/a2a/v1/tasks/textstats
  | RequestTask x5          |                          |            |
  |------------------------>| each task to ONE member  |            |
  |                         |------------------------->|            |
  |                         |-------------------------------------->|
  |                         | responses -> coordinator's inbox      |
  |<------------------------|                          |            |
  |                         | PUB events/worker/jobs/done (event)   |
  |<------------------------| (coordinator watches events/+/jobs/#) |
```

The broker grants capabilities per agent (`capabilities` in the config).
An agent without the grant can send tasks but can't consume them. The
demo's `acl` step checks this.

## A model-backed agent

An `llm:` handler in a standalone agent (see
[AGENTS.md](AGENTS.md#model-backed-handlers)) turns one request into a
conversation with a model. The broker sees only the bus messages: the
request, any calls the model makes to other agents, and the reply. In the
diagram, "fenced" means the other agent's reply is passed to the model
behind the untrusted-input preamble, which marks it as data and not
instructions.

```text
caller          broker          hostdoc (agent runtime)          model API
  | task          |                 |                                |
  |-------------->|---------------->| llm handler                    |
  |               |                 |--- system + envelope --------->|
  |               |                 |<---------- tool call: disk {} -|
  |               |                 | runs disk as the caller        |
  |               |                 |--- its output ---------------->|
  |               |                 |<-------- tool call: bus_task --|
  |               |<----------------| PUB tasks/weather.forecast     |
  |               |   to weather,   |   headers {hops: "1"}          |
  |               |   and its reply |                                |
  |               |---------------->|--- the reply, fenced --------->|
  |               |                 |<-------------- final text -----|
  |<--------------|<----------------| response                       |
```

- **The model API is not on the bus.** The agent calls it over HTTPS with
  its own key. The broker never sees prompts or model output, only the
  envelopes.
- **Tool calls to the agent's own handlers stay in the process.** They
  run the handler directly, as a request from the original sender. Only
  `bus_request` and `bus_task` publish, and those go through the
  broker's usual checks under the agent's own identity.
- **The `hops` header** is one more than the request being handled. At
  `max_hops` the runtime refuses to publish, so agents that ask agents
  can't loop (see [CONTRACT.md](../CONTRACT.md#headers)).
- **The whole run fits in the request's deadline.** Model calls, tool
  calls and bus calls all share the handler's context. When it ends, the
  caller gets `handler timed out` instead of waiting for its own TTL.

## Retention

With `data_dir` set, mochi's bbolt storage hook saves sessions,
subscriptions, queued messages and retained presence cards to
`broker.db`. The broker loads that file before it accepts connections, so
no agent connects to a half-restored broker:

```text
Start
  server.Serve()           # load broker.db: sessions, subscriptions, queues, retained
  restorePresence()        # restored cards -> offline (nobody is connected yet)
  listener.open()          # only now accept connections (they waited in the backlog)
Close
  stop accepting; server.Close()   # closes broker.db
  save dedupe.json; close the audit file
```

See [RETENTION.md](RETENTION.md) for what is kept and the audit log format.

## Config reload

```text
serve
  watch the config file (every --watch, default 1s) and SIGHUP
    LoadConfig + Validate          # bad file: keep running config, report the error
    Reload
      agents, rates, clock skew    -> new hookState, swapped in atomically
      listen, TLS, storage, limits -> restart_needed (kept as they are)
      removed agents, rotated tokens -> DisconnectClient
      removed agents               -> card cleared, session and queue dropped
      changed agents               -> card re-stamped (grants, pinned keys)
  OnSessionEstablished re-checks every login against the current config,
  so a login that raced the reload can't keep a removed or rotated identity
```

Every check reads one immutable snapshot (`hookState`), so a reload
never leaves a check half on the old config and half on the new. Revoked
capabilities take effect at the next task: shared-subscription members
without the grant are pruned before a worker is picked, so a stale stored
subscription can't receive tasks.

## Request tracking and metrics

The broker keeps a table of open requests: one entry per accepted request,
removed when the reply arrives or the request expires.

```text
request accepted   -> open entry {requester, id, answerer, start, expiry}
                      256 per requester (own oldest evicted), 65,536 total
reply accepted     -> claim the entry: answerer only, once
                      outcome answered or errored; latency into a histogram
every second       -> sweep expired entries: outcome expired, audit "expire"
```

The same table enforces who may reply and records what happened to each
request, per target (`agent:<id>` or `task:<capability>`). `status`
shows it under `requests`. With `metrics_listen` set, the broker also
serves it over HTTP:

```text
GET /healthz   200 while serving
GET /readyz    200 once accepting connections and the last reload succeeded
GET /metrics   Prometheus text: agentbus_requests_open, agentbus_requests_total
               {outcome=answered|errored|expired|evicted},
               agentbus_request_duration_seconds (histogram),
               per-agent online/queued/accepted/rejected, rejections by code,
               auth failures, reloads, MQTT traffic
```

Label values come only from the config (agent IDs and capabilities) and
fixed code lists, never from message content, so a sender can't create
new series.

## Broker status

The broker publishes a snapshot of its own state as a retained message:

```text
every status_interval (default 2s)
  Broker.Status()                   # mochi traffic counters + the hook's decision counters
                                    # + config, limits, and every agent's state and queue
  publish retained -> $SYS/agentbus/status
                                    # readable only by observers (OnACLCheck)
                                    # not written to broker.db
```

`agentbus console` and `agentbus status --plain` subscribe to it with
`client.Observe`. See [CLI.md](CLI.md).

## Presence

A presence card is a retained JSON message that gives an agent's ID,
status, capabilities and description. The agent publishes it, and the
broker sets the status:

```text
connect      -> client publishes its card (retained) -> broker stamps it online
ServeTasks   -> client republishes the card with the new capability
disconnect   -> broker republishes the stored card with status=offline
                (skipped if a new connection took over the same agent ID)
```

`client.Agents()` and `agentbus agents` read these retained cards.
