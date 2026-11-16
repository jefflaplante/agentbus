# Lab 0: Setup

**Goal:** agentbus on your PATH and a lab workspace that every later lab
uses.

## 1. Install agentbus

Clone the repository. The labs use files from it, and the weather agent
builds against it:

```bash
git clone https://github.com/jefflaplante/agentbus.git ~/src/agentbus
cd ~/src/agentbus
go build -o bin/agentbus ./cmd/agentbus
```

`agentbus` is the broker, the CLI client, the terminal UIs, the MCP server
and the agent generator in one binary.

> No clone needed just for the binary:
> `go install github.com/jefflaplante/agentbus/cmd/agentbus@latest`.
> You still need the repository for the lab files.

## 2. Set up the workspace

Put these lines somewhere you can paste them again. **Every new terminal
in these labs needs them.**

```bash
export REPO=~/src/agentbus                # where you cloned it
export LAB=~/agentbus-lab                 # everything the labs create
export AGENTBUS_CONFIG=$LAB/broker.json   # the broker's config, and where agent tokens go
export PATH=$REPO/bin:$PATH
```

`AGENTBUS_CONFIG` keeps the labs out of your real setup. Without it,
agentbus uses `~/.config/agentbus/`.

```bash
mkdir -p $LAB
agentbus --help
```

You should see the command groups: Broker, Setup, Messaging, Observing
and Standalone agents.

## 3. Check

```bash
go version        # go1.26 or newer
python3 --version
claude --version  # for lab 5
```

**Done when:** `agentbus --help` runs, and `echo $AGENTBUS_CONFIG` prints
a path under `$LAB`.
