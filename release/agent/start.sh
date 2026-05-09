#!/bin/sh
# polar-agent start helper. Forks the agent in the background, writes
# a pidfile + log. Pairs with stop.sh.
#
# Configure by env or by editing the block below. Minimum required:
# BOT_ID. Server + token must already exist via `polar-agent login`
# (or by placing ~/.polar/agent.toml from agent.toml.example).
#
# Run: ./start.sh
# View log: tail -f $LOGFILE

set -eu

# === edit me =====================================================
# Path to the polar-agent binary. Pick the right one from agents/
# (matches your OS/arch) and either symlink it onto PATH or set this.
POLAR_AGENT_BIN="${POLAR_AGENT_BIN:-./polar-agent}"

# Bot user id this agent attaches to. Find it under /bots.html or
# from the auto-register response. REQUIRED.
BOT_ID="${BOT_ID:-}"

# Working directory the agent operates in (read_file / write_file
# resolve under here, run_cmd cwd is here).
WORKDIR="${WORKDIR:-$HOME/polar-work}"

# Passthrough tool: kimi / claude / codex (or empty for tool-loop
# mode where dock drives the LLM).
TOOL="${TOOL:-kimi}"

# Where to drop the pidfile + log. Override on systems with read-only
# /tmp or where multiple agents run on the same host.
PIDFILE="${PIDFILE:-/tmp/polar-agent.pid}"
LOGFILE="${LOGFILE:-/tmp/polar-agent.log}"
# === end edit =====================================================

if [ -z "$BOT_ID" ]; then
  echo "BOT_ID is required. Set it via env or edit start.sh." >&2
  exit 1
fi
if [ ! -x "$POLAR_AGENT_BIN" ]; then
  echo "polar-agent binary not found at $POLAR_AGENT_BIN" >&2
  echo "Adjust POLAR_AGENT_BIN or copy the right agents/polar-agent_<os>_<arch> binary into place." >&2
  exit 1
fi

if [ -f "$PIDFILE" ]; then
  EXISTING="$(cat "$PIDFILE")"
  if kill -0 "$EXISTING" 2>/dev/null; then
    echo "polar-agent already running (pid $EXISTING)"
    exit 0
  fi
  rm -f "$PIDFILE"
fi

mkdir -p "$WORKDIR"
ARGS="attach --bot=$BOT_ID --workdir=$WORKDIR"
if [ -n "$TOOL" ]; then
  ARGS="$ARGS --tool=$TOOL"
fi

# `nohup ... &` detaches us from the controlling terminal; useful when
# the operator runs ./start.sh from an SSH session that may close.
nohup "$POLAR_AGENT_BIN" $ARGS > "$LOGFILE" 2>&1 &
PID=$!
echo "$PID" > "$PIDFILE"

# Brief settle so we can report the actual outcome instead of a
# "started" message that's about to crash.
sleep 1
if kill -0 "$PID" 2>/dev/null; then
  echo "polar-agent started (pid $PID, bot=$BOT_ID, workdir=$WORKDIR, tool=${TOOL:-tool-loop})"
  echo "log: $LOGFILE"
else
  echo "polar-agent failed to start. tail of log:" >&2
  tail -n 20 "$LOGFILE" >&2 || true
  rm -f "$PIDFILE"
  exit 1
fi
