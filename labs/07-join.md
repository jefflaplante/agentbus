# Lab 7: Join (optional)

**Goal:** a second host asks to join the bus, and you approve it as the
operator. No token file is copied.

In labs 2 to 5 the operator did two things for every agent: added it
with `agentbus config agent add`, and got its token file to the agent's
host. In lab 4 you copied `weather.token` by hand. With join, the host
makes its own token and asks, and the operator only approves.

You need the broker from lab 1 running in terminal 1. Step 5 also uses
the weather binary from lab 4; skip that step if you don't have it.

## 1. Turn on the join listener

**Terminal 2:**

```bash
agentbus config set join_listen 127.0.0.1:8884
```

Terminal 1 logs `restart_needed=[join_listen]`. A listener address is
one of the few settings the broker can't apply live.

**Terminal 1:** `ctrl+c`, then:

```bash
agentbus serve
```

It now logs a second listener:

```text
level=INFO msg="join requests over HTTP" addr=127.0.0.1:8884 path=/join tls=false
```

The join listener is a separate HTTP port. It only takes join requests;
it carries no messages. Loopback is safe without TLS. For hosts on the
network, use an address they can reach and set `tls_cert` and `tls_key`.
Over plain HTTP, someone on the network path could replace the request
with their own.

## 2. Ask to join, as a second host

Everything in these labs is on one machine, so a new terminal stands in
for the second host. It gets its own config directory, so its token file
doesn't land in the broker's `tokens/` directory.

**Terminal 5** (a new one; do **not** paste the lab 0 exports):

```bash
export PATH=~/src/agentbus/bin:$PATH
export AGENTBUS_CONFIG_DIR=~/agentbus-lab/host2
agentbus join --join-url http://127.0.0.1:8884 --agent weather-2 \
  --capability weather.forecast --capability weather.alerts \
  --description "second forecast worker"
```

```text
✓ asked 127.0.0.1:8884 to add weather-2; the token stays in /Users/you/agentbus-lab/host2/tokens/weather-2.token
  Waiting for an operator. On the broker: agentbus config join approve weather-2
  Run this command again to see how the request stands, or add --wait 1h to block until it's answered.
```

`join` did three things:
- made a token
- wrote it to a token file on this host (mode 0600)
- sent the broker the token's SHA-256, the ID it wants, and the
  capabilities it would like to serve

The token itself never left this host. The broker stores only hashes, as
you saw in lab 1, so the hash is all it needs.

> `connection refused` means the broker wasn't restarted after step 1, or
> the port is wrong.

The token doesn't work yet. Try it:

```bash
agentbus agents --plain --url tcp://127.0.0.1:1883 --agent weather-2 \
  --token-file ~/agentbus-lab/host2/tokens/weather-2.token
```

```text
agentbus: connect tcp://127.0.0.1:1883: not Authorized
```

Terminal 1 logs `auth rejected` for `weather-2`. Asking grants nothing.

## 3. Approve it, with less than it asked for

**Terminal 2:**

```bash
agentbus config join
```

```text
AGENT      ASKS FOR                         FROM                  AGE  DESCRIPTION
weather-2  weather.alerts,weather.forecast  yourhost (127.0.0.1)  40s  second forecast worker
```

`FROM` is the host name the client reported and the address the broker
saw. The host name is a label the client chose. On a real network, check
the address, or ask whoever started the client, before you approve.

`ASKS FOR` is only a request. The operator decides what to grant:

| Command | Grants |
|---|---|
| `agentbus config join approve weather-2` | what it asked for |
| `agentbus config join approve weather-2 --capability X` | exactly `X` |
| `agentbus config join approve weather-2 --capability=` | no capabilities; it can still send, request and receive |
| add `--send-to me` to any of these | limits whose inbox it may message |
| `agentbus config join deny weather-2` | nothing; the request is discarded |

Nobody on this bus sends `weather.alerts` tasks, so grant only the
forecast queue:

```bash
agentbus config join approve weather-2 --capability weather.forecast
```

```text
✓ approved weather-2: serves weather.forecast; may send to anyone; rate default
  A running broker accepts it within a second. Its token stays on yourhost (127.0.0.1).
```

Approving adds `weather-2` to `broker.json` with the hash the client
sent. Terminal 1 logs `config reloaded ... added=[weather-2]`.

## 4. Collect the answer on the client

**Terminal 5:** run the `agentbus join` command from step 2 again (up
arrow). It reuses the token file, so it asks about the same request:

```text
✓ weather-2 is approved: granted capabilities weather.forecast
  not granted: weather.alerts
export AGENTBUS_URL=tcp://127.0.0.1:1883
export AGENTBUS_AGENT=weather-2
export AGENTBUS_TOKEN_FILE=/Users/you/agentbus-lab/host2/tokens/weather-2.token
```

The two status lines go to stderr and the `export` lines to stdout, so
you can keep the settings in a file. Run it once more, into a file, and
load it:

```bash
agentbus join --join-url http://127.0.0.1:8884 --agent weather-2 > ~/agentbus-lab/host2/weather-2.env
source ~/agentbus-lab/host2/weather-2.env
agentbus agents --plain
```

The same token that was refused in step 2 now connects, and `weather-2`
is in the list as `online`.

A script on a real host does this in one command with `--wait`, which
blocks until the operator answers:

```bash
agentbus join --join-url https://BROKER-HOST:8884 --agent worker-7 --wait 10m > worker-7.env
```

## 5. Use it: a second weather worker

The weather binary from lab 4 reads the same three variables, so it can
run as `weather-2` with no rebuild.

**Terminal 5:**

```bash
~/agentbus-lab/remote-host/weather -v
```

It logs `msg="agent serving" agent=weather-2`.

**Terminal 2** (as `me`): send the same task several times.

```bash
for i in 1 2 3 4 5 6; do
  agentbus task --plain -c weather.forecast --body '{"location":"Seattle, WA","days":1}' \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["from"])'
done
```

```text
weather-2
weather-2
weather
weather-2
weather-2
weather
```

The broker gives each task to one of the two workers. The split varies
from run to run. This is the answer to lab 4's second question: the same
binary, a different identity per host, the same capability.

## 6. What a client can't do

**Terminal 5:** stop the agent (`ctrl+c`), then clear the settings
you loaded, so the next commands don't run as `weather-2`:

```bash
unset AGENTBUS_URL AGENTBUS_AGENT AGENTBUS_TOKEN_FILE
```

Ask for an ID that already exists:

```bash
agentbus join --join-url http://127.0.0.1:8884 --agent weather
```

```text
agentbus: join refused: that agent id is taken; pick another, or use the token that first asked for it
```

A client can't take over an existing agent, and the first request for a
new ID holds that ID against anyone else. Now ask for every capability:

```bash
agentbus join --join-url http://127.0.0.1:8884 --agent visitor --capability '*'
```

```text
agentbus: invalid capability "*": use 1-64 of a-z 0-9 . _ -
```

A join can never ask for `*` or for the observer role. Only the operator
can give those: `--capability '*'` on approve, or
`agentbus config agent edit` for the observer role.

## 7. Deny one, from the editor

**Terminal 5:**

```bash
agentbus join --join-url http://127.0.0.1:8884 --agent visitor --description "no idea who this is"
```

If the console is open in terminal 4, it shows a notice that `visitor`
asked to join.

**Terminal 2:**

```bash
agentbus config
```

This is the config editor. Press `tab` twice for the **Join requests**
view; its name shows a count while anyone is waiting. Select `visitor`.
`enter` would open an approval form with the capabilities filled in for
you to trim. Press `d` to deny it, and `q` to quit. The console's Config
tab (`6`) has the same view.

The command-line form is `agentbus config join deny visitor`, which
prints `✓ denied visitor; nothing was added`.

A client that was waiting with `--wait` is told
`the request for visitor was denied, or expired unanswered`.

## Things to know

- **The joined agent has no token file on the broker.**
  `agentbus config agent list` shows `(none: rotate to create)` for
  `weather-2`. That's expected: its token is on the client. Rotating it
  on the broker would write a new token there and cut the client off.
- **Requests expire** after 24 hours unanswered.
- **Limits:** at most 4 requests wait from one address, and 64 in total.

[Joining the bus](../docs/JOINING.md) and the
[CLI reference](../docs/CLI.md#letting-an-agent-ask-to-join) cover TLS,
`--ca-file` and the rest.

## Clean up

To remove only what this lab added, in **terminal 2**:

```bash
agentbus config agent rm weather-2
rm -rf $LAB/host2
```

Or stop everything and `rm -rf $LAB`, as at the end of lab 6.

## Questions

1. The client sends a hash of its token. What could someone who read
   that request off the network do with the hash? What could they do if
   they could change the request on its way to the broker, and what
   stops that?
2. The client asked for `weather.alerts` and didn't get it. How would
   you grant it later? (Hint: `agentbus config agent edit --help`.)
3. Why does the first request for an ID hold it, even before anyone
   approves?

**Done when:** `weather-2` joined with a token that never left
terminal 5, it was granted one of the two capabilities it asked for, and
a join for `weather` was refused as taken.
