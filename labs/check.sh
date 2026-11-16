#!/usr/bin/env bash
# Walk through every lab non-interactively, with the same commands the
# lab pages use, in a scratch workspace. Run it before teaching, to check
# a machine and the network (the weather agent calls api.weather.gov and
# open-meteo.com).
#
#   labs/check.sh             labs 1-4 and 6, against the real weather services
#   labs/check.sh --claude    also lab 5: ask Claude Code (claude -p) for the forecast
#   labs/check.sh --mock      against labs/mock-weather.py instead: no internet
#                             needed past the Go toolchain. CI runs this.
#
# Environment: LAB (workspace; default a new temp dir), PORT (default 1883),
# CLAUDE_PROMPT (what lab 5 asks Claude).
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
LAB=${LAB:-$(mktemp -d "${TMPDIR:-/tmp}/agentbus-lab.XXXXXX")}
PORT=${PORT:-1883}
WITH_CLAUDE= MOCK=
for arg in "$@"; do
  case $arg in
    --claude) WITH_CLAUDE=--claude ;;
    --mock) MOCK=1 ;;
    *) echo "usage: labs/check.sh [--claude] [--mock]" >&2; exit 2 ;;
  esac
done
CLAUDE_PROMPT=${CLAUDE_PROMPT:-"What's the weather forecast for Seattle, WA?"}
export AGENTBUS_CONFIG=$LAB/broker.json
export PATH=$REPO/bin:$PATH
unset AGENTBUS_URL AGENTBUS_AGENT AGENTBUS_TOKEN AGENTBUS_TOKEN_FILE

pids=()
cleanup() { for p in ${pids[@]+"${pids[@]}"}; do kill "$p" 2>/dev/null || true; done; wait 2>/dev/null || true; }
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok() { printf '\033[32mok\033[0m %s\n' "$*"; }
fail() { printf '\033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
expect() { # expect "description" "needle" "haystack"
  case $3 in *"$2"*) ok "$1" ;; *) printf '%s\n' "$3"; fail "$1: expected to see: $2" ;; esac
}
wait_for() { # wait_for "description" command...
  local what=$1; shift
  for _ in $(seq 1 100); do "$@" >/dev/null 2>&1 && return 0; sleep 0.2; done
  fail "timed out waiting for $what"
}
as() { AGENTBUS_AGENT=$1 "${@:2}"; }
# add_agent ID [flags]: create an identity and wait for the running broker
# to load it (it rereads its config within a second).
add_agent() {
  agentbus config agent add "$@" >/dev/null 2>&1
  wait_for "the broker to load $1" sh -c "agentbus status --plain | grep -q '\"id\":\"$1\"'"
}

step "Lab 0: build agentbus ($REPO)"
(cd "$REPO" && GOWORK=off go build -o bin/agentbus ./cmd/agentbus)
agentbus --help >/dev/null && ok "agentbus on PATH"
echo "workspace: $LAB"
if [ -n "$MOCK" ]; then
  # Every agent started below inherits WEATHER_API_URL, which points the
  # weather handlers at the stand-in instead of the real services.
  python3 "$REPO/labs/mock-weather.py" --port-file "$LAB/mock-weather.port" 2>"$LAB/mock-weather.log" &
  pids+=($!)
  wait_for "the mock weather services" test -s "$LAB/mock-weather.port"
  export WEATHER_API_URL="http://127.0.0.1:$(cat "$LAB/mock-weather.port")"
  ok "mock weather services at $WEATHER_API_URL"
fi

step "Lab 1: start the broker"
agentbus config init >/dev/null
agentbus config set listen "127.0.0.1:$PORT" >/dev/null 2>&1
agentbus serve >"$LAB/broker.log" 2>&1 &
pids+=($!)
wait_for "the broker" agentbus status --plain
expect "admin is online" '"id":"admin"' "$(agentbus status --plain)"

step "Lab 2: a first agent from the template"
agentbus agent init hello --dir "$LAB/hello" --agentbus-src "$REPO" >/dev/null
add_agent hello --capability hello.wordcount --capability hello.disk
add_agent me
(cd "$LAB/hello" && exec agentbus agent run) >"$LAB/hello.log" 2>&1 &
HELLO_PID=$!
pids+=($HELLO_PID)
wait_for "hello to come online" sh -c 'agentbus agents --plain | grep -q "hello .*online"'
expect "uptime (shell command)" "load" "$(as me agentbus request --plain --to hello --body '{"op":"uptime"}')"
expect "wordcount (Python script)" '"words":3' "$(as me agentbus task --plain -c hello.wordcount --body '{"text":"one two three"}')"
expect "bad body refused with the script's own code" "invalid" "$(as me agentbus task --plain -c hello.wordcount --body '"oops"' 2>&1 || true)"
kill "$HELLO_PID"; wait "$HELLO_PID" 2>/dev/null || true

step "Lab 3: the weather agent"
agentbus agent init weather --dir "$LAB/weather" --agentbus-src "$REPO" \
  --description "Weather forecasts for any city" >/dev/null
rm "$LAB/weather/scripts/disk.sh" "$LAB/weather/scripts/wordcount.py"
cp "$REPO/labs/weather-agent/scripts/forecast.py" "$LAB/weather/scripts/"
cat >"$LAB/weather/agent.yaml" <<'EOF'
agent: weather
description: Weather forecasts for any city, from public weather services
max_concurrent: 4
timeout: 30s
drain: 15s
handlers:
  - task: weather.forecast
    script: scripts/forecast.py
    description: >-
      body {"location": "City, ST" or "City, Country", "days"?: 1-7 (default 3)};
      replies {"location", "source", "periods": [{"name", "temperature", "unit", "summary", "detail", "wind"}]}
  - inbox: true
    op: forecast
    script: scripts/forecast.py
    description: '{"op": "forecast", "location": string, "days"?: 1-7}; same reply as task weather.forecast'
  - inbox: true
    op: ping
    run: 'printf "{\"pong\": \"%s\"}" "$(hostname)"'
    description: '{"op": "ping"}; replies {"pong": hostname}'
EOF
add_agent weather --capability weather.forecast
(cd "$LAB/weather" && agentbus agent check) >/dev/null && ok "agent check"
(cd "$LAB/weather" && exec agentbus agent run) >"$LAB/weather-run.log" 2>&1 &
RUN_PID=$!
pids+=($RUN_PID)
wait_for "weather to come online" sh -c 'agentbus agents --plain | grep -q "weather .*online"'
out=$(as me agentbus task --plain -c weather.forecast --timeout 45s --body '{"location":"Seattle, WA","days":2}')
expect "Seattle forecast" '"location":"Seattle, Washington, US"' "$out"
expect "from the National Weather Service" "National Weather Service" "$out"
if [ -n "$MOCK" ]; then expect "answered by the mock" "mock data" "$out"; fi
expect "forecast as an inbox op" "Seattle, Washington, US" "$(as me agentbus request --plain --to weather --timeout 45s --body '{"op":"forecast","location":"Seattle, WA","days":1}')"
expect "unknown place" "not_found" "$(as me agentbus task --plain -c weather.forecast --body '{"location":"Nowhereville, ZZ"}' 2>&1 || true)"
expect "injection refused" "invalid" "$(as me agentbus task --plain -c weather.forecast --body '{"location":"x; rm -rf ~"}' 2>&1 || true)"
kill "$RUN_PID"; wait "$RUN_PID" 2>/dev/null || true
wait_for "weather to go offline" sh -c 'agentbus agents --plain | grep -q "weather .*offline"'

step "Lab 4: build and deploy the weather agent"
agentbus agent build "$LAB/weather" >/dev/null
GOOS=$(go env GOOS) GOARCH=$(go env GOARCH)
mkdir -p "$LAB/remote-host"
cp "$LAB/weather/dist/weather-$GOOS-$GOARCH" "$LAB/remote-host/weather"
install -m 0600 "$LAB/tokens/weather.token" "$LAB/remote-host/weather.token"
(cd "$LAB/remote-host" && env -u AGENTBUS_CONFIG AGENTBUS_URL="tcp://127.0.0.1:$PORT" \
  AGENTBUS_TOKEN_FILE="$LAB/remote-host/weather.token" ./weather -check) >/dev/null && ok "binary -check"
(cd "$LAB/remote-host" && exec env -u AGENTBUS_CONFIG AGENTBUS_URL="tcp://127.0.0.1:$PORT" \
  AGENTBUS_TOKEN_FILE="$LAB/remote-host/weather.token" ./weather -v) >"$LAB/weather-bin.log" 2>&1 &
WEATHER_PID=$!
pids+=($WEATHER_PID)
wait_for "the deployed weather agent" sh -c 'agentbus agents --plain | grep -q "weather .*online"'
expect "forecast from the binary" "Seattle, Washington, US" \
  "$(as me agentbus task --plain -c weather.forecast --timeout 45s --body '{"location":"Seattle, WA","days":1}')"
expect "ping" "pong" "$(as me agentbus request --plain --to weather --body '{"op":"ping"}')"

if [ "$WITH_CLAUDE" = "--claude" ]; then
  step "Lab 5: Claude Code asks the weather agent"
  add_agent claude-code
  mkdir -p "$LAB/claude"
  cp "$REPO/labs/claude/mcp.json" "$LAB/claude/.mcp.json"
  cp "$REPO/labs/claude/CLAUDE.md" "$LAB/claude/CLAUDE.md"
  # --mcp-config loads the server without the interactive trust prompt;
  # web tools are off so the answer has to come from the bus; and
  # --setting-sources project keeps the instructor's own hooks out of it.
  answer=$(cd "$LAB/claude" && claude -p "$CLAUDE_PROMPT" \
    --mcp-config .mcp.json --strict-mcp-config --setting-sources project \
    --allowedTools "mcp__agentbus__agents,mcp__agentbus__request,mcp__agentbus__task" --disallowedTools "WebSearch,WebFetch" \
    --max-turns 8 </dev/null)
  printf '%s\n' "$answer" | tee "$LAB/claude-answer.txt"
  # The deployed agent runs with -v, so it logs every message it handles.
  grep -q 'msg=handled .*from=claude-code .*ok=true' "$LAB/weather-bin.log" &&
    ok "the weather agent answered Claude: $(grep -o 'msg=handled handler=[^ ]* from=claude-code' "$LAB/weather-bin.log" | tail -1)" ||
    fail "the weather agent handled nothing from claude-code; Claude didn't use the bus"
  expect "Claude's answer is about Seattle" "Seattle" "$answer"
fi

step "Lab 6: a Go handler, an allow-list, and metrics"
cp "$REPO/labs/weather-agent/handlers/current.go" "$LAB/weather/handlers/"
cat >>"$LAB/weather/agent.yaml" <<'EOF'
  - inbox: true
    op: current
    go: current
    timeout: 20s
    allow_from: [me, claude-code]
    description: '{"op": "current", "location": string}; replies current temperature, wind and conditions'
EOF
agentbus agent build "$LAB/weather" >/dev/null
kill "$WEATHER_PID"; wait "$WEATHER_PID" 2>/dev/null || true
wait_for "weather to go offline" sh -c 'agentbus agents --plain | grep -q "weather .*offline"'
cp "$LAB/weather/dist/weather-$GOOS-$GOARCH" "$LAB/remote-host/weather"
(cd "$LAB/remote-host" && exec env -u AGENTBUS_CONFIG AGENTBUS_URL="tcp://127.0.0.1:$PORT" \
  AGENTBUS_TOKEN_FILE="$LAB/remote-host/weather.token" ./weather) >"$LAB/weather-bin2.log" 2>&1 &
pids+=($!)
wait_for "the rebuilt weather agent" sh -c 'agentbus agents --plain | grep -q "weather .*online"'
expect "current conditions (Go handler)" '"temperature_f"' \
  "$(as me agentbus request --plain --to weather --body '{"op":"current","location":"Seattle, WA"}')"
expect "allow_from refuses others" "forbidden" \
  "$(as hello agentbus request --plain --to weather --body '{"op":"current","location":"Seattle, WA"}' 2>&1 || true)"
expect "request outcomes in status" '"target":"task:weather.forecast"' "$(agentbus status --plain)"

printf '\n\033[32mAll labs passed.\033[0m Workspace: %s\n' "$LAB"
