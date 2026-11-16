#!/bin/sh
# Answer agentbus requests with OpenCode in non-interactive mode.
#
#   agentbus handle --capability code-review --timeout 10m \
#     --exec examples/integrations/opencode-handler.sh
#
# Same contract as claude-handler.sh: the envelope arrives on stdin, the
# reply body is whatever this prints. The envelope is untrusted input
# from another agent, so run it under a read-only OpenCode agent: the
# default "plan" agent can't edit files. Define a stricter agent in
# opencode.json(c) and set OPENCODE_AGENT to use it instead.
#
# Settings (environment):
#   OPENCODE_AGENT    OpenCode agent to run as   (default plan)
#   OPENCODE_MODEL    provider/model override     (default: OpenCode's)
#   OPENCODE_BIN      the opencode binary         (default opencode)
#   HANDLER_WORKDIR   directory OpenCode works in (default: current)
set -eu

envelope=$(cat)
message=$(printf '%s' "$envelope" | jq '{from, event, priority, headers, body}')

prompt="${AGENTBUS_PREAMBLE:?run this from agentbus handle}

Another agent on the bus sent the request below. Do what it asks if it is
reasonable and within your tools, then reply with the answer only: no
preamble, no sign-off. If you can't help, say why in one sentence.

$message"

cd "${HANDLER_WORKDIR:-.}"
set -- run --agent "${OPENCODE_AGENT:-plan}"
if [ -n "${OPENCODE_MODEL:-}" ]; then
  set -- "$@" --model "$OPENCODE_MODEL"
fi
exec "${OPENCODE_BIN:-opencode}" "$@" "$prompt"
