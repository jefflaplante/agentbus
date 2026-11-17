# Lab 1: The broker

**Goal:** a running broker, and a look inside it.

The broker is an MQTT server that enforces a contract between agents:
- every agent logs in with its own ID and token
- the broker stamps who sent each message, so nobody can impersonate
  another agent
- each agent can reach only the agents and queues its config allows

## 1. Create the config

**Terminal 1:**

```bash
agentbus config init
agentbus config path
```

```text
✓ created /Users/you/agentbus-lab/broker.json
  admin token: /Users/you/agentbus-lab/tokens/admin.token
  listen:      127.0.0.1:1883
```

`config path` then lists the config, the tokens directory and the
broker's data and audit files, all under `$LAB`. If any of them is under
`~/.config/agentbus`, `AGENTBUS_CONFIG` isn't set: paste the lab 0
`export` lines and run `config init` again.

`config init` writes `$LAB/broker.json` with:
- a listener on `127.0.0.1:1883`. It's loopback only, so it's safe
  without TLS.
- an identity called `admin`. It's an observer: it can watch all
  traffic. Its token is in `$LAB/tokens/admin.token` (mode 0600).
- storage under `$LAB/state`.

```bash
cat $LAB/broker.json
```

Note `token_sha256`: the broker stores only a hash of each token.

> Port 1883 taken? `agentbus config set listen 127.0.0.1:1884`. Every
> command finds the address from the config, so nothing else changes.

## 2. Start it

**Terminal 1:**

```bash
agentbus serve
```

The last line it prints is:

```text
level=INFO msg="agentbus broker listening" addr=127.0.0.1:1883 agents=1 tls=false config=...
```

Leave it running. It logs every connection and message, and it rereads
`broker.json` within a second of any change, so adding an agent needs no
restart. Only the listener settings need one: labs 6 and 7 each change
one and restart the broker.

> `address already in use` means something else has port 1883. Change the
> port as in step 1 and run `agentbus serve` again.

## 3. Look inside

**Terminal 2** (paste the lab 0 `export` lines first):

```bash
agentbus status
```

This opens the console on its Overview tab: a live view of the broker.
`admin` is online because the console itself connects as `admin`: with no
agent named, the CLI uses the admin token it finds next to the config.
Press `2` for the Agents tab, `?` for every key, and `q` to quit. The same
snapshot is available as JSON:

```bash
agentbus status --plain | python3 -m json.tool | head -30
```

## Questions

1. Where does the CLI get the broker's address and a token, when you
   gave it neither? (Hint: `AGENTBUS_CONFIG`.)
2. Why is `listen` loopback by default? What would you need before
   listening on `0.0.0.0`? (See the warning `serve` prints, and
   `docs/SECURITY.md`.)

**Done when:** `agentbus status` shows the broker up with `admin` online.
