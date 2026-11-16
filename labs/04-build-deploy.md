# Lab 4: Build and deploy

**Goal:** turn the weather agent into one file you can copy to any host,
and run it the way a remote host would.

## 1. Build

**Terminal 2:**

```bash
agentbus agent build $LAB/weather
```

This compiles a static binary, with CGO off, trimmed and stripped. Inside
it are:
- `agent.yaml`
- everything in `scripts/`
- the Go handlers in `handlers/`
- the agentbus client

It prints the path, size (about 6 MiB) and SHA-256. Cross-compiling for
your servers is one flag:

```bash
agentbus agent build $LAB/weather --target linux/amd64,linux/arm64
ls -la $LAB/weather/dist/
```

The build also runs the binary for this machine with `-check`, so a
manifest that names a Go handler the binary doesn't have fails here,
rather than on the server.

## 2. Pretend to be a remote host

A host gets two things: the binary and its token. Nothing else: no
manifest, no scripts, no Python files.

First stop `agentbus agent run` in **terminal 3** (`ctrl+c`). An agent ID
is one process at a time, and the binary is about to use `weather`.

**Terminal 3:**

```bash
mkdir -p $LAB/remote-host && cd $LAB/remote-host
cp $LAB/weather/dist/weather-$(go env GOOS)-$(go env GOARCH) ./weather
install -m 0600 $LAB/tokens/weather.token ./weather.token
ls
```

On a real host the token goes to `/etc/agentbus/weather.token`, and you'd
copy it there over SSH. The broker host created it in lab 3.

## 3. Check, then run

```bash
unset AGENTBUS_CONFIG                         # a remote host has no broker config
export AGENTBUS_URL=tcp://127.0.0.1:1883      # where the broker is
export AGENTBUS_TOKEN_FILE=$PWD/weather.token
./weather -check
./weather -version
./weather -print-manifest | head -5
./weather -v
```

`-check` confirms that:
- the manifest is valid
- every Go handler is compiled in
- the interpreters exist on this host (python3 and sh)
- the token is readable

`-v` logs a line per message.

**Terminal 2:**

```bash
agentbus task -c weather.forecast --body '{"location":"Seattle, WA","days":1}'
agentbus agents
```

The forecast comes from the binary now. In `agentbus agents`, the
weather card's `meta` shows `runtime: agentbus-agent` and the host name;
use `agentbus agents` in a terminal and `enter`, then `i`, to inspect
the card.

## 4. Stop it like systemd would

**Terminal 3:** `ctrl+c` (systemd sends SIGTERM, which does the same):

```text
level=INFO msg="agent stopped" agent=weather
```

On shutdown the agent:
- answers new requests with `busy`
- waits up to `drain` (15s here) for running handlers, and for their
  replies to go out
- disconnects
- deletes the temporary directory it unpacked its scripts into

Start it again (`./weather -v`) for the next lab.

## 5. Read the systemd unit

```bash
cat $LAB/weather/deploy/weather.service
```

On a real Linux host:

```bash
sudo install -m 0755 weather-linux-amd64 /usr/local/bin/weather
sudo install -m 0600 -D weather.token /etc/agentbus/weather.token
sudo cp weather.service /etc/systemd/system/ && sudo systemctl enable --now weather
```

What the unit does:
- **The token** arrives as a systemd credential, readable only by the
  service.
- **The service** runs as a throwaway user.
- **The system** is read-only to it.
- **Stopping** leaves time for the drain.

## Questions

1. What's in the binary, and what isn't? Why is the token deliberately
   not built in?
2. You deploy the same binary to three build hosts. What do you change
   so the three don't knock each other off the bus? (Hint: identities,
   capabilities, and `AGENTBUS_AGENT`.)

**Done when:** the forecast comes from `./weather` running in
`$LAB/remote-host`, and it stops cleanly on `ctrl+c`.
