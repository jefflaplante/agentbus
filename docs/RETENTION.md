# Retention and audit

Two optional settings, both embedded and both pure Go. Neither needs a
database server:

```json
{
  "data_dir": "/var/lib/agentbus",
  "audit_file": "/var/log/agentbus/audit.jsonl"
}
```

## `data_dir`: broker state survives restarts

Without `data_dir`, everything is in memory and a restart starts from
nothing. With it, the broker keeps:

| State | Where | After a restart |
|---|---|---|
| Persistent sessions (agents connecting with clean session off) | `broker.db` (bbolt) | Restored; the agent resumes its session when it reconnects |
| Subscriptions, including workers' task-queue subscriptions | `broker.db` | Restored, so tasks for a capability still route to its workers |
| Queued messages for offline agents (QoS 1 in flight) | `broker.db` | Delivered when the agent reconnects |
| Presence cards | `broker.db` (retained messages) | Restored and **marked offline**, since nobody is connected yet. Each agent's card goes back online when it reconnects. |
| Dedupe window (recent `from`+`id` pairs) | `dedupe.json`, written on shutdown | Reloaded, so a message resent after the restart isn't delivered twice |

The directory is created with mode 0700 and the files with mode 0600. Only
one broker can use a `data_dir` at a time; a second fails at startup
("is another broker using this data_dir?").

**Limits still apply.** Each session holds at most `max_queued` messages
(default 1024). A session nobody reconnects to is dropped after
`session_expiry` (default 7 days), along with its queue.

**What isn't kept.**
- Sessions of agents that connect with clean session on (the CLI's one-shot
  commands do). That's what clean session means.
- Rate-limit buckets.
- Dedupe entries from a crash. `dedupe.json` is only written on a clean
  shutdown. The clients' own dedupe catches most redeliveries anyway.

**Workers must register task handlers at connect time.** When a worker
with a persistent session reconnects, the broker immediately redelivers the
tasks queued for it. In Go, pass handlers in `client.Options.Tasks`, not
`ServeTasks` after connecting: the late call would miss those tasks, and
they'd be answered `no_handler`. `agentbus handle --capability` and the
demo worker already do this.

**Tasks go to connected workers.** A worker that disconnects keeps its
place in the task queue's share group (its session persists), but the
broker sends each task to a connected member. Tasks only queue for an
offline worker when every worker for that capability is offline.

### How it works

The broker uses mochi-mqtt's bbolt storage hook, with one fix. In mochi
v2.7.9 the hook doesn't save the packet ID of queued messages, so after a
restart they can't be redelivered: the connection drops with "missing
packet id" on every reconnect. `broker/store.go` restores the ID from each
record's storage key. If mochi fixes this upstream, the wrapper can go.

At startup the broker loads the store, marks restored presence cards
offline, and only then starts accepting connections. Connections arriving
in the meantime wait in the TCP backlog.

### Backups

`broker.db` is a single bbolt file. Copy it while the broker is stopped, or
use any bbolt-aware backup tool while it runs. Deleting `data_dir` resets
the broker to an empty state.

## `audit_file`: a JSONL decision log

One JSON object per line, for every decision the broker makes:

| `action` | When |
|---|---|
| `connect`, `disconnect` | An agent session starts or ends |
| `refused` | A connection was refused: bad login (throttled per IP) or a Will message |
| `presence` | An agent published its presence card |
| `accept` | An envelope was delivered |
| `duplicate` | An envelope was dropped as a repeat |
| `reject` | An envelope was refused, with `code` and `reason` (throttled per agent, like the error envelopes) |

```json
{"ts":"2026-11-15T20:41:13.6Z","action":"accept","agent":"coordinator","topic":"a2a/v1/agents/worker/inbox","id":"7aa2…","type":"request","from":"coordinator","to":"worker","priority":"high","event":"doorbell_ring","bytes":231,"body_sha256":"9f2c…"}
{"ts":"2026-11-15T20:41:14.0Z","action":"reject","agent":"you","topic":"a2a/v1/agents/coordinator/inbox","id":"c1…","type":"request","code":"not_authorized","reason":"not allowed to send to \"coordinator\"","bytes":180}
```

**Bodies are never written,** only their SHA-256. Bodies may hold prompts,
personal data or (once conduit-1gkv lands) ciphertext. The hash lets you
check that a body you got from somewhere else is the one the broker
handled.

The file rotates at `audit_max_bytes` (default 64 MiB). The broker keeps
`audit_keep` old files (default 3) as `audit.jsonl.1` through `.3`. The
directory is created if missing, and files are mode 0600.

Useful queries:

```bash
# everything one agent sent
jq -c 'select(.from == "worker")' audit.jsonl

# rejections by code
jq -r 'select(.action == "reject") | .code' audit.jsonl | sort | uniq -c

# high-priority traffic
jq -c 'select(.priority == "high") | {ts, from, to, event}' audit.jsonl
```

## Not built: searchable message history

The audit log records that messages happened, not what they said. If you
later want "show me what agent X sent last week" with bodies, or replay of
a time range, that's a separate opt-in store. SQLite through
`modernc.org/sqlite`, the pure-Go driver conduit already uses, would suit
it. It belongs in its own feature, with its own retention and access
rules, because it would hold message contents.
