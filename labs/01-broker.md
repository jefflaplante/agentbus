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

Leave it running. It logs every connection and message, and it rereads
`broker.json` within a second of any change, so you won't restart it
again in these labs.

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
