#!/bin/sh
# polar-agent docker entrypoint. Reads three required env vars, an
# optional per-bot tool map, runs `polar-agent login` once, then
# fans out to N concurrent `polar-agent attach` processes (one per
# bot id). Background processes; `wait` blocks the entrypoint so
# the container stays alive while any agent is running.
#
# Required:
#   POLAR_DOCK_URL          https://dock.example.com
#   POLAR_AGENT_TOKEN       polar_agent_xxxxxxxxxxxxxxxxxx
#   POLAR_BOT_IDS           bot_kimi,bot_claude  (comma-sep)
#
# Optional:
#   POLAR_BOT_TOOLS         kimi,claude          (same order/length
#                                                 as POLAR_BOT_IDS;
#                                                 empty entries ok =
#                                                 tool-loop mode for
#                                                 that bot)
#   POLAR_WORK_ROOT         default /work
#   POLAR_KIMI_TIMEOUT      default 30m  (per-coder wall clock)

set -eu

require() {
  if [ -z "$(eval "echo \${$1:-}")" ]; then
    echo "❌ env $1 required" >&2
    echo "   docker run -e POLAR_DOCK_URL=... -e POLAR_AGENT_TOKEN=... -e POLAR_BOT_IDS=... ..." >&2
    exit 1
  fi
}

require POLAR_DOCK_URL
require POLAR_AGENT_TOKEN
require POLAR_BOT_IDS

WORK_ROOT="${POLAR_WORK_ROOT:-/work}"
mkdir -p "$WORK_ROOT"

echo "→ polar-agent login → $POLAR_DOCK_URL"
polar-agent login --server="$POLAR_DOCK_URL" --token="$POLAR_AGENT_TOKEN"

# Convert comma-separated env vars to whitespace-separated for `for` loops.
BOT_IDS=$(printf '%s' "$POLAR_BOT_IDS" | tr ',' ' ')
BOT_TOOLS=$(printf '%s' "${POLAR_BOT_TOOLS:-}" | tr ',' ' ')

# Iterate bot ids by index so we can match a tool from BOT_TOOLS at
# the same position. POSIX sh has no real arrays — fake it with set.
set -- $BOT_IDS
i=0
for bot in "$@"; do
  i=$((i + 1))
  workdir="$WORK_ROOT/$bot"
  mkdir -p "$workdir"

  # Pull the i-th word from BOT_TOOLS, if any.
  tool=""
  if [ -n "$BOT_TOOLS" ]; then
    tool=$(printf '%s' "$BOT_TOOLS" | awk -v idx="$i" '{ print $idx }')
  fi
  tool_arg=""
  if [ -n "$tool" ] && [ "$tool" != "_" ]; then
    tool_arg="--tool=$tool"
  fi

  echo "→ attach bot=$bot workdir=$workdir tool=${tool:-<tool-loop>}"
  polar-agent attach \
    --bot="$bot" \
    --workdir="$workdir" \
    $tool_arg \
    --verbose &
done

# Block forever (or until a child crashes — we let docker restart
# the container in that case via --restart=unless-stopped).
wait
