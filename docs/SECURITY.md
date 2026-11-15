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
| `send_to` is enforced | Checked on every inbox publish. `reply_to` can't redirect a reply elsewhere. |
| Each task goes to one granted worker | The only allowed task subscription is `$share/{cap}/a2a/v1/tasks/{cap}`, and only for agents granted `{cap}`. |
| Presence can't be forged | Only the owner publishes its card. The broker stamps status and time, drops ungranted capabilities, and checks keys against pinned ones. |
| Stale or future-dated messages are refused | `ts` must be within `max_clock_skew`, and `ttl_ms` is capped at 24h. |
| Bounded resource use | See the limits table. |

## Limits

| Setting | Default | Bounds |
|---|---|---|
| `connect_timeout` | 10s | time to send CONNECT; idle sockets are closed |
| `max_connections`, `max_connections_per_ip` | 1024, 64 | open sockets, before and after auth |
| `rate_per_second`, `rate_burst` (also per agent) | 20/s, 50 | publishes per agent, including rejected ones |
| `max_envelope_bytes` | 256 KiB | one message |
| `max_queued` | 1024 | in-flight plus queued messages per session |
| `session_expiry` | 7 days | how long an offline agent's queue is kept (also across restarts with `data_dir`) |
| `dedupe_window` | 10 min | repeat `from`+`id` suppression; capped at 1M entries |
| `max_clock_skew` | 2 min | allowed `ts` drift |
| subscriptions | 256 per client | |
| error envelopes and reject logs | ~2/s per agent | one bad publish can't trigger unbounded output |
| auth-failure logs | ~1/s per IP | attacker-chosen IDs are truncated in logs |

## Operator checklist

- **Turn on TLS for anything off-host.** The broker refuses a non-loopback
  `listen` without `tls_cert`/`tls_key` unless `allow_insecure` is set.
  Without TLS, tokens cross the network in clear.
- **Give every process its own identity**, including CLIs and desktop tools.
  A shared ID makes connections knock each other off (the log shows
  `agent session taken over`).
- **Narrow `send_to`.** It defaults to everyone. Name the agents each agent
  should reach.
- **Treat `observer` tokens as admin credentials.** An observer reads every
  inbox.
- **Keep tokens out of `ps`.** Use `AGENTBUS_TOKEN_FILE` or `-token-file`
  rather than `-token`. Tokens are stored as unsalted SHA-256, which is safe
  for the 256-bit tokens `agentbus token` generates. Don't hand-pick short
  ones.
- **Pin keys** (`agents[].keys`) for agents that will exchange encrypted or
  signed messages.
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

For `agentbus handle -exec`:
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
| Low | Silent drops to missing agents or workers; MQTT 5 properties forwarded; self-asserted capabilities; presence expiring after 24h; QoS 2 rejection quirk | `no_route`/`no_consumer`; properties stripped; capabilities filtered by grants; presence refreshed; max QoS 1 |

## Not done yet

- **End-to-end signatures** (Ed25519 `sig` over canonical header fields plus
  a body hash). Until then, authenticity rests on the broker.
- **Sealed bodies** (X25519 + HKDF + AES-256-GCM with header fields as
  associated data), using pinned `kex` keys. `enc` is only a label today.
- **Token rotation and config reload** without a restart.
- **mTLS client authentication.**
- **A metrics endpoint.**
