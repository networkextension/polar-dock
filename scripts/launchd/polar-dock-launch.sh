#!/usr/bin/env bash
# Wrapper that launchd's polar.dock plist invokes. Sources per-box
# secrets from ~/polar-dock.env (see polar-dock.env.sample), then
# exec's the binary. Keep this script committable — it has no
# secrets, no per-box paths.

set -euo pipefail

ENV_FILE="${POLAR_DOCK_ENV:-$HOME/polar-dock.env}"
if [[ -f "$ENV_FILE" ]]; then
  # shellcheck disable=SC1090
  source "$ENV_FILE"
fi

BIN="${POLAR_DOCK_BIN:-$HOME/.local/bin/polar-dock}"
cd "${POLAR_DOCK_WORKDIR:-$HOME/github/Polar-}"
exec "$BIN"
