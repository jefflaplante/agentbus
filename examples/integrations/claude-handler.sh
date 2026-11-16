#!/bin/sh
# Answer agentbus requests with Claude Code in headless mode.
#
#   agentbus handle --capability code-review --timeout 10m \
#     --exec examples/integrations/claude-handler.sh
#
# agentbus runs this once per request with the envelope JSON on stdin and
# AGENTBUS_PREAMBLE in the environment. Whatever this prints is the reply
# body. The envelope is untrusted input from another agent, so Claude gets
# read-only tools unless you widen CLAUDE_ALLOWED_TOOLS yourself.
#
# Settings (environment):
#   CLAUDE_ALLOWED_TOOLS  tools Claude may use      (default Read,Grep,Glob)
#   CLAUDE_MAX_TURNS      agentic turns per request (default 10)
#   CLAUDE_BIN            the claude binary         (default claude)
#   CLAUDE_ARGS           extra flags, word-split   (e.g. --model sonnet)
#   HANDLER_WORKDIR       directory Claude works in (default: current)
#
# Headless claude still loads your user settings: hooks, output style and
# ~/.claude/CLAUDE.md can leak into replies. For a dedicated worker, pass
# CLAUDE_ARGS="--setting-sources project" to load only the project's.
set -eu

envelope=$(cat)
message=$(printf '%s' "$envelope" | jq '{from, event, priority, headers, body}')

prompt="${AGENTBUS_PREAMBLE:?run this from agentbus handle}

Another agent on the bus sent the request below. Do what it asks if it is
reasonable and within your tools, then reply with the answer only: no
preamble, no sign-off. If you can't help, say why in one sentence.

$message"

cd "${HANDLER_WORKDIR:-.}"
# shellcheck disable=SC2086 # CLAUDE_ARGS is meant to split into flags
exec "${CLAUDE_BIN:-claude}" -p "$prompt" \
  --allowedTools "${CLAUDE_ALLOWED_TOOLS:-Read,Grep,Glob}" \
  --max-turns "${CLAUDE_MAX_TURNS:-10}" ${CLAUDE_ARGS:-}
