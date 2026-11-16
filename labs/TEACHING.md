# Teaching script: agentbus in two hours

This is the instructor's guide to the labs. It covers:
- what to check before the session
- what to say and show for each lab
- where people get stuck
- questions that start useful discussion

The labs themselves are in this folder; students follow them at their
own pace while you narrate the main points.

**Audience:** engineers who know a terminal and some Go or Python. MQTT
experience isn't needed.
**Format:** about two hours, hands-on, each student on their own laptop.
**Outcome:** each student has run a broker, built and deployed a custom
agent, and watched Claude Code use it to answer a question.

## Before the session

Run the whole lab sequence on the machine you'll present from, and ask
students to do the same the day before:

```bash
labs/check.sh --claude
```

It runs every lab unattended (about 3 minutes) and stops at the first
problem. It checks:
- **Go:** the version, and that agentbus builds
- **Port 1883:** that it's free
- **The weather APIs:** they need outbound HTTPS to api.weather.gov,
  geocoding-api.open-meteo.com and api.open-meteo.com
- **Claude Code:** that it's signed in

On a locked-down network, get those three hosts allowed first.

Have four terminals ready, in a layout people can read from the back of
the room:

| Terminal | Runs |
|---|---|
| 1 | `agentbus serve`, the broker log |
| 2 | your commands, as `me` |
| 3 | the agent being worked on |
| 4 | `agentbus console --tab traffic` (as `admin`), and later `claude` |

## Timing

| Time | Lab | Main point |
|---|---|---|
| 0:00 | Intro | Why agents need a bus, and why this one enforces identity |
| 0:10 | 0, 1 | One binary; zero-config broker; identities and tokens |
| 0:25 | 2 | Triggers and actions; inbox vs. task; errors are information |
| 0:45 | 3 | A real handler: validate, call out, reply or refuse |
| 1:10 | 4 | One file to deploy; the token stays out of it |
| 1:25 | 5 | Claude as one more agent; discovery needs guidance |
| 1:45 | 6 | Go handlers, allow-lists, metrics; wrap up |

If time runs short, cut lab 6 to a demo and keep lab 5. Lab 5 is what
people remember.

## Intro (10 min)

**Say:**
- **Agents need to talk to each other**: a gateway like conduit, coding
  sessions, small workers on servers. Point-to-point HTTP between all of
  them gets messy: every pair needs addresses, auth and retries.
- **A message bus fixes the addressing.** But a plain MQTT broker trusts
  whatever a client says about itself.
- **agentbus is a bus that enforces a contract.** Every agent has an
  identity and a token. The broker stamps who sent each message, and
  checks who may talk to whom and who may take which work. Agents can
  trust `from`.

**Show** the diagram in [README.md](README.md). Point out the three kinds
of participant they'll build: a person at a CLI, a deployed agent, and
Claude Code. To the broker, all three are the same thing.

## Labs 0 and 1: setup and the broker (15 min)

**Say:**
- **One binary.** `agentbus` is the broker, the CLI, the console, the
  MCP server and the agent generator.
- **`AGENTBUS_CONFIG`** keeps today's work in `$LAB`. Without it,
  everything goes to `~/.config/agentbus`.
- **Secure defaults.** `config init` creates a loopback-only broker and an
  `admin` observer whose token file is mode 0600, and the broker stores
  only a hash of each token. The broker refuses to listen beyond loopback
  without TLS unless you explicitly allow it.

**Show:** `agentbus status`. `admin` is online because the CLI found the
admin token next to the config. Point at the config path line and the
agent table.

**Ask:** "Where did the CLI get the address and the token?" The answer
is the config, which is the pattern for every later command.

**Watch for:** port 1883 already in use (a local Mosquitto). Fix it with
`agentbus config set listen 127.0.0.1:1884`; every command follows.

## Lab 2: a first agent (20 min)

**Say:**
- **An agent is a manifest.** Each handler has a trigger (a task queue, an
  inbox op, or an event) and an action (a shell command, a script, or a
  Go function).
- **Inbox vs. task.** An inbox request goes to one named agent. A task
  goes to whoever serves the capability: ten workers, and each task goes
  to exactly one of them.
- **Scripts share one contract.** The envelope comes in on stdin and the
  reply goes out on stdout. Exit 3 with a code to refuse.

**Show:** `agent.yaml` side by side with `scripts/wordcount.py`. Then the
four failure commands. Linger on them:
- `invalid` is the script choosing its own error
- `unknown_op` lists the ops the agent handles
- `no_consumer` comes from the broker, before any agent is involved

**Show:** the console's Traffic tab in terminal 4 while you send
requests. People like seeing the traffic. Then show Compose: `2`, select
`hello`, `r`, a body, `ctrl+s`.

**Ask:** "disk.sh refuses `/; rm -rf ~`. Whose job is that check?" The
handler's. The broker guarantees who sent a message, not that its content
is safe.

**Watch for:**
- **`not Authorized`** when the agent starts right after `config agent
  add`. The broker reloads within a second; run it again.
- **A missing `export AGENTBUS_AGENT=me`** in terminal 2. Commands then
  run as `admin`, which works but makes the console's traffic confusing.

## Lab 3: the weather agent (25 min)

**Say:**
- **This is a real handler:** validate the input, call two public APIs,
  shape a reply, or refuse with a precise error.
- **Standard library only,** so it runs on any host with python3.
- **One script, two triggers:** a task for "anyone who can do this", and
  an inbox op for "ask this agent".

**Show:** `forecast.py` top to bottom, using the table in the lab. Then
run it by hand with `echo '…' | python3 scripts/forecast.py`. Making the
point that a handler is just a program, testable without the bus, saves
a lot of debugging later.

**Show:** the Seattle task, then Paris (Open-Meteo), then the three
refusals.

**Ask:** "What do the descriptions in agent.yaml do?" They go on the
presence card, and in lab 5 they're how Claude learns what to send.
Plant this now.

**Watch for:**
- **`CERTIFICATE_VERIFY_FAILED`**: a Python without CA certificates. The
  script falls back to the OS bundle. If that fails too, on macOS run
  `/Applications/Python 3.x/Install Certificates.command`.
- **Corporate proxies** blocking api.weather.gov: the NWS call fails over
  to Open-Meteo for US cities. Point out the `source` field.

## Lab 4: build and deploy (15 min)

**Say:**
- **`agent build` makes one static file per target,** with the manifest,
  scripts and Go handlers inside. Copy it and run it.
- **The token is never inside.** Each host gets its own token file,
  ideally through the systemd credential in the generated unit.
- **One process per agent ID.** Stop `agent run` first, because the
  binary uses the same identity.

**Show:**
- `ls` in `$LAB/remote-host`: two files, the binary and its token
- `./weather -check`
- the forecast arriving from the binary
- `ctrl+c` and the "agent stopped" line. Explain the drain: new work gets
  `busy`, running handlers finish, then it disconnects.

**Ask:** "How would you run this on three build hosts?" Give each host
its own identity with the same capability; tasks then spread across
them.

**Watch for:** forgetting to stop `agent run` before starting the binary.
Both flap on and off the bus, and terminal 1 logs "agent session taken
over". That makes a good live lesson if it happens.

## Lab 5: Claude Code (20 min)

This is the payoff.

**Say:**
- **Claude Code joins the bus as an agent,** with its own identity,
  `claude-code`. The `agentbus mcp` server gives it seven tools.
- **To the broker, Claude is no different from the weather agent.** Same
  token rules, same checks on `from`.

**Do step 3 live, without the CLAUDE.md file.** Ask *"What's the weather
forecast for Seattle, WA?"*. Very often Claude doesn't use the bus: it
tries `curl` on a weather site or says it can't check. Don't skip this,
because it's the most important lesson in the lab: **having a tool isn't
the same as knowing when to use it.**

**Then step 4: add `CLAUDE.md` and ask again.** Now Claude:
1. calls `agents`
2. finds `weather` and reads the body shape from its description
3. sends the task
4. answers, naming the agent and the NWS as the source

Show terminal 3 logging `from=claude-code`.

**Discuss:**
- **Discovery depends on descriptions.** A vague description means wrong
  bodies or no call at all.
- **Prompt injection.** Replies are wrapped as "untrusted input from
  another agent". Ask: "What could a hostile weather agent put in a
  forecast?"
- **Where the guidance lives.** `CLAUDE.md` for a project, a skill, or
  the user's own instructions. This is a design decision for any team
  putting agents on a bus.

**Watch for:**
- **Claude started without the exports,** so `${AGENTBUS_CONFIG}` is
  empty and the MCP server can't connect. `/mcp` shows it failed; restart
  `claude` from a terminal with the exports.
- **The project MCP server prompt dismissed:** run `/mcp` to approve it.

## Lab 6: extend it (15 min, or a demo)

**Say:**
- **A Go handler is a registered function,** and adding one is dropping a
  file in `handlers/`.
- **`allow_from` limits a handler to named senders,** on top of the
  broker's grants.
- **The broker tracks every request it routes:** open, answered,
  errored, expired, and latency. With `metrics_listen`, Prometheus can
  scrape it.

**Show:**
- `current` answering you and refusing `hello` with `forbidden`
- the `requests` row in `agentbus status`

**Ask:** "What would you alert on?" A capability's `expired` count rising
means no worker answered in time.

## Wrap-up (5 min)

Recap the three participants: a person, a deployed agent, and Claude.
All three went through one broker that checked every identity.

Ask what they would put on the bus at work. Typical answers:
- a log fetcher on app servers
- a deploy approver
- a cost reporter
- a GPU health probe

Each one is an `agent init`, a handler and a build.

**Further reading in this repository:**
- [docs/AGENTS.md](../docs/AGENTS.md): building agents
- [docs/SERVICES.md](../docs/SERVICES.md): how to serve requests well
- [docs/INTEGRATIONS.md](../docs/INTEGRATIONS.md): Claude Code, OpenCode,
  conduit
- [docs/SECURITY.md](../docs/SECURITY.md): what the broker guarantees and
  what it doesn't

## Common problems, quick reference

| Symptom | Fix |
|---|---|
| `address already in use` | `agentbus config set listen 127.0.0.1:1884` before `serve` |
| `not Authorized` right after adding an identity | Wait a second and retry; the broker reloads its config within a second |
| An agent flaps online and offline | Two processes share its ID; stop one |
| `CERTIFICATE_VERIFY_FAILED` | Python lacks CA certs; on macOS run Install Certificates.command |
| No forecast, `upstream_error` | The network blocks the weather APIs; check with `curl https://api.weather.gov` |
| `/mcp` shows agentbus failed | `claude` started without the exports, or `agentbus` isn't on PATH |
| Claude ignores the bus | Add `CLAUDE.md` (lab 5, step 4), or ask it to "use the agentbus tools" |
| `agent build` says a Go handler isn't in the binary | The manifest names a `go:` function that no file in `handlers/` registers |
