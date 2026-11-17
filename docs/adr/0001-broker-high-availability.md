# ADR-0001: Broker high availability, with Raft-replicated storage and one active broker

**Status:** Proposed
**Date:** 2026-11-16
**Deciders:** Jeff LaPlante

## Context

The broker is a single process. When it stops, the bus stops: no agent
can send, no task is routed, and requests in flight time out. With
`data_dir` set, the broker restarts with its sessions and queues intact,
but only after the process comes back on a host that can open the same
`broker.db`. In the [Kubernetes example](../../examples/kubernetes/agentbus.yaml)
that means a StatefulSet with one replica and a ReadWriteOnce volume. If
the node dies, the pod waits for the volume to detach before it can start
somewhere else, which can take minutes.

The goal is availability. The bus carries tens to hundreds of agents at
low message rates, and one broker handles that load. Nothing here is
about throughput.

### What the broker holds

| State | Where it lives | After a crash today |
|---|---|---|
| Agents, tokens, grants | config file, reloaded on change | Kept |
| Pending join requests | confstore directory beside the config | Kept if the disk survives |
| Sessions, subscriptions, queued QoS 1 messages, retained presence cards | `broker.db`, through mochi's bbolt storage hook ([store.go](../../broker/store.go)) | Kept if the disk survives |
| Dedupe window | memory; `dedupe.json` is written only on a clean shutdown | Lost |
| Open-request table | memory ([replies.go](../../broker/replies.go)) | Lost |
| Rate-limit buckets, last presence card per agent | memory | Lost; both rebuild on their own |
| Audit log | local JSONL file | Kept, per host |

Two of these matter for correctness after a crash:

- **Dedupe.** A sender that resends after the restart can have its
  message delivered twice. The client's own `seenIDs` catches most of
  these, but not all of them.
- **The open-request table.** It decides who may answer a request. A
  reply whose request isn't in the table falls back to the sender's
  `send_to`. So after a crash, a worker answering a task for an agent
  outside its `send_to` is rejected, and any agent inside the requester's
  reach can answer a request it was never sent. The "only the answerer,
  only once" rule lapses for every request opened before the crash.

### Why clustering is not just more replicas

Every contract check assumes the broker can see every client.
`route` ([hook.go](../../broker/hook.go)) refuses an inbox publish when
the recipient has no session on this broker, and refuses it when the
recipient's queue is full. `OnSelectSubscribers` picks a task worker
from this broker's own subscriber list, preferring connected ones. A
second broker running beside the first would see half the clients and
reject messages to the other half.

Mochi has no clustering of its own.

## Decision

Run three broker nodes with embedded Raft (`hashicorp/raft`). One node,
the Raft leader, is the active broker. It is the only node that accepts
MQTT connections. Broker state goes through Raft, so every node holds a
current copy. When the leader fails, another node wins the election,
loads its copy the way `Start` already loads `broker.db`, and opens its
listener. Clients reconnect to it.

Each accepted message is written to Raft in `OnPublish`, before mochi
sends the PUBACK. Mochi's storage hooks can't do this job: they run
after the PUBACK and can't refuse a write. See
[When the message is stored](#when-the-message-is-stored).

The dedupe window, the open-request table, the config and pending join
requests move into the same replicated state, so none of them is lost
on failover.

## Options considered

### Option A: Several active brokers sharing an external store

The approach of [comqtt](https://github.com/wind-c/comqtt), a mochi
fork: gossip for membership, Raft for cluster metadata, Redis for
sessions, in-flight messages and retained messages, and gRPC to forward
messages between nodes.

| Dimension | Assessment |
|---|---|
| Complexity | High |
| Operations | A Redis or Valkey deployment beside the broker |
| Scalability | Scales out across nodes |
| Fit with the contract | Poor without a rewrite of routing and worker selection |

**Pros:** Load spreads across nodes. A node failure affects only the
agents connected to it.

**Cons:** Needs a cluster-wide table of which agent is on which node,
forwarding between nodes, worker selection across nodes, and session
migration when an agent reconnects to a different node. Redis
replication is asynchronous, so a failover can lose writes the broker
already acknowledged. Adds a database server, which
[RETENTION.md](../RETENTION.md) avoids on purpose. Buys throughput the
bus doesn't need.

### Option B: Replace mochi with NATS JetStream, EMQX or VerneMQ

NATS serves MQTT with sessions and retained messages stored in
JetStream, which is replicated with Raft. EMQX and VerneMQ cluster
natively.

| Dimension | Assessment |
|---|---|
| Complexity | High: a rewrite of the broker |
| Operations | A separate server instead of one binary |
| Scalability | High |
| Fit with the contract | Poor |

**Pros:** Clustering is built, tested and maintained by someone else.

**Cons:** The a2a/v1 contract lives in mochi hooks: `OnPublish`
re-encodes envelopes and sends error envelopes, `OnSelectSubscribers`
picks workers, and the open-request table authorizes replies. NATS has
no inline hook on each publish where that logic could run. EMQX has
ExHook over gRPC, which adds a network call to every message. NATS'
support for MQTT shared subscriptions, which task queues depend on, is
not clearly documented. Either way, agentbus stops being one binary.

### Option C: One active broker, Raft-replicated storage (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Medium |
| Operations | Three pods instead of one; no other services |
| Scalability | Same as today: one node carries all traffic |
| Fit with the contract | Good: hooks and routing don't change |

**Pros:** Routing, worker selection and reply authorization stay
single-node, so those checks don't change. Sessions and subscriptions
can still be replicated through the `boltStore` wrapper around mochi's
storage hook. Failover measured 1.1–2.4 seconds with default Raft
timeouts, plus client reconnect. Three nodes survive one failure. It
stays one binary with no database server.

**Cons:** Every stored message becomes a quorum write. Queued messages
move out of mochi's storage hook and into agentbus' own replicated
state, which is new code. Two of the three nodes are idle. See
[Research findings](#research-findings).

### Option D: Keep one replica, recover faster

Keep a single broker and shorten recovery: replicated block storage, a
shorter volume-detach timeout, or a file copy of `broker.db` shipped to
a standby.

| Dimension | Assessment |
|---|---|
| Complexity | Low |
| Operations | Depends on the storage class |
| Scalability | Same as today |
| Fit with the contract | No change |

**Pros:** No code change in the broker.

**Cons:** Recovery is still measured in tens of seconds to minutes. A
copy of `broker.db` is only as current as the last copy, and bbolt
can't ship changes incrementally. Doesn't fix the dedupe or
open-request loss.

## Trade-off analysis

Options A and B solve a scaling problem the bus doesn't have, and both
need the contract enforcement rewritten. Option D is cheap but leaves
recovery slow and leaves the correctness gaps in place. Option C keeps
the contract checks as they are and changes how state is stored.

If one node does run out of capacity later, partitioning by agent fits
the contract. Every check is keyed by the sender (rate limit, dedupe)
or by the recipient (route, queue limit, reply claim). A reply always
goes to the requester's inbox, so it lands on the node that holds the
request's claim. That makes Option A tractable later on top of
Option C. Session migration between nodes is still the hard part.

## Research findings

The first draft of this ADR left four questions open. These are the
answers, from mochi v2.7.9's source (still the latest release, from
March 2025), `hashicorp/raft` v1.7.3, and measurements on a MacBook.

### When the message is stored

Mochi sends the PUBACK before it stores the message for the recipient.
In `processPublish` the order for a QoS 1 publish is:

1. `OnPublish`, which is where `contractHook` runs its checks.
2. Store a record of the PUBACK in the sender's in-flight table
   (`OnQosPublish`), send the PUBACK, then delete that record
   (`OnQosComplete`).
3. `publishToSubscribers`, which stores the message in the recipient's
   in-flight table (`OnQosPublish`) and sends it.

The recipient's PUBACK later deletes its record (`OnQosComplete`), so
each delivered message costs four bbolt transactions, each with its own
fsync.

This has two consequences:

- **The storage hook can't make a message durable before the PUBACK.**
  It runs after the PUBACK, and its methods have no error return, so a
  failed write can't stop the PUBACK or the delivery. The only hook that
  runs before the PUBACK and can refuse a message is `OnPublish`. The
  replicated write for a message has to happen there, and agentbus has
  to keep the queued-message record itself instead of relying on
  mochi's in-flight records. The other choice is to patch mochi so it
  stores before it acknowledges. That means carrying a fork.
- **The same gap exists today on one node.** A crash between the
  PUBACK and the recipient's write loses a message the sender was told
  was delivered. The window is one fsync wide.

The sender-side record in step 2 is written and deleted inside the same
call, around the PUBACK. It only matters for QoS 2, which the broker
turns off, so the `boltStore` wrapper can skip it. That removes two of
the four writes per message, with or without Raft.

### What a replicated write costs

Two measurements, both on a MacBook with its internal SSD. On macOS,
Go's `fsync` uses `F_FULLFSYNC`, which is much slower than `fdatasync`
on a typical Linux server, so these are pessimistic. Linux numbers have
not been measured.

The broker as it is today, alice sending 600 inbox messages to bob:

| `data_dir` | Senders | Messages/s | Per message |
|---|---|---|---|
| off | 1 | 14,848 | 0.07 ms |
| off | 8 | 35,796 | 0.03 ms |
| on | 1 | 27 | 36 ms |
| on | 8 | 28 | 36 ms |

With `data_dir` on, the four fsyncs per message cap the whole broker at
about 28 messages a second on this machine. More senders don't help,
because bbolt runs one write transaction at a time.

A three-node `hashicorp/raft` cluster on loopback, each node with a
`raft-boltdb` log and a bbolt state machine, writing 512-byte entries.
The state machine doesn't fsync, since the Raft log is the durable copy
and is replayed after a crash:

| Setup | Concurrent writers | p50 | p99 | Writes/s |
|---|---|---|---|---|
| bbolt alone (today's storage hook) | 1 | 8.0 ms | 10.7 ms | 121 |
| bbolt alone | 16 | 131 ms | 149 ms | 121 |
| Raft, 3 nodes | 1 | 22.0 ms | 33.4 ms | 45 |
| Raft, 3 nodes | 16 | 79.2 ms | 115 ms | 198 |
| Raft, 3 nodes, state machine fsyncs too | 1 | 42.8 ms | 58.3 ms | 24 |

All three Raft nodes shared one disk in this test. On separate hosts
each node fsyncs its own disk, and a write costs about two fsyncs plus a
network round trip. Raft batches concurrent writes into one fsync, which
is why 16 writers get more writes a second than bbolt alone.

The broker today spends four fsyncs on each message (36 ms here). The
proposed design spends one synchronous Raft write on each message, in
`OnPublish`, and replicates the recipient's acknowledgement afterwards
without the publisher waiting for it. Losing an acknowledgement means
the message is delivered again after a failover, and the client's
`seenIDs` drops the duplicate. On this machine that is about 22 ms per
message, against today's 36 ms. Replication does not make the broker
slower than it is today with `data_dir` on.

Leader failover took 1.1 to 2.4 seconds across runs with the default
timeouts (1 s heartbeat, 1 s election).

### A leader cut off from the others

`hashicorp/raft` handles most of this:

- A leader that can't reach a quorum steps down after
  `LeaderLeaseTimeout` (500 ms by default).
- A write on a node that isn't the leader fails with `ErrNotLeader`. A
  write in progress when leadership is lost fails with
  `ErrLeadershipLost`.
- `LeaderCh` reports every change of leadership.

What agentbus has to do:

- **When leadership is lost,** stop accepting connections and
  disconnect every client. Both pieces exist: `guardedListener` has
  `stopAccepting` and `open`, and `closeListenerClients` disconnects
  everyone on a listener.
- **When the Raft write in `OnPublish` fails,** disconnect the sender
  without a PUBACK. Rejecting the message is wrong here: the broker acks
  a rejected publish, and a QoS 1 sender doesn't resend a message that
  was acked. Without a PUBACK, paho resends the message after it
  reconnects to the new leader, and dedupe drops it if the old leader
  had in fact committed it.
- **When leadership is gained,** load the replicated state, then open
  the listener, the same order `Start` uses.

MQTT 5 has a "use another server" reason code with a server reference.
paho.mqtt.golang speaks MQTT 3.1.1 and ignores it, so clients find the
new leader by trying their list of broker addresses.

### Config and join approvals

Today the config is a file on the broker's own volume. `agentbus config`
edits it in place, the broker watches it, and `/join` writes pending
requests as files beside it. In the Kubernetes example an init container
creates it on first start.

Two choices were considered:

- **A ConfigMap mounted into every pod.** Operators edit the ConfigMap.
  The kubelet updates mounted files with a delay of up to a minute or
  more, so a follower that becomes leader can briefly serve the old
  config. A removed agent or a rotated token would still work there
  until the update arrives. Pending join requests still need somewhere
  writable.
- **The config through Raft (chosen).** The leader's config watcher
  sends each change through Raft, and every node writes the committed
  config to its own file. `agentbus config` keeps working as it does,
  pointed at the leader pod. A follower whose file differs from the
  committed config logs a warning and rewrites it. Pending join
  requests go through Raft the same way.

Pending requests have to survive failover either way. A client polling
`/join/status` that gets `unknown` reports that its request "was denied,
or expired unanswered" and stops, so losing pending requests on
failover gives the requester a wrong answer.

## What this costs

- **New queue code.** Queued messages are kept by agentbus in
  replicated state, not by mochi's storage hook. Redelivery after
  failover has to be rebuilt from that state.
- **Two idle nodes.** Three pods and three volumes for the capacity of
  one.
- **Bootstrap and membership.** Raft needs a known initial membership
  and a way to replace a dead node. On Kubernetes, StatefulSet ordinal
  names give stable peer addresses.
- **Config edits go to the leader.** An edit made on a follower's file
  is overwritten.

## Consequences

- A node failure costs about an election timeout plus reconnect, not a
  pod reschedule and volume detach.
- Dedupe and reply authorization survive both failover and an ordinary
  crash.
- The Kubernetes example goes from one replica to three, each with its
  own volume. The Service routes only to the leader, through `/readyz`.
- Clients need more than one broker address, so a client whose broker
  goes away can try the others.
- Broker tests need a three-node harness, including a partitioned
  leader.
- Audit logs stay per node. Reading the full history means reading all
  three files.

## Action items

1. [ ] **Client: accept several broker URLs.** `client.Options` takes one
   `BrokerURL` ([client.go](../../client/client.go)). paho accepts
   several and tries them in turn. This helps with any failover setup,
   so it goes first.
2. [ ] **Skip the sender-side PUBACK record in `boltStore`.** Halves the
   fsyncs per message on a single node today. Independent of the rest.
3. [x] **Benchmark Raft writes and check mochi's PUBACK ordering.** See
   [Research findings](#research-findings).
4. [ ] **Measure on Linux.** Run `make bench`
   ([BENCHMARKS.md](../BENCHMARKS.md)) on a Linux host with the kind of
   disk the broker will run on, and repeat the Raft test across three
   separate hosts, before relying on the numbers above.
5. [ ] **Keep the queued-message record in `OnPublish`, on one node
   first.** Store the accepted message before mochi sends the PUBACK,
   and rebuild queues from that record on start. This closes today's
   gap between PUBACK and storage, and it is the record Raft will
   replicate.
6. [ ] **Move dedupe and the open-request table into the same stored
   state.** Both are lost on a crash today.
7. [ ] **Raft: replicate that state, the config and pending joins;
   leader-only listener; readiness gated on leadership.** Followers
   refuse CONNECT. A deposed leader stops accepting and disconnects its
   clients, and a failed write in `OnPublish` disconnects the sender
   without a PUBACK.
8. [ ] **Update the Kubernetes example and
   [CONTAINERS.md](../CONTAINERS.md)** for three replicas.
9. [ ] **Revisit active/active** only if one node runs out of capacity.

## References

- [comqtt cluster README](https://github.com/wind-c/comqtt/blob/main/cluster/README.md)
- [NATS: QoS, sessions, and retained messages](https://docs.nats.io/learn/mqtt/qos-sessions-and-retained)
- [NATS MQTT configuration](https://docs.nats.io/running-a-nats-service/configuration/mqtt)
- [hashicorp/raft](https://github.com/hashicorp/raft)
