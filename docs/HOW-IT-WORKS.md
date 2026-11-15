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
OnPublish(client, packet)
  ref = peek(payload)                  # id + type, for a correlated error
  authorizeWrite(agent, topic)         # own presence/events; inbox allowed by send_to
  rate.allow(agent)                    # token bucket per agent
  if presence topic
    validate card, agent == client
    stamp status=online, ts=now, force retain
    remember card                      # reused for the offline card
    deliver
  size <= max_envelope_bytes
  Parse + Validate envelope
  ValidateFor(topic, agent)            # from == agent, to == inbox owner,
                                       # tasks are requests, events are events
  not expired (ts + ttl_ms)
  dedupe(from/id)  -> duplicate: ack, drop silently
  log one line (no body), deliver

on any failure: reject()
  ack and drop                         # a QoS 1 sender won't retry
  send an error envelope from $broker to the sender's inbox,
  with correlation_id = the rejected id
```

Reads are checked in `OnACLCheck`, at subscribe time and again for each
delivery. Observer identities pass every read check except shared
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

## Presence

```text
connect      -> client publishes its card (retained) -> broker stamps it online
ServeTasks   -> client republishes the card with the new capability
disconnect   -> broker republishes the stored card with status=offline
                (skipped if a new connection took over the same agent ID)
```

`client.Agents()` and `agentbus agents` read these retained cards.
