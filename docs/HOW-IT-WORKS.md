# How agentbus works

How the broker, the client, and an exchange between two agents fit
together. [CONTRACT.md](../CONTRACT.md) is the normative spec. This page
explains the mechanics.

## Layers

```text
 agents (conduit, demo/cmd/worker, demo/cmd/coordinator, agentbus CLI)
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

mochi-mqtt does all the protocol work. agentbus adds one hook that turns a
generic MQTT broker into one that knows about agents.

## Topics

```text
a2a/v1/agents/{id}/inbox       direct messages and replies   read: owner only
a2a/v1/agents/{id}/presence    retained presence card        write: owner only
a2a/v1/tasks/{capability}      work queue ($share/{cap}/...) read: granted agents
a2a/v1/events/{id}/{name...}   broadcasts                    write: owner only
```

## Every publish goes through OnPublish

```text
OnConnect                              # refuse a CONNECT that carries a Will
OnConnectAuthenticate                  # client ID == username == agent; token hash

OnPublish(client, packet)
  ref = peek(payload)                  # id + type, for a correlated error
  rate.allow(agent)                    # first, so rejected publishes cost tokens too
  authorizeWrite(agent, topic)         # own presence/events; inbox allowed by send_to
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
  route: recipient configured and has a session; task has a worker
  dedupe(from/id)  -> duplicate: ack, drop silently
  payload = re-encoded envelope        # receivers see exactly what was checked
  log one line (action=accept, no body), deliver

on any failure: reject(code)
  ack and drop                         # a QoS 1 sender won't retry
  throttled per agent:
    log action=reject with the code
    send an error envelope from $broker to the sender's inbox,
    with correlation_id = the rejected id
```

Reads are checked in `OnACLCheck`, at subscribe time and again for each
delivery. `OnSubscribe` adds two rules the ACL can't express: task queues
only through the exact `$share/{cap}/...` filter, and at most 256
subscriptions per client. Observer identities pass every read check except shared
subscriptions, and `OnSubscribe` grants them QoS 0. That's how MQTT
Explorer can show the whole bus
([DESKTOP-CLIENTS.md](DESKTOP-CLIENTS.md)). Writes are authorized in `OnPublish` rather than the ACL hook,
because mochi disconnects an MQTT 3.1.1 client whose QoS 1 publish fails the
ACL, and the client would resend that message on every reconnect.

## Request and reply

What the demo's `ping` step does:

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

What the demo's `tasks` step does, here with two workers:

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

The broker grants a capability per agent (`capabilities` in the config).
An agent without the grant can send tasks but can't consume them, which is
what the demo's `acl` step checks.

## Retention

With `data_dir` set, mochi's bbolt storage hook saves sessions,
subscriptions, queued messages and retained presence cards. On startup:

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

## Broker status

```text
every status_interval (default 2s)
  Broker.Status()                   # mochi traffic counters + the hook's decision counters
                                    # + config, limits, and every agent's state and queue
  publish retained -> $SYS/agentbus/status
                                    # readable only by observers (OnACLCheck)
                                    # not written to broker.db
```

`agentbus status` and `agentbus dashboard` subscribe to it with
`client.Observe`. See [CLI.md](CLI.md).

## Presence

```text
connect      -> client publishes its card (retained) -> broker stamps it online
ServeTasks   -> client republishes the card with the new capability
disconnect   -> broker republishes the stored card with status=offline
                (skipped if a new connection took over the same agent ID)
```

`client.Agents()` and `agentbus agents` read these retained cards.
