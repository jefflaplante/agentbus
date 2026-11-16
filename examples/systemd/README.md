# systemd units

Units for running agentbus on Linux servers. Each file's header comment has
its setup commands.

| Unit | Runs | On |
|---|---|---|
| [agentbus.service](agentbus.service) | the broker, as the `agentbus` system user, with config and tokens in `/etc/agentbus` and data in `/var/lib/agentbus` | the broker host |
| [agentbus-handle@.service](agentbus-handle@.service) | an agent run by `agentbus handle`, one instance per agent: `agentbus-handle@researcher` | any host |
| [counter.service](counter.service) | an agent built with the Go client: [examples/service](../service/main.go) | any host |
| [counter-python.service](counter-python.service) | the same agent in Python: [examples/python](../python/worker.py) | any host |

Standalone agents made with `agentbus agent init` get their own unit in
`deploy/NAME.service`. See [docs/AGENTS.md](../../docs/AGENTS.md). Agents
with `llm:` handlers also need a model key. The generated unit shows how to
pass it as a credential, the same way as the token; see
[Deploying](../../docs/AGENTS.md#deploying).

## Install the binary

On each host, from a checkout:

```bash
make install            # /usr/local/bin/agentbus, which the units expect; may need sudo
```

Or copy a binary built elsewhere, for example
`GOOS=linux GOARCH=amd64 go build -o agentbus ./cmd/agentbus`.

## The broker

The setup is in the header of [agentbus.service](agentbus.service). In
short: a system user, `/etc/agentbus` and `/var/lib/agentbus` owned by it,
`agentbus config init` with `AGENTBUS_CONFIG_DIR` and `AGENTBUS_STATE_DIR`
set, then `systemctl enable --now agentbus`.

Before agents on other hosts can connect, set a non-loopback `listen`
with `tls_cert` and `tls_key`, then restart:

```bash
sudo -u agentbus env AGENTBUS_CONFIG_DIR=/etc/agentbus agentbus config set \
  tls_cert /etc/agentbus/cert.pem tls_key /etc/agentbus/key.pem listen 0.0.0.0:8883
sudo systemctl restart agentbus
```

## An agent

1. **Create its identity on the broker host.** This writes the token and
   prints where it is and how to start the agent:

   ```bash
   sudo -u agentbus env AGENTBUS_CONFIG_DIR=/etc/agentbus agentbus config agent add researcher --capability research
   ```

2. **Copy the token to the agent's host** as `/etc/agentbus/researcher.token`,
   mode 0600. Every agent unit here loads it from there as a systemd
   credential, so only that service can read it. On the broker host itself
   the token is already in `/etc/agentbus/tokens/`; point `LoadCredential=`
   at it with `systemctl edit`.

3. **Install a unit and start it.** For a shell command, use the template:

   ```bash
   sudo install -D -m 0644 /dev/stdin /etc/agentbus/agents/researcher.env <<'ENV'
   AGENTBUS_URL=ssl://bus.example.com:8883
   CAPABILITY=research
   EXEC=/usr/local/lib/agents/research.sh
   ENV
   sudo cp agentbus-handle@.service /etc/systemd/system/
   sudo systemctl enable --now agentbus-handle@researcher
   ```

   For a program of your own, copy [counter.service](counter.service) and
   change `ExecStart`, the agent ID and the token path.

4. **Check it.** `journalctl -u agentbus-handle@researcher` on the agent's
   host, and the Agents tab of `agentbus console` on the broker host.

Agent units run as a throwaway user (`DynamicUser=yes`) with a read-only
view of the system. If the agent's command has to write somewhere, add
`ReadWritePaths=` for it, or run it as a user that owns what it changes.

The units pass `systemd-analyze verify` on RHEL 9 (systemd 252). That
checks their syntax and that each `ExecStart` exists, not that the agent
runs; check that with `journalctl` after the first start.
