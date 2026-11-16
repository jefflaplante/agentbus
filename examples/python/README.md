# Python examples

Connect to the bus and serve it from Python. These use only
[paho-mqtt](https://pypi.org/project/paho-mqtt/) 2.x and the standard
library.

| File | What it is |
|---|---|
| `agentbus.py` | The `Agent` class: one connection as one agent, serving and calling |
| `worker.py` | A service agent: the `wordcount` task queue, an inbox that answers `{"op": "ping"}`, a watcher for `jobs/...` events |
| `client.py` | A command-line client: `agents`, `request`, `task`, `send`, `emit`, `watch` |

`test/e2e/python_test.go` runs all three against a live broker, together
with the Go CLI. The test needs a Python with paho-mqtt. Set
`AGENTBUS_PYTHON` to its path if that isn't `python3`.

## Running them

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt

agentbus config agent add counter --capability wordcount
agentbus config agent add asker
```

In one terminal, the worker:

```bash
AGENTBUS_AGENT=counter .venv/bin/python worker.py
```

In another, the client:

```bash
export AGENTBUS_AGENT=asker
.venv/bin/python client.py agents
.venv/bin/python client.py task wordcount '{"text": "one two three"}'   # {"words": 3}
.venv/bin/python client.py request counter '{"op": "ping"}'             # {"op": "pong"}
.venv/bin/python client.py send counter '{"x": 1}' --event hello --priority low
.venv/bin/python client.py emit jobs/finished '{"job": 42}'
```

Connection settings come from `AGENTBUS_AGENT`, `AGENTBUS_TOKEN_FILE`
(default `~/.config/agentbus/tokens/<agent>.token`) and `AGENTBUS_URL`
(default `tcp://127.0.0.1:1883`). These are what `agentbus config env NAME`
prints. An `ssl://` URL turns on TLS.

## Writing your own agent

```python
from agentbus import Agent, ReplyError

def summarize(env):
    body = env.get("body")
    if not isinstance(body, dict) or "text" not in body:
        raise ReplyError("invalid", 'body must be {"text": "..."}')  # the caller sees this
    return {"summary": body["text"][:100]}                          # the reply body

agent = Agent.from_env(
    "summarizer",
    description='summarize: body {"text": string}, replies {"summary": string}',
    tasks={"summarize": summarize},   # before connect: queued tasks arrive at once
    max_concurrent=2,                 # more requests than this get "busy"
)
with agent:
    reply = agent.request("researcher", {"q": "..."}, timeout=120)
    ...
```

What the `Agent` class does for you, following
[docs/SERVICES.md](../../docs/SERVICES.md):

- **It acknowledges each message only after the handler finishes.** If the
  process dies mid-task, the broker resends the task when the agent
  reconnects. Tested by killing a worker with `kill -9` while it held a
  task: its replacement got the task and answered it.
- **It replies exactly once per request.** The handler's return value is
  the response body. `ReplyError(code, reason)` sends an error the caller
  should see. Any other exception is sent as `handler_error: handler
  failed`, and the traceback goes to the local log, not the requester.
- **It drops envelopes that have expired, or that it has already handled
  in the last 10 minutes.**
- **It answers `busy` when `max_concurrent` handlers are already running.**
  Events over the limit are dropped with a log line.
- **It accepts a reply only from the agent the request went to** (any
  agent for a task), or from `$broker`.
- **It uses a persistent session by default,** so work sent while the
  agent is down waits for it. `client.py` passes `clean_session=True`
  because a one-shot client shouldn't leave a session behind.

Handlers run on a thread pool, not on paho's network thread, so a slow
handler doesn't stop other messages. `agentbus.preamble(env)` returns the
standard "untrusted input" text, to put in front of an envelope a model
will read.

Each process needs its own agent ID. A second connection with the same ID
disconnects the first.
