# Lab 2: A first agent

**Goal:** generate an agent from the template, run it, and talk to it
from the shell.

## 1. Two identities

Every process on the bus has its own agent ID. You need one for yourself
and one for the agent.

**Terminal 2:**

```bash
agentbus config agent add me
agentbus config agent add hello --capability hello.wordcount --capability hello.disk
```

`--capability` lets `hello` take work from those two task queues. The
broker picks up both identities within a second; terminal 1 logs
`config reloaded`.

Each `add` writes a token file under `$LAB/tokens` and prints three
`export AGENTBUS_...` lines. Those lines are for running the agent on
another host. Don't paste them here: on this machine the CLI finds the
address and the token from the config.

## 2. Generate the agent

```bash
agentbus agent init hello --dir $LAB/hello --agentbus-src $REPO
cd $LAB/hello
cat agent.yaml
```

> If you installed with `go install` and have no clone, leave out
> `--agentbus-src`. It points the project at your checkout.

The manifest lists **handlers**. Each one has a **trigger**, saying which
messages it handles, and an **action**, saying what runs:

| Handler | Trigger | Action |
|---|---|---|
| greet | inbox, `op: greet` | `go: greet`, a Go function in `handlers/handlers.go` |
| wordcount | `task: hello.wordcount` | `script: scripts/wordcount.py` |
| disk | `task: hello.disk` | `script: scripts/disk.sh` |
| uptime | inbox, `op: uptime` | `run: uptime`, a shell command |

**Inbox vs. task:**
- **Inbox:** a request sent to `hello` by name.
- **Task:** work sent to whoever serves the capability. With three
  `hello`s on three hosts, the broker would give each task to exactly one
  of them.

Read `scripts/wordcount.py`. It's the whole contract for a script
handler:
- the envelope JSON arrives on stdin
- the reply goes to stdout
- to refuse with an error the requester sees, print
  `{"code": ..., "reason": ...}` and exit 3

## 3. Run it

**Terminal 3** (exports first):

```bash
cd $LAB/hello
agentbus agent run
```

```text
level=WARN msg="skipping Go handler: it only exists in a built agent (agentbus agent build)" handler=inbox:greet go=greet
● serving as hello on tcp://127.0.0.1:1883; ctrl+c to stop
```

It warns that `greet` is skipped. Go handlers exist only in a built
binary, which is lab 4. The other three run straight from disk.

> `not Authorized` here means the broker hasn't loaded the new identity
> yet. Run the command again.

## 4. Talk to it

**Terminal 2:**

```bash
export AGENTBUS_AGENT=me
agentbus agents
```

`hello` is online. Its description lists each handler's body shape: that's
how other agents, and later Claude, know what to send. Press `q` to leave
the list.

`export AGENTBUS_AGENT=me` makes every later command in this terminal
run as `me`. Without it they run as `admin`.

```bash
agentbus request --to hello --body '{"op":"uptime"}'
agentbus task -c hello.wordcount --body '{"text":"one two three"}'
agentbus task -c hello.disk --body '{"path":"/"}'
```

On a terminal, each prints who answered and how long it took, then the
reply body. For the word count:

```text
✓ response from hello in 270ms
{
  "lines": 1,
  "words": 3
}
```

Piped, or with `--plain`, the same commands print the whole reply
envelope as one line of JSON, for scripts.

Now the failures. Each has its own error code, and `from` says who
refused:

```bash
agentbus task -c hello.wordcount --body '"oops"'             # the script's own error code: invalid
agentbus task -c hello.disk --body '{"path":"/; rm -rf ~"}'  # refused: input is validated
agentbus request --to hello --body '{"op":"nope"}'           # unknown_op, with the ops it does handle
agentbus task -c nobody.home --body '{}'                      # no_consumer: nobody serves that queue
```

Each prints the error code, who refused, and the reason, and exits 1:

```text
✗ invalid from hello after 260ms
  body must be {"text": "..."}
✗ invalid from hello after 250ms
  path may contain only A-Z a-z 0-9 . _ / -
✗ unknown_op from hello after 240ms
  this agent handles body.op: uptime
✗ no_consumer from $broker after 210ms
  no agent is serving capability "nobody.home"
```

The first three come from `hello`. The last comes from `$broker`: no
agent was involved.

## 5. Watch it

**Terminal 4** (exports first, but **not** `AGENTBUS_AGENT`):

```bash
agentbus console --tab traffic
```

It connects as `admin`, an observer, so it shows everyone's traffic.
Rerun the requests in terminal 2 and watch them arrive. Use `↑`/`↓` and
`enter` to inspect a message, and `q` to quit.

You can send from the console too. Press `2` for Agents, select `hello`,
and press `r`: the Compose tab opens with a request to `hello`. Type
`{"op": "uptime"}` as the body and press `ctrl+s`. The reply shows beside
the form.

## 6. Stop it

In terminal 3, press `ctrl+c`. The agent stops taking new work, finishes
what's running, and disconnects. `agentbus agents` now shows `hello`
offline.

## Questions

1. Which of the four handler kinds would you use for a one-line check,
   for a data job, and for something that has to run on a host without
   Python?
2. `disk.sh` refuses `/; rm -rf ~`. Where exactly is that check, and why
   does it pass the path after `--`?

**Done when:** you have a reply from `uptime`, `wordcount` and `disk`,
and have seen `invalid` and `unknown_op`.
