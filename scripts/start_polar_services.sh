#!/usr/bin/env bash
# start_polar_services.sh — boot all the brew-installed services Polar's
# host depends on. Idempotent: if a service is already started it stays
# started (brew services start is a no-op).
#
# Services covered (in start order):
#   redis        session / refresh-token store
#   prometheus   metrics scrape (consumed by Grafana)
#   grafana      observability dashboards
#   nginx        TLS terminator + reverse proxy (depends on the above
#                being up so the proxied paths don't 502 on first hit)
#
# Postgres is NOT in this script — it runs out-of-band (Postgres.app on
# the master, streaming replica on the slave). PostgreSQL.app launches
# itself; if you're on the slave and it's stopped, start it via the
# Postgres.app GUI or `pg_ctl -D <data-dir> start`.
#
# dock + polar-agent are the other thing not here. They're invoked via
# `go run cmd/dock/main.go` / `polar-agent attach` directly so they
# stream logs to the operator's terminal.
#
# Usage:
#   ./scripts/start_polar_services.sh start    # default
#   ./scripts/start_polar_services.sh stop     # stop all
#   ./scripts/start_polar_services.sh status   # list state
#   ./scripts/start_polar_services.sh restart  # bounce all
#
# Prereq: brew on PATH. On a slave that was provisioned by rsync but
# hasn't logged in interactively yet, run
#   eval "$(/opt/homebrew/bin/brew shellenv)"
# first (or set up ~/.zshrc / ~/.cshrc to do it on login).

set -euo pipefail

SERVICES=(redis prometheus grafana nginx)

# Ordered start so e.g. nginx isn't first to come up before its
# upstream is reachable. brew services start is a no-op when already
# running, so re-running this script is safe.
cmd_start() {
  for s in "${SERVICES[@]}"; do
    echo "[start] $s"
    brew services start "$s" 2>&1 | tail -1
  done
  echo
  brew services list | head -1
  for s in "${SERVICES[@]}"; do
    brew services list | awk -v s="$s" '$1==s'
  done
}

# Reverse order on stop so dependents go down before their upstream.
cmd_stop() {
  for ((i=${#SERVICES[@]}-1; i>=0; i--)); do
    s="${SERVICES[$i]}"
    echo "[stop] $s"
    brew services stop "$s" 2>&1 | tail -1
  done
}

cmd_restart() {
  cmd_stop
  echo
  cmd_start
}

cmd_status() {
  brew services list | head -1
  for s in "${SERVICES[@]}"; do
    brew services list | awk -v s="$s" '$1==s'
  done
  echo
  # Quick port probes to spot a started-but-crashed service.
  for entry in "redis 6379" "prometheus 9090" "grafana 3100" "nginx 443"; do
    label=$(echo "$entry" | awk '{print $1}')
    port=$(echo "$entry" | awk '{print $2}')
    if lsof -i ":$port" -sTCP:LISTEN -P >/dev/null 2>&1; then
      echo "  port $port ($label): LISTEN"
    else
      echo "  port $port ($label): not listening"
    fi
  done
}

case "${1:-start}" in
  start)   cmd_start ;;
  stop)    cmd_stop ;;
  restart) cmd_restart ;;
  status)  cmd_status ;;
  -h|--help|help)
    sed -n '2,/^$/p' "$0"
    ;;
  *)
    echo "unknown subcommand: $1" >&2
    echo "usage: $0 {start|stop|restart|status}" >&2
    exit 2
    ;;
esac
