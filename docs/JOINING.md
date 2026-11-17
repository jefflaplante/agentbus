# Joining the bus

There are two ways for an agent to get an identity on the bus.

| | Who creates the token | What the operator does |
|---|---|---|
| **Add** | the broker host, with `agentbus config agent add NAME` | adds the agent, then copies its token file to the agent's host |
| **Join** | the agent's host | approves a request |

Adding is described in [Onboarding an agent](CLI.md#onboarding-an-agent).
This page is about joining: a host asks the broker for an identity, and
gets nothing until a person approves the request on the broker. It covers
the `agentbus join` command, the HTTP endpoint behind it, and how to join
with `curl` or a few lines of Python when the `agentbus` binary isn't on
the host.

## How a join works

```text
New host                         Broker (join_listen)              Operator
   |                                   |                               |
   | make a random token,              |                               |
   | keep it in a file                 |                               |
   |                                   |                               |
   |-- POST /join -------------------->|                               |
   |   id, SHA-256 of the token,       | store the request             |
   |   capabilities it would like      | in pending/                   |
   |<-- 202 pending -------------------|                               |
   |                                   |<-- approve, choosing grants --|
   |                                   | add the agent to broker.json  |
   |                                   | and reload                    |
   |-- POST /join/status ------------->|                               |
   |<-- 200 approved, with the grants -|                               |
   |                                   |                               |
   |== MQTT CONNECT with the token ===>|                               |
```

Three properties follow from this.

- **The token never leaves the host that made it.** The broker stores the
  SHA-256 of every agent's token and nothing else, so the hash is all it
  needs. Nobody copies a secret between machines.
- **A request grants nothing.** Until it is approved the agent is not in
  the config, and its token is refused like any unknown token.
- **The operator decides the grants.** The capabilities in a request are
  what the host would like to serve. The operator can grant all of them,
  some, others, or none.

## Turn it on

The endpoint is off until `join_listen` is set. It needs a restart.

```bash
agentbus config set join_listen 127.0.0.1:8884     # this machine only
agentbus config set join_listen 0.0.0.0:8884       # other hosts; needs tls_cert and tls_key
```

`join_listen` follows the same rule as `listen`: loopback, or TLS, or
`allow_insecure`. Over plain HTTP, someone on the network path could
replace the token hash in a request with their own and be the one you
approve. It uses the broker's `tls_cert` and `tls_key`, and it may share
an address with `mcp_listen`.

Leave it unset when you aren't adding agents.

## Join with the CLI

On the new host:

```bash
agentbus join --join-url https://bus.example.com:8884 --agent worker-7 \
    --capability summarize --description "nightly batch box"
```

```text
✓ asked bus.example.com:8884 to add worker-7; the token stays in ~/.config/agentbus/tokens/worker-7.token
  Waiting for an operator. On the broker: agentbus config join approve worker-7
  Run this command again to see how the request stands, or add --wait 1h to block until it's answered.
```

`join` writes the token file before it sends anything, so the token
behind a request is never lost. Running it again reuses that file and
reports how the request stands. Once the request is approved, it prints
the agent's settings on stdout:

```text
$ agentbus join --join-url https://bus.example.com:8884 --agent worker-7 --capability summarize > worker-7.env
✓ worker-7 is approved: granted capabilities summarize
$ cat worker-7.env
export AGENTBUS_URL=ssl://bus.example.com:8883
export AGENTBUS_AGENT=worker-7
export AGENTBUS_TOKEN_FILE="$HOME/.config/agentbus/tokens/worker-7.token"
```

| Flag | |
|---|---|
| `--join-url` | the broker's `join_listen` address (`AGENTBUS_JOIN_URL`) |
| `--agent` | the agent ID to ask for (`AGENTBUS_AGENT`) |
| `--capability` | a task queue this agent would like to serve; repeat for more |
| `--description` | a note the operator sees next to the request |
| `--wait 1h` | block until the request is answered, for unattended setup |
| `--token-file` | where to keep the token; default `tokens/ID.token` in the config directory |
| `--ca-file` | a PEM certificate or CA to trust, for a broker with a private certificate |

For a provisioning script, `--wait` makes it one step: the script blocks,
the operator approves, and the script continues with the env file.

## Approve or deny

On the broker:

```text
$ agentbus config join
AGENT     ASKS FOR   FROM                  AGE  DESCRIPTION
worker-7  summarize  batch-03 (10.0.4.17)  12s  nightly batch box
```

```bash
agentbus config join approve worker-7                     # grant what it asked for
agentbus config join approve worker-7 --capability summarize --send-to conduit-home
agentbus config join approve worker-7 --capability=       # let it join, serve nothing
agentbus config join deny worker-7
```

Approving adds the agent to `broker.json` with the grants you chose. A
running broker reloads the file and accepts the agent within a second.

In the config editor (`agentbus config`) and the console's Config tab,
**Join requests** is the third view. `enter` opens a request with its
capabilities filled in for you to trim or change, and `d` denies it. The
tab shows a count while anyone is waiting, and the console shows a notice
when a request arrives.

Before you approve, read the `FROM` column. The host name is what the
client reported, so it is a label and proves nothing. The address is what
the broker saw. If you don't recognize either, ask the person who started
the host.

## Join without the agentbus CLI

The join endpoint is plain HTTP with JSON bodies, so any HTTP client can
use it. A host needs three things: a source of random bytes, SHA-256, and
an HTTP POST. This is the same exchange `agentbus join` makes.

### With curl

Make a token and keep it. Any string works as a token, but the broker
stores an unsalted SHA-256, which is only safe for long random tokens.
Use 32 random bytes:

```bash
(umask 077; openssl rand -hex 32 > sensor-12.token)
```

Hash it. The hash is over the token's characters with no trailing
newline, so strip the newline before hashing:

```bash
HASH=$(tr -d '\n' < sensor-12.token | sha256sum | cut -d' ' -f1)   # macOS: shasum -a 256
```

Ask to join:

```bash
curl -sS -X POST https://bus.example.com:8884/join \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "sensor-12",
    "token_sha256": "'"$HASH"'",
    "capabilities": ["temp.read"],
    "description": "greenhouse sensor",
    "host": "pi-zero-3"
  }'
```

```json
{"status":"pending","agent":"sensor-12"}
```

Ask how the request stands. Every few seconds is fine:

```bash
curl -sS -X POST https://bus.example.com:8884/join/status \
  -d '{"id": "sensor-12", "token_sha256": "'"$HASH"'"}'
```

```json
{"status":"approved","agent":"sensor-12","capabilities":["temp.read"],"mqtt_port":"8883","tls":true}
```

Once the status is `approved`, connect with any MQTT client: the client
ID and the username are the agent ID, and the password is the token.
With the Mosquitto tools:

```bash
mosquitto_sub -h bus.example.com -p 8883 --capath /etc/ssl/certs \
  -i sensor-12 -u sensor-12 -P "$(cat sensor-12.token)" \
  -t 'a2a/v1/agents/sensor-12/inbox' -q 1 -v
```

What to publish and subscribe to from there is the
[contract](../CONTRACT.md). [Desktop MQTT clients](DESKTOP-CLIENTS.md)
has the connection rules that catch generic clients out: the client ID
must equal the username, and a Will message is refused.

For a broker with a private certificate, give `curl` the CA with
`--cacert ca.crt`.

### As one script

This asks, waits for the answer, and exits 0 once the agent is approved.
It needs `curl`, `openssl` and `sha256sum` (or `shasum`), and no JSON
parser: the HTTP status code says how the request stands.

```bash
#!/bin/sh
# join.sh JOIN_URL AGENT_ID [CAPABILITY...]
# Asks an agentbus broker to add this host as AGENT_ID and waits for an
# operator to approve. The token is kept in ./AGENT_ID.token.
set -eu
url=${1%/}; id=$2; shift 2

token_file=$id.token
if [ ! -s "$token_file" ]; then
  (umask 077; openssl rand -hex 32 > "$token_file")
fi
if command -v sha256sum >/dev/null; then sum=sha256sum; else sum="shasum -a 256"; fi
hash=$(tr -d '\n' < "$token_file" | $sum | cut -d' ' -f1)

caps=""
for c in "$@"; do caps="$caps${caps:+,}\"$c\""; done
body="{\"id\":\"$id\",\"token_sha256\":\"$hash\",\"capabilities\":[$caps],\"host\":\"$(hostname)\"}"

# Prints the reply body, then the HTTP status code on a line of its own.
post() {
  curl -sS -w '\n%{http_code}' -X POST "$url$1" \
    -H 'Content-Type: application/json' -d "$body"
}

out=$(post /join)
while :; do
  code=$(printf '%s\n' "$out" | tail -n 1)
  reply=$(printf '%s\n' "$out" | sed '$d')
  case $code in
    200) echo "approved: $reply"; exit 0 ;;
    202) echo "waiting for an operator: agentbus config join approve $id" >&2 ;;
    404) echo "the request for $id was denied, or expired" >&2; exit 1 ;;
    *)   echo "join failed ($code): $reply" >&2; exit 1 ;;
  esac
  sleep 5
  out=$(post /join/status)
done
```

```text
$ sh join.sh https://bus.example.com:8884 sensor-12 temp.read
waiting for an operator: agentbus config join approve sensor-12
waiting for an operator: agentbus config join approve sensor-12
approved: {"status":"approved","agent":"sensor-12","capabilities":["temp.read"],"mqtt_port":"8883","tls":true}
```

### With Python

The standard library is enough:

```python
import hashlib, json, os, secrets, socket, time, urllib.error, urllib.request

JOIN_URL = "https://bus.example.com:8884"
AGENT = "sensor-12"

token_file = f"{AGENT}.token"
if not os.path.exists(token_file):
    fd = os.open(token_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as f:
        f.write(secrets.token_hex(32) + "\n")
with open(token_file) as f:
    token = f.read().strip()

request = {
    "id": AGENT,
    "token_sha256": hashlib.sha256(token.encode()).hexdigest(),
    "capabilities": ["temp.read"],
    "host": socket.gethostname(),
}

def post(path):
    req = urllib.request.Request(JOIN_URL + path, data=json.dumps(request).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            return resp.status, json.load(resp)
    except urllib.error.HTTPError as e:      # 4xx and 5xx carry a JSON body too
        return e.code, json.load(e)

code, reply = post("/join")
while code == 202:                           # pending
    time.sleep(5)
    code, reply = post("/join/status")
if code != 200:
    raise SystemExit(f"join failed ({code}): {reply.get('error', reply.get('status'))}")
print("approved; granted", reply.get("capabilities", []))
# connect with paho-mqtt: client_id=AGENT, username=AGENT, password=token
```

## The join endpoint

Two paths, both `POST` with a JSON body of at most 16 KB. There is no
authentication. What protects the bus is that a request grants nothing.

### `POST /join`

Creates a request, or reports on the one this token already made.

| Field | | |
|---|---|---|
| `id` | required | the agent ID wanted: 1-64 of `a-z 0-9 . _ -` |
| `token_sha256` | required | hex SHA-256 of the token, 64 characters |
| `capabilities` | optional | task queues it would like to serve, at most 32. `*` is refused |
| `description` | optional | a note for the operator, at most 200 bytes |
| `host` | optional | the host's name for itself, at most 200 bytes |

Unknown fields are ignored. Characters that don't print are dropped from
`description` and `host`. The broker records the address the request came
from and the time by itself; a client can't set them.

### `POST /join/status`

Takes `id` and `token_sha256` and reports how the request stands. It
never creates one, so a denied client that keeps polling does not put
itself back in the queue.

### Replies

Every reply is a JSON object.

| Status | Body | Meaning |
|---|---|---|
| `202` | `{"status":"pending","agent":ID}` | waiting for the operator |
| `200` | `{"status":"approved","agent":ID,...}` | in the config; connect with the token |
| `404` | `{"status":"unknown","agent":ID}` | `/join/status` only: never asked, denied, or expired |
| `400` | `{"error":...}` | the request is malformed; the message says how |
| `409` | `{"agent":ID,"error":...}` | the ID belongs to a different token, waiting or already an agent |
| `429` | `{"error":...}` | too many requests, or too many waiting, from this address |
| `503` | `{"error":...}` | too many requests are waiting in all |

An `approved` reply also carries what was granted and how to reach the
bus on the host the client already used:

| Field | |
|---|---|
| `capabilities` | the task queues granted; absent when none |
| `send_to` | the agents it may message; absent when it may message anyone |
| `mqtt_port` | the port of the broker's MQTT listener |
| `tls` | `true` when that listener uses TLS; absent otherwise |

Compare `capabilities` with what you asked for. The operator may have
granted less, and subscribing to a task queue you weren't granted is
refused.

The token hash is what ties a request to its owner. `/join` and
`/join/status` answer `pending` or `approved` only when `token_sha256`
matches the one that first asked for the ID.

## Limits, stated plainly

- **A person has to approve.** There is no join token that lets a fleet in
  unattended. That is deliberate for now: a leaked secret can't mint
  agents. The cost is that every new host takes an operator a few seconds.
- **Anyone who can reach the endpoint can ask.** The broker bounds what
  they can make it store: 64 requests waiting in all, 4 from one address,
  60 HTTP requests a minute from one address, 16 KB each. Requests expire
  after 24 hours.
- **The first request for an ID holds it** until it is answered or
  expires. A stranger can hold a name for a day. Deny the request to free
  it.
- **A denied client isn't told why, and can ask again.** `/join/status`
  answers `unknown` for a denied request, the same as for one that
  expired. Running `agentbus join` again after a denial sends a new
  request. `agentbus join` also writes the token file before it asks, so a
  refused request leaves a token file behind that is good for nothing.
- **A join can't ask for everything.** `*` and the observer role are
  refused. An operator can still grant them afterwards with
  `agentbus config agent edit`.
- **The broker has no token file for a joined agent.** `agentbus config
  agent list` shows `(none: rotate to create)` and the editor shows
  `missing`. That is expected. Rotating the token on the broker would
  write a new one there and cut the client off. To rotate, remove the
  agent and have the host join again with a new token.
- **Join requests are in the broker's log, not the audit file.** The
  approval shows up there as a config reload that adds the agent.
- **The host name and description are whatever the client sent.** Only
  the address is the broker's own observation, and behind NAT or a proxy
  it is the proxy's.

Join is specific to agentbus. It is not part of the
[a2a/v1 contract](../CONTRACT.md), which starts once an agent has an ID
and a token.
