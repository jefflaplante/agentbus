# agentbus: a broker, a custom agent, and Claude Code: video script

The narration and on-screen commands of the lab video, generated from
`storyboard.py` by `make_video.py script`. Edit the storyboard, not this file.

## 1. Introduction

*diagram: title.svg*

> This is a hands-on tour of agentbus, a message bus for AI agents. In the next few minutes we'll start a broker, build a small weather agent, deploy it as a single binary, and then watch Claude Code use it to answer one question: what's the weather forecast for Seattle, Washington?

## 2. Why a bus

*diagram: why-bus.svg*

> Agents need to talk to each other: a gateway like conduit, coding sessions, and small workers on servers. Wiring every pair together directly means every pair needs addresses, credentials and retries.

> A message bus fixes the addressing. agentbus goes one step further. Every agent has its own identity and token, and the broker checks who sent each message and who may talk to whom. So an agent can trust who a message is from.

## 3. What we'll build

*diagram: lab-map.svg*

> Here's what we'll build. In the middle, the broker. On the right, a weather agent: a Python script that calls the National Weather Service, packaged into one binary. On the left, two clients: you, at a terminal, and Claude Code, connected through agentbus's MCP server. To the broker, all three are just agents.

## 4. Lab 1 · Start a broker

*terminal*

> Everything is one binary, called agentbus. Setting AGENTBUS_CONFIG keeps this demo in its own folder, away from your real configuration.

```bash
export AGENTBUS_CONFIG=$LAB/broker.json
```

> config init writes a broker config: a listener on localhost, and an admin identity whose token goes in a file that only you can read.

```bash
agentbus config init
```

> Start the broker. Here it runs in the background; normally you'd give it its own terminal.

```bash
agentbus serve > broker.log 2>&1 &
```

> status connects as admin, with the token it finds next to the config, and shows what the broker sees: where it's listening, and who's online.

```bash
agentbus status --plain | jq '{listen, agents: [.agents[] | {id, state}]}'
```

## 5. Anatomy of an agent

*diagram: agent-anatomy.svg*

> An agent is a manifest. Each handler pairs a trigger with an action. The trigger is a task queue, an inbox op, or an event. The action is a shell command, a script, or a Go function.

> A script gets the whole message as JSON on standard input, and writes its reply to standard output. To refuse with a specific error, it prints a code and a reason, and exits with status three.

## 6. Lab 3 · The weather agent

*terminal*

> Generate a project from the template, and swap its examples for the forecast script. It uses only Python's standard library, so it runs on any host with Python.

```bash
agentbus agent init weather --dir weather --agentbus-src $REPO
cp $REPO/labs/weather-agent/scripts/forecast.py weather/scripts/
```

> The manifest serves the script two ways: as the weather dot forecast task, for anyone who wants a forecast, and as an inbox op, for callers who ask this agent by name. The descriptions go on the agent's presence card. That's how other agents learn what to send.

```bash
cat > weather/agent.yaml <<'EOF'
agent: weather
description: Weather forecasts for any city, from public weather services
timeout: 30s
handlers:
  - task: weather.forecast
    script: scripts/forecast.py
    description: 'body {"location": "City, ST", "days"?: 1-7}; replies {"location", "source", "periods"}'
  - inbox: true
    op: forecast
    script: scripts/forecast.py
    description: '{"op": "forecast", "location": string}; same reply as task weather.forecast'
EOF
```

> Give the agent an identity with that capability, and one for us. Then run it straight from disk.

```bash
agentbus config agent add weather --capability weather.forecast
agentbus config agent add me
```

```bash
agentbus agent run -f weather/agent.yaml > weather.log 2>&1 &
```

> Now, as the user we just created, ask for the forecast.

```bash
export AGENTBUS_AGENT=me
```

> The task goes to the broker, the broker hands it to the weather agent, and the script's answer comes back: the National Weather Service forecast for Seattle.

```bash
agentbus task -c weather.forecast --body '{"location":"Seattle, WA","days":1}'
```

> The failures are just as useful. An unknown place gets not found. And input that looks like a shell command is refused before it goes anywhere.

```bash
agentbus task -c weather.forecast --body '{"location":"Nowhereville, ZZ"}'
agentbus task -c weather.forecast --body '{"location":"x; rm -rf ~"}'
```

## 7. One binary, no token inside

*diagram: deploy.svg*

> agent build compiles the project into one static binary per target. Inside it: the manifest, the scripts, any Go handlers, and the agentbus client. Not inside it: the token. Each host brings its own, so the same binary can go anywhere.

## 8. Lab 4 · Build and deploy

*terminal*

> First stop the agent we ran from disk. An agent ID is one process at a time.

```bash
kill %1
```

> Build it, for this Mac and for Linux servers.

```bash
agentbus agent build weather --target darwin/arm64,linux/amd64
```

> A remote host needs two files: the binary, and its token.

```bash
mkdir host && cp weather/dist/weather-darwin-arm64 host/weather
cp $LAB/tokens/weather.token host/ && du -h host/*
```

> On the host, the environment says which agent this is and where its token lives. check confirms the manifest, the interpreters and the token, before you start it.

```bash
cd host
```

```bash
AGENTBUS_AGENT=weather AGENTBUS_TOKEN_FILE=weather.token ./weather -check
```

> Run it, and ask again. Same answer, now from the binary.

```bash
AGENTBUS_AGENT=weather AGENTBUS_TOKEN_FILE=weather.token ./weather -v > weather.log 2>&1 &
```

```bash
agentbus task --plain -c weather.forecast --body '{"location":"Seattle, WA","days":1}' \
    | jq '.body | {location, source, tonight: .periods[0].detail}'
```

```bash
cd ..
```

## 9. Claude Code on the bus

*diagram: claude-mcp.svg*

> Claude Code reaches the bus through agentbus mcp, a small MCP server that gives the model seven tools, such as agents, request and task. It connects with its own identity, claude dash code, and the broker treats it exactly like any other agent.

> Everything another agent sends back reaches Claude marked as untrusted input: evidence, never instructions.

## 10. Lab 5 · Claude Code asks the weather agent

*terminal*

> Give Claude an identity, and a project folder with an MCP config that starts agentbus mcp.

```bash
agentbus config agent add claude-code
mkdir claude && cp $REPO/labs/claude/mcp.json claude/.mcp.json && cat claude/.mcp.json
```

> First, the plain question, with no other guidance.

```bash
cd claude
```

> Claude has the tools now, but nothing tells it that other agents on the bus can answer questions like this one. Watch what it does.

```bash
claude -p "What's the weather forecast for Seattle, WA?" \
    --mcp-config .mcp.json --strict-mcp-config --setting-sources project \
    --allowedTools mcp__agentbus__agents,mcp__agentbus__request,mcp__agentbus__task
```

> It didn't think of the bus. Having a tool isn't the same as knowing when to use it. So add a project CLAUDE dot M D that says other agents serve live data, and to check the agents tool first.

```bash
cp $REPO/labs/claude/CLAUDE.md . && sed -n '1,13p' CLAUDE.md
```

> Ask again. This time Claude calls agents, finds the weather agent and the body shape in its description, and sends the weather dot forecast task.

```bash
claude -p "What's the weather forecast for Seattle, WA?" \
    --mcp-config .mcp.json --strict-mcp-config --setting-sources project \
    --allowedTools mcp__agentbus__agents,mcp__agentbus__request,mcp__agentbus__task
```

> That's the National Weather Service forecast, through our agent, on the bus. And the weather agent's log shows who asked: claude code, an identity the broker checked.

```bash
grep -o 'msg=handled handler=[^ ]* from=claude-code' ../host/weather.log | tail -1
```

## 11. Recap

*diagram: recap.svg*

> To recap. A broker that checks every identity. An agent that's a manifest, scripts and Go functions, built into one file you can copy anywhere. And Claude Code, joining as one more agent, once it knows the bus is there.

> The labs folder in the repository walks through all of it step by step, with a teaching script, the finished weather agent, and a checker that runs every lab for you.
