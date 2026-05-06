#!/usr/bin/env bash
# start_agents.sh — boot all polar-agent passthrough processes in
# one tmux session (one window per tool). Reattach later with
# `tmux attach -t polar-agents`. Kill the lot with `tmux kill-
# session -t polar-agents` or via the script's `stop` subcommand.
#
# Usage:
#   ./scripts/start_agents.sh start   # start all configured agents
#   ./scripts/start_agents.sh stop    # kill the tmux session
#   ./scripts/start_agents.sh attach  # tmux attach -t polar-agents
#   ./scripts/start_agents.sh status  # show window list + pid hints
#
# Configure via env vars or by editing the defaults below. At
# minimum each tool needs a bot_user_id; workdirs default to a
# tool-specific subdir under POLAR_WORK_ROOT and are mkdir -p'd
# on demand. Skipping a bot id (leave it empty) skips that tool.
#
#   POLAR_BOT_KIMI       bot_user_id paired with kimi-cli
#   POLAR_BOT_CLAUDE     bot_user_id paired with claude
#   POLAR_BOT_CODEX      bot_user_id paired with codex
#   POLAR_WORK_ROOT      parent dir for per-tool workdirs (default:
#                        $HOME/github)
#   POLAR_WORKDIR_KIMI   override kimi's workdir (default:
#                        $POLAR_WORK_ROOT/work_kimi)
#   POLAR_WORKDIR_CLAUDE override claude's workdir
#   POLAR_WORKDIR_CODEX  override codex's workdir
#   POLAR_KIMI_TIMEOUT   wall-clock cap per spawn (default 1h);
#                        applies to all tools, not just kimi
#                        (env var name kept for compat with v1)
#   POLAR_AGENT_BIN      path to polar-agent (default: which
#                        polar-agent / fallback ~/.local/bin/polar-agent)
#
# Persist your bot ids in ~/.polar/start_agents.env so this script
# is idempotent across logins; the file is sourced if present.

set -euo pipefail

SESSION="polar-agents"
ENV_FILE="${POLAR_AGENTS_ENV:-$HOME/.polar/start_agents.env}"

# Source persistent config so users don't need to re-export every
# shell. Quote-safe — the file is plain `export KEY=VALUE` lines.
if [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1090
  . "$ENV_FILE"
fi

WORK_ROOT="${POLAR_WORK_ROOT:-$HOME/github}"
TIMEOUT="${POLAR_KIMI_TIMEOUT:-1h}"

WORKDIR_KIMI="${POLAR_WORKDIR_KIMI:-$WORK_ROOT/work_kimi}"
WORKDIR_CLAUDE="${POLAR_WORKDIR_CLAUDE:-$WORK_ROOT/work_claude}"
WORKDIR_CODEX="${POLAR_WORKDIR_CODEX:-$WORK_ROOT/work_codex}"

resolve_agent_bin() {
  if [ -n "${POLAR_AGENT_BIN:-}" ]; then
    echo "$POLAR_AGENT_BIN"
    return
  fi
  if command -v polar-agent >/dev/null 2>&1; then
    command -v polar-agent
    return
  fi
  if [ -x "$HOME/.local/bin/polar-agent" ]; then
    echo "$HOME/.local/bin/polar-agent"
    return
  fi
  echo "polar-agent not found in PATH or ~/.local/bin/polar-agent" >&2
  echo "set POLAR_AGENT_BIN=/path/to/polar-agent or 'go build -o ~/.local/bin/polar-agent ./cmd/polar-agent' first" >&2
  exit 1
}

require_tmux() {
  if ! command -v tmux >/dev/null 2>&1; then
    echo "tmux required (install via 'brew install tmux' on macOS)" >&2
    exit 1
  fi
}

# spawn_window <window-name> <bot_id> <tool> <workdir>
# Creates a new tmux window in the session, mkdir -p's the workdir,
# and runs polar-agent attach with --tool=<tool>.
spawn_window() {
  local name="$1" bot="$2" tool="$3" wd="$4"
  if [ -z "$bot" ]; then
    echo "[skip] $tool: no bot id (set POLAR_BOT_${tool^^})"
    return
  fi
  mkdir -p "$wd"
  local cmd="POLAR_KIMI_TIMEOUT=$TIMEOUT $AGENT_BIN attach --bot=$bot --workdir=$wd --tool=$tool --verbose"
  if tmux list-windows -t "$SESSION" 2>/dev/null | grep -q ": ${name}\b"; then
    echo "[exists] $name (window already in session, leaving it)"
    return
  fi
  if ! tmux has-session -t "$SESSION" 2>/dev/null; then
    tmux new-session -d -s "$SESSION" -n "$name" "$cmd"
  else
    tmux new-window -t "$SESSION" -n "$name" "$cmd"
  fi
  echo "[start] $name → $cmd"
}

cmd_start() {
  require_tmux
  AGENT_BIN="$(resolve_agent_bin)"

  spawn_window "kimi"   "${POLAR_BOT_KIMI:-}"   "kimi"   "$WORKDIR_KIMI"
  spawn_window "claude" "${POLAR_BOT_CLAUDE:-}" "claude" "$WORKDIR_CLAUDE"
  spawn_window "codex"  "${POLAR_BOT_CODEX:-}"  "codex"  "$WORKDIR_CODEX"

  if ! tmux has-session -t "$SESSION" 2>/dev/null; then
    echo "no agents started — set at least one POLAR_BOT_* env var" >&2
    exit 1
  fi
  echo
  echo "session '$SESSION' running. Useful commands:"
  echo "  tmux attach -t $SESSION              # attach (Ctrl-b d to detach)"
  echo "  tmux list-windows -t $SESSION         # see window list"
  echo "  $0 stop                                # kill session"
}

cmd_stop() {
  require_tmux
  if tmux has-session -t "$SESSION" 2>/dev/null; then
    tmux kill-session -t "$SESSION"
    echo "killed session '$SESSION'"
  else
    echo "no '$SESSION' tmux session to stop"
  fi
}

cmd_attach() {
  require_tmux
  if ! tmux has-session -t "$SESSION" 2>/dev/null; then
    echo "no '$SESSION' tmux session — run '$0 start' first" >&2
    exit 1
  fi
  tmux attach -t "$SESSION"
}

cmd_status() {
  require_tmux
  if ! tmux has-session -t "$SESSION" 2>/dev/null; then
    echo "session '$SESSION' not running"
    return
  fi
  echo "session '$SESSION' windows:"
  tmux list-windows -t "$SESSION" -F '  #{window_index}: #{window_name}  pane=#{pane_pid}'
  echo
  echo "polar-agent processes:"
  pgrep -fl polar-agent | awk '{print "  " $0}' || echo "  (none — windows may have exited)"
}

case "${1:-start}" in
  start)  cmd_start ;;
  stop)   cmd_stop ;;
  attach) cmd_attach ;;
  status) cmd_status ;;
  -h|--help|help)
    sed -n '2,/^$/p' "$0"
    exit 0
    ;;
  *)
    echo "unknown subcommand: $1" >&2
    echo "usage: $0 {start|stop|attach|status}" >&2
    exit 2
    ;;
esac
