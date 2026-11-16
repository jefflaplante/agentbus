# agentbus labs: a broker, a custom agent, and Claude Code

In these labs you run an agentbus broker, write and deploy a weather
agent, and have Claude Code ask that agent for the forecast in Seattle,
WA. By the end you have built every piece of this:

```text
  Claude Code ──(MCP: agentbus mcp)──┐
                                     ▼
  you, in a terminal ───────────► broker ◄─────── weather agent (one binary)
  (agentbus CLI)                (agentbus serve)    ├─ forecast.py  → api.weather.gov
                                                    └─ current.go   → open-meteo.com
```

| Lab | You will | Time |
|---|---|---|
| [0. Setup](00-setup.md) | Install agentbus and set up a lab workspace | 10 min |
| [1. The broker](01-broker.md) | Start a broker, look inside it | 10 min |
| [2. A first agent](02-first-agent.md) | Generate an agent from the template, send it requests and tasks | 20 min |
| [3. The weather agent](03-weather-agent.md) | Write a forecast handler in Python and serve it | 25 min |
| [4. Build and deploy](04-build-deploy.md) | Compile the agent to one binary and run it as a remote host would | 15 min |
| [5. Claude Code](05-claude-code.md) | Connect Claude Code to the bus and ask for the Seattle forecast | 20 min |
| [6. Extend it](06-extend.md) | Add a Go handler, an allow-list, and look at request metrics; optionally, a model with your script as its tool | 20 min |

That's about two hours. Instructors: [TEACHING.md](TEACHING.md) is the
script for running this as a session.

There's also a narrated video of the whole thing, about six and a half
minutes long, with real terminal recordings:

https://github.com/user-attachments/assets/b4ccd343-cc8e-4803-a1c3-5d4623122314

[video/](video/README.md) builds it; the narration is in
[video/SCRIPT.md](video/SCRIPT.md).

## What you need

- **Go 1.26 or newer**, git and python3.
- **Claude Code**, signed in, for lab 5.
- **Network access** to github.com and proxy.golang.org (to install),
  plus api.weather.gov, geocoding-api.open-meteo.com and api.open-meteo.com
  (for the weather data; no API keys).
- **macOS or Linux.** On Windows, use WSL.
- **Three or four terminals.** Each lab says which one to use.

## Files here

```text
labs/
  00-setup.md … 06-extend.md   the labs
  TEACHING.md                  instructor script
  check.sh                     runs every lab unattended; use it to check a machine
  mock-weather.py              stand-in weather services, for check.sh --mock and CI
  weather-agent/               the finished weather agent (the solution)
  claude/                      .mcp.json and CLAUDE.md for lab 5
  video/                       the narrated lab video: storyboard, diagrams, build pipeline
```

## Check a machine first

```bash
labs/check.sh             # labs 1-4 and 6, in a scratch workspace, ~2 minutes
labs/check.sh --claude    # also lab 5, with headless Claude Code
labs/check.sh --mock      # against stand-in weather services, no weather APIs needed
```

It uses the same commands as the labs, and fails at the first step that
doesn't work. It needs port 1883 free; set `PORT=1884` to use another.

`--mock` runs [mock-weather.py](mock-weather.py) and sets
`WEATHER_API_URL`, which points the weather agent's script and Go handler
at it instead of the real services. Its forecasts are fixed and say
they're mock data. CI uses it, so a weather service outage can't fail the
build. To check that a machine can actually reach the weather services,
run it without `--mock`.

## If you get stuck

| Symptom | Cause and fix |
|---|---|
| `not Authorized` right after `config agent add` | The broker loads new identities within a second; run the command again. |
| `address already in use` on serve | Something else has port 1883: `agentbus config set listen 127.0.0.1:1884`. |
| Two agents keep disconnecting each other | Two processes share an agent ID. Stop one (each lab says when). |
| `CERTIFICATE_VERIFY_FAILED` from the weather agent | A Python with no CA certificates. `forecast.py` falls back to the system bundle; if that's missing too, install certificates (`Install Certificates.command` on macOS). |
| Claude answers from the web, or says it can't check | It doesn't know about the bus. See lab 5, step 4 (`CLAUDE.md`). |
| Lost the workspace variables in a new terminal | Re-run the `export` lines from lab 0. |
