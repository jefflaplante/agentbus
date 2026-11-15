#!/usr/bin/env bash
# agentbus demo: builds everything, starts a broker and a worker agent, runs
# the coordinator's scripted conversation, then leaves the bus running so
# you can poke at it. Ctrl-C stops everything.
#
#   scripts/demo.sh            run, then stay up for exploring
#   scripts/demo.sh --once     run the conversation and exit (status = result)
#
# Env: AGENTBUS_DEMO_PORT (default 18830), WORKER_DELAY (default 300ms).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
RUN="$ROOT/demo/.run"
BIN="$ROOT/bin"
PORT="${AGENTBUS_DEMO_PORT:-18830}"
DELAY="${WORKER_DELAY:-300ms}"
ONCE=false
[[ "${1:-}" == "--once" ]] && ONCE=true
export GOWORK=off

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }

PIDS=()
cleanup() {
  trap - EXIT
  for pid in "${PIDS[@]:-}"; do [[ -n "$pid" ]] && kill "$pid" 2>/dev/null; done
  for pid in "${PIDS[@]:-}"; do [[ -n "$pid" ]] && wait "$pid" 2>/dev/null; done
  return 0
}
trap cleanup EXIT
trap 'echo; echo "stopping"; exit 130' INT TERM

wait_for_log() { # file pattern what
  for _ in $(seq 1 100); do
    grep -q "$2" "$1" 2>/dev/null && return 0
    sleep 0.1
  done
  echo "timed out waiting for $3; log:" >&2
  cat "$1" >&2
  exit 1
}

step "Building bin/agentbus, bin/worker, bin/coordinator"
mkdir -p "$BIN" "$RUN"
(cd "$ROOT" && go build -o "$BIN/agentbus" ./cmd/agentbus \
            && go build -o "$BIN/worker" ./demo/cmd/worker \
            && go build -o "$BIN/coordinator" ./demo/cmd/coordinator)

step "Minting a token per agent and writing demo/.run/broker.json"
mint() { # id -> writes demo/.run/<id>.env, prints the token hash
  local out
  out=$("$BIN/agentbus" token)
  cat >"$RUN/$1.env" <<EOF
export AGENTBUS_URL=tcp://127.0.0.1:$PORT
export AGENTBUS_AGENT=$1
export AGENTBUS_TOKEN=$(awk '/^token:/ {print $2}' <<<"$out")
EOF
  chmod 600 "$RUN/$1.env"
  awk '/^token_sha256:/ {print $2}' <<<"$out"
}
cat >"$RUN/broker.json" <<EOF
{
  "listen": "127.0.0.1:$PORT",
  "agents": [
    {"id": "coordinator", "token_sha256": "$(mint coordinator)"},
    {"id": "worker",      "token_sha256": "$(mint worker)",   "capabilities": ["textstats"]},
    {"id": "worker-2",    "token_sha256": "$(mint worker-2)", "capabilities": ["textstats"]},
    {"id": "you",         "token_sha256": "$(mint you)",      "send_to": ["worker", "worker-2"]},
    {"id": "explorer",    "token_sha256": "$(mint explorer)", "observer": true}
  ]
}
EOF
echo "agents: coordinator, worker, worker-2 (both serve textstats), you (may only message the workers),"
echo "        explorer (read-only observer for MQTT Explorer and other desktop clients)"

step "Starting the broker on 127.0.0.1:$PORT (log: demo/.run/broker.log)"
"$BIN/agentbus" serve -config "$RUN/broker.json" >"$RUN/broker.log" 2>&1 &
PIDS+=($!)
wait_for_log "$RUN/broker.log" 'agentbus broker listening' "the broker"

step "Starting the worker agent, $DELAY per task (log: demo/.run/worker.log)"
(source "$RUN/worker.env" && exec "$BIN/worker" -delay "$DELAY") >"$RUN/worker.log" 2>&1 &
PIDS+=($!)
wait_for_log "$RUN/worker.log" 'worker ready' "the worker"

step "Running the coordinator's conversation with the worker"
set +e
(source "$RUN/coordinator.env" && "$BIN/coordinator") >"$RUN/summary.json"
status=$?
set -e
echo "(JSON summary: demo/.run/summary.json)"

if $ONCE; then
  exit $status
fi

step "The bus is still up. In another terminal, from $(basename "$ROOT")/:"
cat <<'EOF'

  source demo/.run/you.env                  # act as agent "you"

  bin/agentbus agents                       # who's online, with what capabilities
  bin/agentbus request -to worker -body '{"op":"ping"}'
  bin/agentbus request -to worker -body '{"op":"reverse","text":"hello bus"}'
  bin/agentbus task -capability textstats -body '{"text":"to be or not to be"}'
  bin/agentbus request -to coordinator -body '{}'     # refused: "you" may only message workers
  bin/agentbus watch                        # live events (jobs/done) from every agent

  # Load-balancing: start a second worker, then rerun the coordinator and
  # look at "handled by" in the tasks step.
  (source demo/.run/worker-2.env && bin/worker -delay 300ms)
  (source demo/.run/coordinator.env && bin/coordinator > /dev/null)

  tail -f demo/.run/broker.log              # the broker's audit log, one line per envelope

  # Desktop MQTT client (MQTT Explorer, MQTTX): see docs/DESKTOP-CLIENTS.md
  cat demo/.run/explorer.env                # host 127.0.0.1, port below, client ID = username = explorer

EOF
bold "Ctrl-C to stop the broker and worker."
while true; do sleep 3600 & PIDS+=($!); wait $!; done  # wait returns at once on a signal
