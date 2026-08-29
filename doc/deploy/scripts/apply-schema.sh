#!/bin/bash
# Apply each plugin's migrate SQL to its own DB (as polar). Skips *backfill* (data, not schema).
export PATH="/opt/homebrew/opt/postgresql@16/bin:$PATH"
PW=$(grep '^POSTGRES_DSN=' ~/.config/polar/dock.env | sed -E 's#.*postgres://polar:([^@]+)@.*#\1#')
cd /tmp/plugin-migrate || exit 1
for d in polar-*/; do
  name="${d#polar-}"; name="${name%/}"
  db="polar_${name}"; mdir="${d}scripts/migrate"
  [ -d "$mdir" ] || continue
  echo "===== $name -> $db ====="
  # base schema first
  base="$mdir/${name}-schema.sql"
  [ -f "$base" ] && PGPASSWORD="$PW" psql -q -v ON_ERROR_STOP=1 -h 127.0.0.1 -U polar -d "$db" -f "$base" 2>&1 | tail -2 && echo "  applied ${name}-schema.sql"
  # then remaining non-backfill SQL
  for f in "$mdir"/*.sql; do
    case "$f" in *"${name}-schema.sql"|*backfill*) continue;; esac
    [ -f "$f" ] || continue
    PGPASSWORD="$PW" psql -q -v ON_ERROR_STOP=0 -h 127.0.0.1 -U polar -d "$db" -f "$f" 2>&1 | tail -1 && echo "  applied $(basename "$f")"
  done
  echo "  tables now: $(PGPASSWORD="$PW" psql -h 127.0.0.1 -U polar -d "$db" -tAc "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")"
done
echo "=== restart plugins so workers pick up schema ==="
U=$(id -u)
for name in wg iosdist video packtunnel library hosts projects expense latch lawyer buildings; do
  launchctl kickstart -k "gui/$U/com.polar.${name}-svc" 2>/dev/null
done
echo "restarted"
