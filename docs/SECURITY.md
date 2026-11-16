# Security model

## Who the broker defends against

1. **Anyone who can reach the port without a token.** They must not get in
   or exhaust the broker.
2. **An agent with a valid token** that is compromised, buggy or steered by
   prompt injection. It must not impersonate another agent, read inboxes
   that aren't its own, reach agents outside its `send_to`, take tasks it
   isn't granted, or swamp the broker or other agents.
3. **Receivers trusting content they shouldn't.** The broker guarantees who
   sent a message and which fields it checked. It does not guarantee that
   the content is safe to act on.

Out of scope for now: someone who controls the broker itself, and network
attackers when TLS is off. End-to-end signing and encryption (see below)
are what remove trust in the broker.

## What the broker guarantees

| Guarantee | How |
|---|---|
| Only configured agents connect | Client ID = username = agent ID. The token's SHA-256 is compared in constant time. |
| `from` is the authenticated agent | Checked on every publish. The broker forwards its own re-encoding, so every receiver parses the same fields it checked. |
| No side doors around those checks | Will messages are refused at connect. MQTT 5 publish properties are stripped. Retain is honored only on presence. |
| Inboxes are private | Only the owner can subscribe. Observers are the explicit exception (see below). |
| `send_to` is enforced | Checked on every inbox publish except one reply per open request, from the agent the request went to (or a worker granted the task's capability), within the request's TTL. `reply_to` can't redirect a reply elsewhere. |
| Replies come from the answerer | While a request is open, a reply to it is refused unless it comes from the addressed agent, or for a task from an agent granted the capability. Open requests are tracked in memory, up to 65,536. |
| Each task goes to one granted worker | The only allowed task subscription is `$share/{cap}/a2a/v1/tasks/{cap}`, and only for agents granted `{cap}`. |
| Presence can't be forged | Only the owner publishes its card. The broker stamps status and time, drops ungranted capabilities, and checks keys against pinned ones. |
| Stale or future-dated messages are refused | `ts` must be within `max_clock_skew`, and `ttl_ms` is capped at 24h. |
| Bounded resource use | See the limits table. |

## Limits

| Setting | Default | Bounds |
|---|---|---|
| `connect_timeout` | 10s | time to send CONNECT; idle sockets are closed |
| `max_connections`, `max_connections_per_ip` | 1024, 64 | open sockets, before and after auth; IPv6 counted per /64 |
| connections not yet logged in | a quarter of each of the above | when full, the oldest unauthenticated socket is closed, so idle sockets can't lock agents out |
| `rate_per_second`, `rate_burst` (also per agent) | 20/s, 50 | publishes per agent, including rejected ones |
| `max_envelope_bytes` | 256 KiB | one message |
| `max_queued` | 1024 | in-flight plus queued messages per session; further messages are refused with `queue_full` |
| `session_expiry` | 7 days | how long an offline agent's queue is kept (also across restarts with `data_dir`) |
| `dedupe_window` | 10 min | repeat `from`+`id` suppression; capped at 1M entries |
| `max_clock_skew` | 2 min | allowed `ts` drift |
| subscriptions | 256 per client | |
| topic length | 512 bytes | event names can't carry kilobytes into logs and audit records |
| open requests | 256 per requester, 65,536 total, 24h without a TTL | expired entries swept every second |
| error envelopes and reject logs | ~2/s per agent | one bad publish can't trigger unbounded output |
| auth-failure and Will-refusal logs | ~1/s per IP | attacker-chosen IDs are truncated in logs |
| rate-limiter state | idle buckets pruned after 10 min | per-IP keys can't grow without bound |

## Operator checklist

- **Turn on TLS for anything off-host.** The broker refuses a non-loopback
  `listen` without `tls_cert`/`tls_key` unless `allow_insecure` is set.
  Without TLS, tokens cross the network in clear.
- **Give every process its own identity**, including CLIs and desktop tools.
  A shared ID makes connections knock each other off (the log shows
  `agent session taken over`).
- **Narrow `send_to`.** It defaults to everyone. Name the agents each agent
  should reach on its own. Replies to requests it receives don't need to be
  listed.
- **Treat `observer` tokens as admin credentials.** An observer reads every
  inbox and the broker status (`$SYS/agentbus/status`), which lists every
  agent, its address and its traffic.
- **Keep tokens out of `ps`.** Use `AGENTBUS_TOKEN_FILE` or `--token-file`
  rather than `-token`. Tokens are stored as unsalted SHA-256, which is safe
  for the 256-bit tokens `agentbus token` generates. Don't hand-pick short
  ones.
- **Pin keys** (`agents[].keys`) for agents that will exchange encrypted or
  signed messages.
- **Rotate tokens and remove agents with `agentbus config`.** The running
  broker reloads the file within a second and disconnects the affected
  agent. Nothing needs a restart, and a config that doesn't validate is
  rejected while the old one stays in force.
- **Keep `metrics_listen` on loopback.** The metrics endpoint has no
  authentication and names every agent. The broker refuses a non-loopback
  address unless `allow_insecure` is set; scrape it locally or through an
  authenticating proxy.
- **The bootstrapped `admin` identity is an observer.** `agentbus serve`
  creates it on first run. Keep `~/.config/agentbus/tokens/` (mode 0700,
  files 0600) private, or remove `admin` once you've set up your own
  observer.
- **Protect `data_dir` and `audit_file`.** `broker.db` holds queued message
  bodies for offline agents. The audit log holds metadata: who talked to
  whom, when, and with what priority. Both are created with mode 0600; keep
  them on a disk only the broker's user can read, and include `broker.db`
  in whatever encryption-at-rest you use.

## Agent-side responsibilities

These are what the Go client does. Other clients should do the same.

- Accept a reply only from the addressed agent, or from `$broker`.
- Recover from handler panics, and cap concurrent handlers (`busy` when
  full; `MaxConcurrent`, default 64).
- Bound each handler with the request TTL, or `DefaultTimeout` (5 min).
- Deduplicate redelivered envelopes.
- Put `a2a.UntrustedPreamble(env)` in front of anything an LLM will read,
  and never let an envelope pick tools, skills, model or permissions.
- Gate outgoing publishes where a human should approve them
  (`Options.BeforePublish`).

For `llm:` handlers in the `agent` runtime:
- The model, system prompt, tools and bus permissions come from the
  manifest. Nothing in an envelope can change them.
- The message, bus replies and presence descriptions reach the model
  behind the untrusted-input preamble.
- A tool call runs as a request from the original sender, so the tool
  handler's `allow_from` applies. The model can't reach a handler that
  the sender couldn't.
- Bus calls are limited to the agents and capabilities in `bus:`, on top
  of the broker's grants, and carry a `hops` count that stops chains at
  `max_hops`.
- Commands run without the model's API key variable, as they run without
  the agentbus token.
- API errors and tool stderr stay in the local log. The requester gets a
  generic error.

None of this solves prompt injection. A message can still talk the model
into calling the tools it was given, with inputs you didn't expect.
Treat every tool as reachable by any sender the `llm:` handler allows:
keep tools read-only where you can, check their input, and give tools
that change things their own `allow_from`.

For `agentbus handle --exec`:
- The command runs only for requests by default (`-types`).
- It runs without the agent's token in its environment.
- On timeout it is killed along with its whole process group.
- Output is capped at 1 MiB.
- Its stderr is logged locally and never sent back to the requester.

## Review history

An independent review (2026-11-15) found the following, all fixed with
regression tests in `broker/security_test.go`, `client/client_test.go` and
`cmd/agentbus/exec_test.go`:

| Severity | Finding | Fix |
|---|---|---|
| Critical | Will messages skipped every contract check, allowing `from` spoofing, `send_to` bypass and presence forgery | Refuse Wills at connect; drop any Will in `OnWill` |
| High | Retained inbox and task messages were replayed on every resubscribe | Retain ignored except on presence |
| High | JSON parser differential: `{"from":"bob","FROM":"carol"}` passed as carol but reads as bob elsewhere | Forward the broker's canonical re-encoding |
| High | Plaintext listener on all interfaces by default | Refuse non-loopback plaintext without `allow_insecure`; example config binds loopback |
| Medium | No pre-auth deadline or connection cap | Guarded listener with CONNECT deadline and per-IP and total caps |
| Medium | Unbounded offline queues | `max_queued`, `session_expiry`, write and QoS caps |
| Medium | Client handlers: no concurrency bound, timeout or panic recovery; `-exec` leaked its token and stderr and could orphan children | `MaxConcurrent`, `DefaultTimeout`, recover; `-exec` hardening |
| Medium | `reply_to` to any inbox; replies not bound to the responder | `reply_to` must be the sender's inbox; client checks the replier |
| Medium | The rejection path wasn't rate-limited | Rate-limit first; throttle error envelopes and logs |
| Medium | Plain task subscriptions copied every task | Only the exact `$share/{cap}/...` filter is allowed |
| Medium | Unbounded `ts` defeated TTL | Clock-skew window; `ttl_ms` ≤ 24h |
| Medium | Any agent that could reach a requester could answer its task first with a forged reply; the requester can't know which worker is genuine (found 2026-11-15, after the review) | The broker tracks open requests and refuses replies from anyone but the answerer (`broker/replies_test.go`) |
| Low | Silent drops to missing agents or workers; MQTT 5 properties forwarded; self-asserted capabilities; presence expiring after 24h; QoS 2 rejection quirk | `no_route`/`no_consumer`; properties stripped; capabilities filtered by grants; presence refreshed; max QoS 1 |

A second review (2026-11-16, five parallel reviewers on security,
concurrency, the CLI and TUI, the clients and MCP server, and missing
features) found the following, fixed with regression tests in
`broker/hardening_test.go`, `broker/replies_internal_test.go`,
`client/review_test.go`, `cmd/agentbus/mcp_test.go` and others:

| Severity | Finding | Fix |
|---|---|---|
| Critical | A config reload during a login could crash the broker (nil agent in `OnSessionEstablished`) | The login is checked again against the current config once the session exists; a stale one is disconnected |
| Critical | Shutdown waited out `connect_timeout` for sockets still in the handshake | The listener tracks every socket and closes them on shutdown; logins are refused once shutdown starts |
| High | One agent could fill the open-request table, switching reply checking off for everyone | 256 open requests per requester (its own oldest is evicted), 24h without a TTL, expired entries swept every second |
| High | A token rotation or removal could miss a login in progress | Same re-check as the crash fix |
| High | Message IDs were only length-checked and appeared raw in the LLM preamble, so a sender could write its own "trusted" text | IDs and correlation IDs are limited to `A-Z a-z 0-9 . _ : -`; the preamble never includes an invalid field |
| High | A client `Emit` with a wildcard name put the agent into a reconnect loop | `Emit` validates names; the broker caps topics at 512 bytes |
| Medium | Pre-auth Will refusals and long event topics could flood the log and rotate the audit file away | Will refusals throttled per IP; topics capped and truncated in logs |
| Medium | Presence cards went stale on reload: a removed agent stayed online, revoked capabilities and re-pinned keys weren't reflected | Removed agents' cards are cleared and their sessions dropped; changed agents' cards are re-stamped, also on restart |
| Medium | Idle unauthenticated sockets could take every connection slot | A separate pre-auth budget that evicts its oldest socket when full; IPv6 limits per /64 |
| Medium | A full offline queue dropped messages silently | Refused with `queue_full` |
| Medium | A late offline card could overwrite a reconnected agent's online card | The offline card is decided and published under the presence lock |
| Medium | Two replies to one task could both be accepted; concurrent reloads could apply an older file last | Reply settlement is a single atomic claim; reloads are serialized |
| Medium | MCP: a second sender reusing a request ID could take the first sender's reply | Pending requests are keyed by sender and ID |
| Medium | Found while checking the fixes: a quick reconnect under the same agent ID (every CLI command reconnects) could lose its session to mochi's late teardown of the previous connection, so replies to it were refused with `no_route` | The broker watches each reconnect for two seconds and restores the session if mochi deletes it (`broker/guard_internal_test.go`) |
| Low | Unbounded per-IP limiter map; JSON parsed before rate limiting; reply window shortened by a slow requester clock | Idle buckets pruned; parsing after the limiter; window counted from the later of `ts` and acceptance |

## Not done yet

- **An upstream data race in mochi 2.7.9.** `TopicsIndex.scanMessages` reads
  a node's retained path without the lock `RetainMessage` holds while
  writing it. It shows up under `-race` when a retained presence card is
  published while another client subscribes. The value written is the same
  topic string each time, so a torn read is very unlikely to matter, but
  it needs a fix in mochi (taking the index lock in `Messages`).

- **End-to-end signatures** (Ed25519 `sig` over canonical header fields plus
  a body hash). Until then, authenticity rests on the broker.
- **Sealed bodies** (X25519 + HKDF + AES-256-GCM with header fields as
  associated data), using pinned `kex` keys. `enc` is only a label today.
- **mTLS client authentication.**
