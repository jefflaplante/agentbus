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
MQTT connections. Its storage writes go through Raft, so every node
holds a current copy of `broker.db`. When the leader fails, another node
wins the election, loads its copy the way `Start` already loads
`broker.db`, and opens its listener. Clients reconnect to it.

The dedupe window and the open-request table move into the same
replicated state, so neither is lost on failover.

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

**Pros:** The seam already exists. `boltStore` wraps mochi's storage
hook, so its writes can be sent through Raft instead of straight to
bbolt. Routing, worker selection and reply authorization stay
single-node, so the hooks don't change. Failover is an election timeout
(about 1–2 seconds with default settings) plus client reconnect. Three
nodes survive one failure. It stays one binary with no database server.

**Cons:** Every storage write becomes a quorum write. See
[What this costs](#what-this-costs). Two of the three nodes are idle.

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
the contract code as it is and changes only how state is stored, and
the storage hook is where mochi already expects that change to happen.

If one node does run out of capacity later, partitioning by agent fits
the contract. Every check is keyed by the sender (rate limit, dedupe)
or by the recipient (route, queue limit, reply claim). A reply always
goes to the requester's inbox, so it lands on the node that holds the
request's claim. That makes Option A tractable later on top of
Option C. Session migration between nodes is still the hard part.

## What this costs

- **A quorum write per stored change.** Each queued QoS 1 message, each
  acknowledgement, each subscription and each presence card becomes a
  Raft log entry. On a LAN that's a round trip to one follower plus an
  fsync. This has not been measured for agentbus. A benchmark of the
  replicated hook comes before anything else is built.
- **Ordering against PUBACK.** A message is only safe from failover if
  its replicated write commits before the sender gets its PUBACK.
  Whether mochi calls the storage hook before or after it sends the
  PUBACK has to be checked in mochi's code. If it's after, a message
  acknowledged in the last moments before the leader dies can be lost.
- **A deposed leader.** A leader cut off from the other two must stop
  accepting work. Its Raft writes will fail, so it can reject publishes,
  but it also has to disconnect its clients so they find the new leader.
  This is a code path that needs its own tests.
- **Config and join approvals.** Today an approval writes the config
  file on the broker's host. With three nodes, either the config comes
  from a shared source (a ConfigMap) and approvals go there, or the
  config also goes through Raft. This decision doesn't settle which.
- **Two idle nodes.** Three pods and three volumes for the capacity of
  one.
- **Bootstrap and membership.** Raft needs a known initial membership
  and a way to replace a dead node. On Kubernetes, StatefulSet ordinal
  names give stable peer addresses.

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
2. [ ] **Benchmark a Raft-backed storage hook** under realistic agent
   traffic, and check whether mochi stores in-flight messages before or
   after it sends the PUBACK.
3. [ ] **Move dedupe and the open-request table into the replicated
   state.** Persisting them is worth doing on a single node too, since
   both are lost on a crash today.
4. [ ] **Raft storage hook, leader-only listener, and readiness gated on
   leadership.** Followers refuse CONNECT. A deposed leader disconnects
   its clients.
5. [ ] **Decide where config and join approvals live** in a three-node
   setup.
6. [ ] **Update the Kubernetes example and
   [CONTAINERS.md](../CONTAINERS.md)** for three replicas.
7. [ ] **Revisit active/active** only if one node runs out of capacity.

## References

- [comqtt cluster README](https://github.com/wind-c/comqtt/blob/main/cluster/README.md)
- [NATS: QoS, sessions, and retained messages](https://docs.nats.io/learn/mqtt/qos-sessions-and-retained)
- [NATS MQTT configuration](https://docs.nats.io/running-a-nats-service/configuration/mqtt)
- [hashicorp/raft](https://github.com/hashicorp/raft)
