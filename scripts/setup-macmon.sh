#!/usr/bin/env bash
# setup-macmon.sh — install macmon (Apple Silicon system telemetry) on a
# Polar host. Idempotent: re-running is safe.
#
# Usage from the dev mac:
#
#   ./scripts/setup-macmon.sh build               # cargo build macmon from source
#   ./scripts/setup-macmon.sh push  <ssh-target>  # rsync binary + install launchd
#   ./scripts/setup-macmon.sh scrape <ssh-target> <hostname-label> [env-label]
#                                                 # append scrape job to dock's
#                                                 # prometheus.yml + hot-reload
#   ./scripts/setup-macmon.sh dashboard <ssh-target>
#                                                 # provision macmon dashboard
#                                                 # + dashboard provider yaml
#
# <ssh-target> follows normal ssh syntax, e.g. `local@169.254.246.147`
# (direct) or `-p 5722 local@127.0.0.1` (tunnel).
#
# Prereqs:
# - macmon repo cloned somewhere (default ~/Codex/macmon; override via
#   MACMON_REPO env var)
# - rustc + cargo on the dev mac (only for `build`)
# - ssh + rsync working to the target box
# - Target box is Apple Silicon (darwin/arm64); macmon doesn't compile
#   for x86_64 macOS or any non-macOS host (FreeBSD / Linux skip this).
#
# This script does NOT touch grafana.ini — those edits are documented
# in doc/macmon-deploy.md and are sensitive enough to want manual
# confirmation (anon Viewer + allow_embedding). Run them once per
# Grafana instance.

set -euo pipefail

MACMON_REPO="${MACMON_REPO:-$HOME/Codex/macmon}"
MACMON_PORT="${MACMON_PORT:-9092}"

usage() {
  sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
}

cmd_build() {
  if [ ! -d "$MACMON_REPO" ]; then
    echo "macmon repo not found at $MACMON_REPO" >&2
    echo "clone first: git clone https://github.com/vladkens/macmon $MACMON_REPO" >&2
    exit 1
  fi
  if ! command -v cargo >/dev/null 2>&1; then
    echo "cargo not found; install via `https://rustup.rs/` first" >&2
    exit 1
  fi
  (cd "$MACMON_REPO" && cargo build --release)
  ls -lh "$MACMON_REPO/target/release/macmon"
}

# Push binary + install launchd plist. Sources the SSH target's existing
# polar deploy conventions (~/.local/bin for binaries).
cmd_push() {
  local target="$*"
  [ -n "$target" ] || usage
  local bin="$MACMON_REPO/target/release/macmon"
  if [ ! -x "$bin" ]; then
    echo "binary not found at $bin — run '$0 build' first" >&2
    exit 1
  fi
  echo "→ rsync $bin → $target:~/.local/bin/macmon"
  rsync -avz -e "ssh" "$bin" "$target:/Users/local/.local/bin/macmon"
  echo "→ install launchd plist (port $MACMON_PORT)"
  ssh "$target" "chmod +x /Users/local/.local/bin/macmon && \
    /Users/local/.local/bin/macmon serve --port $MACMON_PORT --install"
  echo "→ smoke /metrics"
  sleep 3
  ssh "$target" "curl -sS http://127.0.0.1:$MACMON_PORT/metrics | head -3"
  echo "✓ macmon up on $target:$MACMON_PORT"
}

# Append a scrape job to the dock host's prometheus.yml.
# Uses idempotent grep-check to avoid duplicating the job.
cmd_scrape() {
  local target="$1"
  local hostname_label="${2:-}"
  local env_label="${3:-prod}"
  [ -n "$target" ] && [ -n "$hostname_label" ] || usage

  if ssh "$target" "grep -q 'job_name: macmon' /opt/homebrew/etc/prometheus.yml"; then
    echo "scrape job already present, skipping append (edit by hand if you need to change labels)"
  else
    echo "→ append macmon scrape to prometheus.yml"
    # Quoting note: the heredoc body is single-quoted to keep the shell
    # vars on the local side; only $hostname_label / $env_label are
    # interpolated.
    ssh "$target" "cp /opt/homebrew/etc/prometheus.yml /tmp/prometheus.yml.before-macmon-$(date +%s) && cat >> /opt/homebrew/etc/prometheus.yml" <<EOF

  - job_name: macmon
    static_configs:
      - targets:
          - 127.0.0.1:$MACMON_PORT
        labels:
          host: $hostname_label
          env: $env_label
EOF
  fi

  echo "→ hot-reload prometheus"
  local code
  code=$(ssh "$target" "curl -sS -X POST http://127.0.0.1:9090/-/reload -o /dev/null -w '%{http_code}'")
  if [ "$code" = "200" ]; then
    echo "✓ prometheus reloaded"
  else
    echo "✗ reload returned $code — check /opt/homebrew/var/log/prometheus.log" >&2
    exit 1
  fi
}

cmd_dashboard() {
  local target="$*"
  [ -n "$target" ] || usage
  local dash="$MACMON_REPO/example-grafana/grafana-dashboard.json"
  if [ ! -f "$dash" ]; then
    echo "dashboard json not found at $dash" >&2
    exit 1
  fi
  echo "→ rsync dashboard json"
  rsync -avz -e "ssh" "$dash" "$target:/tmp/macmon-overview.json"
  echo "→ install"
  ssh "$target" "
    mkdir -p /opt/homebrew/opt/grafana/share/grafana/dashboards
    mv /tmp/macmon-overview.json /opt/homebrew/opt/grafana/share/grafana/dashboards/macmon-overview.json
    mkdir -p /opt/homebrew/opt/grafana/share/grafana/conf/provisioning/dashboards
    cat > /opt/homebrew/opt/grafana/share/grafana/conf/provisioning/dashboards/macmon.yaml <<'PROV'
apiVersion: 1

providers:
  - name: macmon
    orgId: 1
    folder: Host
    type: file
    disableDeletion: false
    updateIntervalSeconds: 30
    allowUiUpdates: true
    options:
      path: /opt/homebrew/opt/grafana/share/grafana/dashboards
PROV
  "
  echo "✓ dashboard provisioned"
  echo
  echo "Note: the Prometheus datasource (uid=prometheus) is assumed to"
  echo "already exist in another provisioning yaml (commonly polar.yaml)."
  echo "If not, see doc/macmon-deploy.md §5 — but never add a second"
  echo "yaml with isDefault: true."
  echo
  echo "Restart grafana when ready:"
  echo "  ssh $target '/opt/homebrew/bin/brew services restart grafana'"
}

case "${1:-}" in
  build)     shift; cmd_build "$@" ;;
  push)      shift; cmd_push "$@" ;;
  scrape)    shift; cmd_scrape "$@" ;;
  dashboard) shift; cmd_dashboard "$@" ;;
  *)         usage ;;
esac
