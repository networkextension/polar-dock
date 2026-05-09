#!/bin/sh
# polar-agent stop helper. Reads the pidfile, sends SIGTERM, waits up
# to 10s, escalates to SIGKILL if still running. Removes the pidfile
# on the way out so a stale file from a crashed process doesn't block
# the next start.

set -eu

PIDFILE="${PIDFILE:-/tmp/polar-agent.pid}"

if [ ! -f "$PIDFILE" ]; then
  echo "no pidfile at $PIDFILE — agent not running (or started outside start.sh)" >&2
  exit 1
fi

PID="$(cat "$PIDFILE")"
if ! kill -0 "$PID" 2>/dev/null; then
  echo "stale pidfile (process $PID gone) — cleaning up"
  rm -f "$PIDFILE"
  exit 0
fi

kill -TERM "$PID"
i=0
while [ "$i" -lt 10 ]; do
  if ! kill -0 "$PID" 2>/dev/null; then
    rm -f "$PIDFILE"
    echo "polar-agent stopped (pid $PID)"
    exit 0
  fi
  sleep 1
  i=$((i + 1))
done

# Didn't shut down cleanly — escalate.
kill -KILL "$PID" 2>/dev/null || true
rm -f "$PIDFILE"
echo "polar-agent killed (force, pid $PID)"
