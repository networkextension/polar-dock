#!/usr/bin/env bash
# One-shot installer for the polar-dock launchd auto-start. Run on
# the deploy box; idempotent.
#
#   bash scripts/launchd/setup.sh
#
# Steps:
#   1. Copy the wrapper to ~/.local/bin/polar-dock-launch.sh
#   2. Copy the plist to ~/Library/LaunchAgents/polar.dock.plist
#   3. Bootstrap ~/polar-dock.env from the sample IF it doesn't
#      exist yet (operator fills in real values + chmod 600)
#   4. launchctl load -w to start + auto-start at login
#
# If ~/polar-dock.env exists, we don't touch it — the operator
# already configured their secrets.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
LOCAL_BIN="$HOME/.local/bin"
LAUNCH_AGENTS="$HOME/Library/LaunchAgents"
ENV_FILE="$HOME/polar-dock.env"

mkdir -p "$LOCAL_BIN" "$LAUNCH_AGENTS"

echo "→ installing wrapper at $LOCAL_BIN/polar-dock-launch.sh"
install -m 0755 "$SCRIPT_DIR/polar-dock-launch.sh" "$LOCAL_BIN/polar-dock-launch.sh"

echo "→ installing plist at $LAUNCH_AGENTS/polar.dock.plist"
install -m 0644 "$SCRIPT_DIR/polar.dock.plist" "$LAUNCH_AGENTS/polar.dock.plist"
plutil -lint "$LAUNCH_AGENTS/polar.dock.plist"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "→ $ENV_FILE not found — bootstrapping from sample (you must fill in real values + chmod 600)"
  install -m 0600 "$SCRIPT_DIR/polar-dock.env.sample" "$ENV_FILE"
  echo ""
  echo "  ! Generate fresh secrets and edit $ENV_FILE before reloading the agent:"
  echo "    METRICS_TOKEN=$(openssl rand -hex 32)"
  echo "    POLAR_CREDENTIAL_KEY=$(openssl rand -hex 32)"
  echo "    IOSDIST_RESOURCE_KEY=$(openssl rand -hex 32)   # optional"
  echo ""
else
  echo "→ $ENV_FILE already present, leaving untouched"
fi

echo "→ launchctl unload + reload (idempotent)"
launchctl unload "$LAUNCH_AGENTS/polar.dock.plist" 2>/dev/null || true
launchctl load -w "$LAUNCH_AGENTS/polar.dock.plist"

sleep 3
if pgrep -fl polar-dock >/dev/null; then
  echo "✓ polar-dock is running under launchd."
  echo "  log: tail -f /tmp/polar.dock.log"
else
  echo "✗ polar-dock did not start. Check /tmp/polar.dock.log + ~/polar-dock.env."
  exit 1
fi
