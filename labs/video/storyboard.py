"""The video's scenes: narration, diagrams, and the terminal commands shown.

This file is the single source for the voice-over (make_video.py tts), the
terminal recordings (make_video.py record) and SCRIPT.md (make_video.py
script). Edit it and rebuild; nothing else holds narration text.

A scene is a diagram (an SVG under diagrams/, shown while its beats are
spoken) or a terminal session. A terminal beat says something, then
optionally types and runs a command:

  run   type the command and run it to completion
  bg    type it and start it in the background (a name, then the command)
  stop  stop a background command by name (shown as `show`, e.g. kill %1)
  show  the text typed on screen, when it differs from what runs
  hide  run this first without showing it (setup the viewer doesn't need)
  hold  seconds to keep the output on screen after the command finishes

Terminal scenes share one workspace (LAB) and their background processes,
so the broker started in one scene is still running in the next.
"""

TITLE = "agentbus: a broker, a custom agent, and Claude Code"

# How the narrator should say words it would otherwise mangle. Applied to
# the narration only; SCRIPT.md and the screen keep the written form.
# Each pair is a case-insensitive whole-word regex and its spoken form.
PRONOUNCE = [
    (r"AGENTBUS_CONFIG", "agent bus config"),
    (r"agentbus", "agent bus"),
]

ASK = ('claude -p "What\'s the weather forecast for Seattle, WA?" \\\n'
       '    --mcp-config .mcp.json --strict-mcp-config --setting-sources project \\\n'
       '    --allowedTools mcp__agentbus__agents,mcp__agentbus__request,mcp__agentbus__task')

LAB3_MANIFEST = r"""cat > weather/agent.yaml <<'EOF'
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
EOF"""

SCENES = [
    {
        "id": "01-title", "kind": "diagram", "svg": "title.svg",
        "beats": [
            "This is a hands-on tour of agentbus, a message bus for AI agents. "
            "In the next few minutes we'll start a broker, build a small weather agent, "
            "deploy it as a single binary, and then watch Claude Code use it to answer one question: "
            "what's the weather forecast for Seattle, Washington?",
        ],
    },
    {
        "id": "02-why", "kind": "diagram", "svg": "why-bus.svg",
        "beats": [
            "Agents need to talk to each other: a gateway like conduit, coding sessions, "
            "and small workers on servers. Wiring every pair together directly means every pair "
            "needs addresses, credentials and retries.",
            "A message bus fixes the addressing. agentbus goes one step further. Every agent has its own "
            "identity and token, and the broker checks who sent each message and who may talk to whom. "
            "So an agent can trust who a message is from.",
        ],
    },
    {
        "id": "03-map", "kind": "diagram", "svg": "lab-map.svg",
        "beats": [
            "Here's what we'll build. In the middle, the broker. On the right, a weather agent: "
            "a Python script that calls the National Weather Service, packaged into one binary. "
            "On the left, two clients: you, at a terminal, and Claude Code, connected through agentbus's "
            "MCP server. To the broker, all three are just agents.",
        ],
    },
    {
        "id": "04-broker", "kind": "terminal", "title": "Lab 1 · Start a broker",
        "beats": [
            {"say": "Everything is one binary, called agentbus. Setting AGENTBUS_CONFIG keeps this demo "
                    "in its own folder, away from your real configuration.",
             "run": "export AGENTBUS_CONFIG=$LAB/broker.json"},
            {"say": "config init writes a broker config: a listener on localhost, and an admin identity "
                    "whose token goes in a file that only you can read.",
             "run": "agentbus config init", "hold": 1.5},
            {"say": "Start the broker. Here it runs in the background; normally you'd give it its own terminal.",
             "bg": ["broker", "agentbus serve > broker.log 2>&1"], "show": "agentbus serve > broker.log 2>&1 &"},
            {"say": "status connects as admin, with the token it finds next to the config, "
                    "and shows what the broker sees: where it's listening, and who's online.",
             "hide": "sleep 1.5",
             "run": "agentbus status --plain | jq '{listen, agents: [.agents[] | {id, state}]}'", "hold": 2},
        ],
    },
    {
        "id": "05-anatomy", "kind": "diagram", "svg": "agent-anatomy.svg",
        "beats": [
            "An agent is a manifest. Each handler pairs a trigger with an action. "
            "The trigger is a task queue, an inbox op, or an event. "
            "The action is a shell command, a script, or a Go function.",
            "A script gets the whole message as JSON on standard input, and writes its reply to standard output. "
            "To refuse with a specific error, it prints a code and a reason, and exits with status three.",
        ],
    },
    {
        "id": "06-weather", "kind": "terminal", "title": "Lab 3 · The weather agent",
        "beats": [
            {"say": "Generate a project from the template, and swap its examples for the forecast script. "
                    "It uses only Python's standard library, so it runs on any host with Python.",
             "run": "agentbus agent init weather --dir weather --agentbus-src $REPO >/dev/null && "
                    "cp $REPO/labs/weather-agent/scripts/forecast.py weather/scripts/",
             "show": "agentbus agent init weather --dir weather --agentbus-src $REPO\n"
                     "cp $REPO/labs/weather-agent/scripts/forecast.py weather/scripts/",
             "hide_after": "rm weather/scripts/disk.sh weather/scripts/wordcount.py"},
            {"say": "The manifest serves the script two ways: as the weather dot forecast task, "
                    "for anyone who wants a forecast, and as an inbox op, for callers who ask this agent by name. "
                    "The descriptions go on the agent's presence card. That's how other agents learn what to send.",
             "run": LAB3_MANIFEST, "hold": 2.5, "fast": True},
            {"say": "Give the agent an identity with that capability, and one for us. "
                    "Then run it straight from disk.",
             "run": "agentbus config agent add weather --capability weather.forecast >/dev/null && "
                    "agentbus config agent add me >/dev/null",
             "show": "agentbus config agent add weather --capability weather.forecast\nagentbus config agent add me",
             "hide_after": "sleep 1.5"},
            {"say": "", "bg": ["weather-run", "agentbus agent run -f weather/agent.yaml > weather.log 2>&1"],
             "show": "agentbus agent run -f weather/agent.yaml > weather.log 2>&1 &", "hide_after": "sleep 2"},
            {"say": "Now, as the user we just created, ask for the forecast.",
             "run": "export AGENTBUS_AGENT=me"},
            {"say": "The task goes to the broker, the broker hands it to the weather agent, "
                    "and the script's answer comes back: the National Weather Service forecast for Seattle.",
             "run": "agentbus task -c weather.forecast --body '{\"location\":\"Seattle, WA\",\"days\":1}'",
             "hold": 4},
            {"say": "The failures are just as useful. An unknown place gets not found. "
                    "And input that looks like a shell command is refused before it goes anywhere.",
             "run": "agentbus task -c weather.forecast --body '{\"location\":\"Nowhereville, ZZ\"}'; "
                    "agentbus task -c weather.forecast --body '{\"location\":\"x; rm -rf ~\"}'",
             "show": "agentbus task -c weather.forecast --body '{\"location\":\"Nowhereville, ZZ\"}'\n"
                     "agentbus task -c weather.forecast --body '{\"location\":\"x; rm -rf ~\"}'",
             "hold": 2.5},
        ],
    },
    {
        "id": "07-deploy", "kind": "diagram", "svg": "deploy.svg",
        "beats": [
            "agent build compiles the project into one static binary per target. "
            "Inside it: the manifest, the scripts, any Go handlers, and the agentbus client. "
            "Not inside it: the token. Each host brings its own, so the same binary can go anywhere.",
        ],
    },
    {
        "id": "08-build", "kind": "terminal", "title": "Lab 4 · Build and deploy",
        "beats": [
            {"say": "First stop the agent we ran from disk. An agent ID is one process at a time.",
             "stop": "weather-run", "show": "kill %1"},
            {"say": "Build it, for this Mac and for Linux servers.",
             "run": "agentbus agent build weather --target darwin/arm64,linux/amd64", "hold": 2},
            {"say": "A remote host needs two files: the binary, and its token.",
             "run": "mkdir host && cp weather/dist/weather-darwin-arm64 host/weather && "
                    "cp $LAB/tokens/weather.token host/ && du -h host/*",
             "show": "mkdir host && cp weather/dist/weather-darwin-arm64 host/weather\n"
                     "cp $LAB/tokens/weather.token host/ && du -h host/*", "hold": 2},
            {"say": "On the host, the environment says which agent this is and where its token lives. "
                    "check confirms the manifest, the interpreters and the token, before you start it.",
             "run": "cd host"},
            {"say": "", "run": "AGENTBUS_AGENT=weather AGENTBUS_TOKEN_FILE=weather.token ./weather -check", "hold": 2},
            {"say": "Run it, and ask again. Same answer, now from the binary.",
             "bg": ["weather-bin", "AGENTBUS_AGENT=weather AGENTBUS_TOKEN_FILE=weather.token ./weather -v > weather.log 2>&1"],
             "show": "AGENTBUS_AGENT=weather AGENTBUS_TOKEN_FILE=weather.token ./weather -v > weather.log 2>&1 &",
             "hide_after": "sleep 2"},
            {"say": "",
             "run": "agentbus task --plain -c weather.forecast --body '{\"location\":\"Seattle, WA\",\"days\":1}' "
                    "| jq '.body | {location, source, tonight: .periods[0].detail}'",
             "show": "agentbus task --plain -c weather.forecast --body '{\"location\":\"Seattle, WA\",\"days\":1}' \\\n"
                     "    | jq '.body | {location, source, tonight: .periods[0].detail}'",
             "hold": 3.5},
            {"say": "", "run": "cd .."},
        ],
    },
    {
        "id": "09-mcp", "kind": "diagram", "svg": "claude-mcp.svg",
        "beats": [
            "Claude Code reaches the bus through agentbus mcp, a small MCP server that gives the model "
            "seven tools, such as agents, request and task. It connects with its own identity, claude dash code, "
            "and the broker treats it exactly like any other agent.",
            "Everything another agent sends back reaches Claude marked as untrusted input: evidence, never instructions.",
        ],
    },
    {
        "id": "10-claude", "kind": "terminal", "title": "Lab 5 · Claude Code asks the weather agent",
        "beats": [
            {"say": "Give Claude an identity, and a project folder with an MCP config that starts agentbus mcp.",
             "run": "agentbus config agent add claude-code >/dev/null && mkdir claude && "
                    "cp $REPO/labs/claude/mcp.json claude/.mcp.json && cat claude/.mcp.json",
             "show": "agentbus config agent add claude-code\n"
                     "mkdir claude && cp $REPO/labs/claude/mcp.json claude/.mcp.json && cat claude/.mcp.json",
             "hide_after": "sleep 1.5", "hold": 2},
            {"say": "First, the plain question, with no other guidance.", "run": "cd claude"},
            {"say": "Claude has the tools now, but nothing tells it that other agents on the bus can answer "
                    "questions like this one. Watch what it does.",
             "run": ASK, "hold": 4},
            {"say": "It didn't think of the bus. Having a tool isn't the same as knowing when to use it. "
                    "So add a project CLAUDE dot M D that says other agents serve live data, "
                    "and to check the agents tool first.",
             "run": "cp $REPO/labs/claude/CLAUDE.md . && sed -n '1,13p' CLAUDE.md", "hold": 3},
            {"say": "Ask again. This time Claude calls agents, finds the weather agent and the body shape "
                    "in its description, and sends the weather dot forecast task.",
             "run": ASK, "hold": 6},
            {"say": "That's the National Weather Service forecast, through our agent, on the bus. "
                    "And the weather agent's log shows who asked: claude code, an identity the broker checked.",
             "run": "grep -o 'msg=handled handler=[^ ]* from=claude-code' ../host/weather.log | tail -1", "hold": 3},
        ],
    },
    {
        "id": "11-recap", "kind": "diagram", "svg": "recap.svg",
        "beats": [
            "To recap. A broker that checks every identity. An agent that's a manifest, scripts and Go functions, "
            "built into one file you can copy anywhere. And Claude Code, joining as one more agent, "
            "once it knows the bus is there.",
            "The labs folder in the repository walks through all of it step by step, with a teaching script, "
            "the finished weather agent, and a checker that runs every lab for you.",
        ],
    },
]
