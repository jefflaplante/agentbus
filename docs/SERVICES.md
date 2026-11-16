# Writing agents that serve the bus

This guide covers agents that do work for other agents: what each topic
is for, how to set up an agent that listens on it, and how to answer what
arrives. [CONTRACT.md](../CONTRACT.md) is the wire format and stays the
authority. This guide is about using it well.

The Go examples use the `client` package. The same rules apply to an
agent built on `agentbus handle`, `agentbus mcp` or a plain MQTT client;
see [Other ways to serve](#other-ways-to-serve).
[examples/service](../examples/service/main.go) is a complete agent that
serves all three kinds of topic, in about 120 lines.

## What an agent can serve

| Topic | Who receives a message | What arrives | Reply |
|---|---|---|---|
| Inbox `a2a/v1/agents/{id}/inbox` | Only `{id}` | Requests and events addressed to this agent; also late replies and broker errors | Exactly one, for a request |
| Task queue `a2a/v1/tasks/{capability}` | Exactly one of the agents serving `{capability}` | Requests for whoever does that kind of work | Exactly one |
| Events `a2a/v1/events/{id}/{name...}` | Every agent watching a matching filter | Broadcasts from `{id}` | None; there is no one to reply to |
| Presence `a2a/v1/agents/{id}/presence` | Everyone | The agent's card, kept by the broker | n/a |

How to choose:

- **Use the inbox when the sender wants this agent specifically:** this
  conduit instance, this person's session, this host. The sender has to
  know the agent's ID and be allowed to reach it by `send_to`.
- **Use a task queue when the sender wants the work done and doesn't care
  by whom:** summarize, review, transcribe. Several workers can serve the
  same capability. The broker gives each task to one of them and keeps
  going if one dies.
- **Use events for facts other agents may want to know about:** a job
  finished, a deploy started. The producer doesn't know or wait for its
  audience.

## Identity and grants

Each running process needs its own agent ID. If a second connection uses
the same ID, the broker disconnects the first. Add the agent with the
capabilities it will take work for:

```bash
agentbus config agent add counter --capability wordcount
```

Two grants matter to a serving agent:

- **`capabilities`** lists the task queues the agent may consume. The broker
  refuses a subscription to any other queue. It also removes ungranted
  capabilities from the agent's presence card, so a card only advertises
  work the agent can actually take.
- **`send_to`** lists the inboxes the agent may contact on its own:
  events with `Send`, and requests with `Request`. It doesn't limit
  replies. When the broker accepts a request, it records it as open. It
  then lets exactly one `response` or `error` with that `correlation_id`
  through to the requester's inbox, even if the requester is outside the
  answerer's `send_to`. The answerer has to be the agent the request was
  addressed to; for a task, any agent granted the capability. A second
  reply, a reply after the request's TTL, and anything that isn't a reply
  are still checked against `send_to`. While the request is open, a reply
  from any other agent is refused, even one `send_to` would allow. Without a TTL, a request stays
  open for `session_expiry`. Open requests are kept in memory, so after a
  broker restart, replies to earlier requests need `send_to` again.

## Serving a task queue

Register task handlers in `client.Options.Tasks`, before connecting:

```go
c, err := client.Connect(ctx, client.Options{
    BrokerURL: url, AgentID: "counter", Token: tok,
    Description: `wordcount: body {"text": string}, replies {"words": int}.`,
    Tasks:       map[string]client.Handler{"wordcount": wordcount},
})
```

`ServeTasks` can add a capability after connecting. Don't use it for a
long-running worker. With a persistent session, the broker redelivers
queued tasks the moment the worker connects, before a `ServeTasks` call
could run. A task that arrives before its handler is registered is
dropped and acknowledged, with no reply, so the requester times out.

How the queue behaves:

- **Each task goes to one connected worker.** Start more workers, each
  with its own ID and the same capability, and the broker spreads tasks
  across them.
- **When no worker is connected, tasks queue** in the persistent session
  of an offline worker and are delivered when it returns. If no worker
  holds a subscription to the queue (none is connected, and none left a
  persistent session), the sender gets `no_consumer` immediately.
- **A task is acknowledged only when the handler returns.** If the worker
  process dies mid-task, the broker resends the task when that worker
  reconnects with its persistent session, and the new process handles it.
  `client/redelivery_test.go` covers this. With `CleanSession: true` the
  session is discarded on disconnect, so that task is gone, and the
  requester times out.

## Serving an inbox

The `Inbox` handler gets everything addressed to this agent, not only
requests. Switch on the type:

```go
Inbox: func(ctx context.Context, e *a2a.Envelope) (any, error) {
    switch e.Type {
    case a2a.TypeRequest:
        return answer(ctx, e)      // the return value is the reply
    case a2a.TypeEvent:
        note(e)                    // no reply; return values are ignored
    case a2a.TypeError:
        // The broker refused something this agent sent with Send or Emit,
        // or a reply arrived after its Request gave up.
        log.Warn("error envelope", "from", e.From, "correlation_id", e.CorrelationID)
    case a2a.TypeResponse:
        // A reply that arrived after its Request gave up. Ignore it.
    }
    return nil, nil
},
```

With no `Inbox` handler, requests are answered `no_handler` and
everything else is dropped.

An inbox is per agent, so it doesn't spread load. A request to an agent
that is offline with a persistent session queues until it returns. A
request to an agent with no session at all fails at once with `no_route`.

## Watching events

```go
err := c.WatchEvents(ctx, "a2a/v1/events/+/jobs/#", func(e *a2a.Envelope) {
    log.Info("job event", "from", e.From, "event", e.Event)
})
```

The filter is an MQTT filter under `a2a/v1/events/`. `+` matches one level
and `#` the rest. Anyone may watch any agent's events. Only the agent
itself may publish under `a2a/v1/events/{its id}/`, so `from` and the
topic always agree.

The callback gets the envelope, not the topic. `Emit` copies the event
name into the envelope's `event` field (unless `WithEvent` sets another),
so watchers can tell events apart; producers using another client should
do the same. Event names are 1-256 bytes with no `+`, `#`, NUL or empty
segments: `Emit` refuses anything else, because the broker would drop the
connection.

Events are fire-and-forget. A watcher that is offline misses them, unless
its persistent session already holds the subscription. There is no reply
and no way to tell the producer an event was handled. If the producer
needs to know, it should send a request instead.

## How to respond

### One reply per request

The handler's return value becomes a `response` with that body. A returned
error becomes an `error` reply. The client publishes exactly one or the
other, with `correlation_id` and `to` set. Don't publish your own reply to
a request as well.

### Error replies

Error bodies are `{"code": "...", "reason": "..."}`. These are the codes a
serving agent can cause:

| Code | Sent by | When |
|---|---|---|
| `handler_error` | client | The handler returned an error. `reason` is the error's text. |
| `handler_panic` | client | The handler panicked. `reason` is just "handler failed". |
| `busy` | client | `MaxConcurrent` handlers were already running. |
| `no_handler` | client | A request reached an agent with no `Inbox` handler. |

The broker sends the requester its own codes (`no_route`, `no_consumer`,
`not_authorized`, `expired` and others) before a request reaches any
handler. They're listed in [CONTRACT.md](../CONTRACT.md#broker-rejections).

**The requester sees the error's text.** The Go client sends `err.Error()`
as the reason, unchanged. Write it for the caller: say what to change, as
in `body must be {"text": "..."}`. Keep stack traces, file paths, internal
hostnames and anything secret out of it, and log those locally instead.
`agentbus handle` does the opposite: every failure is sent as "handler
failed", and the command's stderr stays in the local log.

To let callers tell kinds of failure apart, choose the code:

```go
return nil, client.Errorf("invalid", `body must be {"text": "..."}`)
```

Codes are 1-64 chars of `a-z 0-9 _ . -`; anything else falls back to
`handler_error`. A wrapped `*client.ReplyError` works too. If a handler's
return value can't be encoded as JSON, the client sends
`handler_error: reply body could not be encoded` rather than no reply.

### Body shapes

Bodies are JSON and the broker never reads them. Nothing checks that a
request has the shape a worker expects, so the worker has to:

- **Validate first.** Return a `handler_error` that names the expected
  shape, before doing any work.
- **Describe the shape in `Description`.** It's on the presence card,
  which `agentbus agents` and the MCP `agents` tool show. Models read it.
  In testing, a model sent a word-count task as a bare string, got the
  error back, and retried as `{"text": ...}`. With the shape in the
  description, it would have sent the right shape the first time.
- **Version by adding fields.** Accept requests without the new field. If
  the shape has to change incompatibly, serve it as a new capability
  (`summarize.v2`) next to the old one until callers move.

### Deadlines

The handler's `ctx` ends at the request's deadline: `ts + ttl_ms` when the
sender set a TTL, or `DefaultTimeout` (5 minutes) after the handler starts
when it didn't. The Go client sets the TTL of `Request` and `RequestTask`
from the requester's context deadline, and the CLI from `--timeout`.
`Send` and `Emit` carry no TTL, so a one-way message to an offline agent
waits for it however short the sender's context was.

- Pass `ctx` to everything the handler calls, and stop when it's done.
  After the deadline the requester has given up, so a late answer is
  thrown away.
- Messages that have already expired when they arrive are dropped before
  the handler sees them. The broker also refuses a publish that's
  already expired.

### Work that takes longer than a request should wait

A requester holds a pending request in memory until the reply comes. For
work measured in minutes or hours, don't make it wait:

1. Validate the request, start the work in the background, and reply at
   once with a job ID: `{"job": "j-42", "status": "accepted"}`.
2. When the work finishes, `Emit` an event (`jobs/done`) carrying the job
   ID, or `Send` the result to the requester's inbox. Sending needs the
   requester in this agent's `send_to`.
3. If the requester may want to check progress, also answer a status
   request in the inbox, keyed by the job ID.

The background work has to outlive the handler. Use the agent's own
context, not the handler's `ctx`. It also has to survive a restart if the
result matters, because the task was already acknowledged when the
handler returned.

### Duplicates

Delivery is at least once:

- **What's already deduplicated:** the broker drops a repeated envelope
  ID from the same sender within `dedupe_window` (10 minutes by default).
  The client drops one it has already handled, also for 10 minutes.
- **What isn't:** a task redelivered after the worker process crashed
  arrives in a new process with no memory of it, and runs again.

So a handler with side effects (writing a file, opening a ticket, calling
a paid API) should be idempotent, keyed by `from` and `id`. Record that
you've done the work for that key before acting on it again, or make the
action safe to repeat.

### Concurrency and order

Handlers run concurrently, up to `MaxConcurrent` (default 64) across the
inbox and all task queues together. Messages are not handled in the order
they arrived.

- Set `MaxConcurrent` to what the agent can actually run. A worker that
  makes model calls might take 2 to 4 at once, not 64. Requests beyond the
  limit are answered `busy` at once, so the requester can retry or try
  elsewhere, instead of waiting in a queue it can't see.
- **Events beyond the limit are dropped, with only a log line.** An agent
  that must not lose events needs a limit above its event rate, or a
  handler that hands the event to a queue and returns at once.
- If order matters, put a sequence number in the body and reorder on
  receipt. The bus doesn't keep order.

### Priority

`priority` is the sender's statement of urgency: `high`, `normal`, `low`
or `info`. The receiver decides what each means. A starting point, which
is how conduit maps it:

| Priority | Suggested handling |
|---|---|
| `high` | Act now, and tell a person |
| `normal` or unset | Act now, quietly |
| `low` | Batch it for the next scheduled run |
| `info` | Record it only |

Treat it as a request from the sender, not a command. An agent that lets
any sender mark anything `high` can be used to page a person on a loop.
If that matters, decide from `from`, which the broker checks, as well as
from `priority`, which the sender chooses.

### Untrusted input

The broker guarantees who sent a message. It says nothing about whether
the content is safe. `body`, `event`, `headers`, and a presence card's
`description` and `meta` are all the sender's words.

- If a model will read the message, put `a2a.UntrustedPreamble(e)` in
  front of it. `agentbus handle` passes the same text as
  `AGENTBUS_PREAMBLE`, and `agentbus mcp` adds it to every message.
- Never let a message choose the model, tools, skills, permissions or
  working directory the handler runs with. Fix those in the agent's own
  config.
- Decide what a sender may ask for by `from`. A worker that can change
  things (edit files, run commands, spend money) should check `from`
  against its own allow list, and give everyone else read-only behavior
  or an error.

## Other ways to serve

### From the shell: `agentbus handle`

```bash
agentbus handle --capability wordcount --exec ./wordcount.sh
agentbus handle --exec ./on-message.sh --types request,event
```

For each message, the command gets the envelope as JSON on stdin. It also
gets `AGENTBUS_FROM`, `AGENTBUS_TYPE`, `AGENTBUS_ID` and
`AGENTBUS_PREAMBLE` in its environment.

- **The reply is stdout:** JSON is sent as JSON, and anything else is
  sent as a JSON string.
- **A non-zero exit sends a generic error.** stderr stays in the local log.
- **The command is killed at the deadline,** along with its children.
- **Output over 1 MiB is an error.**

The session is persistent, so the redelivery rules above apply. See
[CLI.md](CLI.md#being-an-agent-from-the-shell).

### From a coding agent: `agentbus mcp` or a headless worker

Two ways to put an LLM coding tool behind a capability:

- **Unattended:** run it headless under `agentbus handle`.
- **Interactive:** let a live session take requests through
  `agentbus mcp`'s `inbox` and `reply` tools.

See [INTEGRATIONS.md](INTEGRATIONS.md).

### From Python

[examples/python](../examples/python) has an `Agent` class on paho-mqtt
2.x, a worker equivalent to the Go example service, and a command-line
client. They follow the rules in this guide, and an e2e test runs them
against a live broker. Unlike the Go client, a Python handler can choose
its error code: `raise ReplyError("invalid", "...")`.

### From any MQTT client

An agent in another language follows the same steps. `agentbus.py` in
the Python examples is a working implementation of each one:

1. Connect with client ID and username set to the agent ID, the token as
   password, and `clean_session = false`.
2. Subscribe with QoS 1 to `a2a/v1/agents/{id}/inbox`, and to
   `$share/{capability}/a2a/v1/tasks/{capability}` for each capability.
   The broker refuses any other subscription to a task topic.
3. Publish a presence card to `a2a/v1/agents/{id}/presence`, retained.
4. For each request, publish one envelope to `a2a/v1/agents/{from}/inbox`
   with `type` set to `response` or `error`, `from` set to your own ID,
   `to` set to the requester, and `correlation_id` set to the request's
   `id`.
5. Acknowledge each message only after you've replied, so a crash leads to
   redelivery, not loss. With paho, that means manual acks.
6. Drop envelopes whose `ts + ttl_ms` has passed, and envelopes you've
   already handled (by `from` and `id`).

The envelope fields and validation rules are in
[CONTRACT.md](../CONTRACT.md#envelope).

## Seeing how a service is doing

The broker counts every request it tracks, per target: how many are open,
answered, errored or expired, and how long replies take (p50 and p95).
`agentbus status --plain` includes these under `requests`. With
`metrics_listen` set in the broker config, the same numbers are on
`/metrics` for Prometheus, as `agentbus_requests_total`,
`agentbus_requests_open` and the `agentbus_request_duration_seconds`
histogram. A capability whose `expired` count climbs has workers that are
too slow, stuck, or offline; one whose `errored` count climbs is
refusing or failing work. See
[HOW-IT-WORKS.md](HOW-IT-WORKS.md#request-tracking-and-metrics).

## Checklist

- Its own agent ID, with `capabilities` for every queue it serves.
- `send_to` covering only the agents it contacts on its own; replies don't need it.
- Task handlers in `Options.Tasks`, and a persistent session.
- `Description` says what each capability expects and returns.
- The handler validates the body first and returns a caller-readable error.
- Error reasons contain nothing internal or secret.
- Every call in the handler takes `ctx`.
- Long work replies with a job ID and reports completion with an event.
- Side effects are idempotent by `from` and `id`.
- `MaxConcurrent` matches what the agent can really run.
- Messages a model reads have the preamble in front of them.
- What a request can make the agent do depends on `from`.

## Limits

- **Reply permission doesn't survive a broker restart.** Open requests
  are held in memory: 256 per requester, 65,536 in all. After a restart,
  or past those limits, a reply needs the answerer's `send_to` again.
- **No progress messages or streaming.** A request gets one reply. Use
  the long-work pattern above, with events for progress.
- **A full queue refuses new messages.** Once an agent has `max_queued`
  messages waiting (default 1024), senders get `queue_full` until it
  catches up. Messages are counted, not bytes, so a queue can hold up to
  `max_queued` × `max_envelope_bytes` (256 MiB at the defaults).
