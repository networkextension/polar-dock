#!/usr/bin/env bash
set -uo pipefail
export PATH=/opt/homebrew/bin:/opt/homebrew/opt/postgresql@16/bin:$PATH
PGPW=$(cat "$HOME/everest-db.secret")
export PGPASSWORD="$PGPW"
PSQL="psql -h127.0.0.1 -U polar"
PSQLSU="psql -h127.0.0.1 -U local"   # superuser for extensions
UID_NUM=$(id -u)

# --- A. ensure pgvector on stock too ---
echo "=== vector ext on polar_stock ==="
$PSQLSU -d polar_stock -c "CREATE EXTENSION IF NOT EXISTS vector;" 2>&1

# --- B. re-run film migrations in correct order (base first) ---
echo "=== film migrations (ordered) ==="
for sql in film-schema.sql film-schema-m4.sql film-schema-m7.sql film-schema-m8.sql; do
  f="/tmp/zone-migrate/polar-film/$sql"
  [ -f "$f" ] || continue
  ERR=$($PSQL -d polar_film -v ON_ERROR_STOP=0 -f "$f" 2>&1 | grep -iE "ERROR" | grep -viE "already exists" | head -3)
  [ -n "$ERR" ] && echo "  $sql: $ERR" || echo "  $sql ok"
done
# stock schema rerun now that vector exists
echo "=== stock migration rerun ==="
ERR=$($PSQL -d polar_stock -v ON_ERROR_STOP=0 -f /tmp/zone-migrate/polar-stock/stock-schema.sql 2>&1 | grep -iE "ERROR" | grep -viE "already exists" | head -3)
[ -n "$ERR" ] && echo "  $ERR" || echo "  stock ok"

# --- C. register plugin tokens (reuse PLAIN already in each env) ---
echo "=== register tokens ==="
FLEET=("wg|wg-svc|8090" "lawyer|lawyer-svc|8099" "buildings|buildings-svc|8100" "release|release-svc|8101" "stock|stock-svc|8103" "screen|screen-svc|8105" "film|film-svc|8106" "dns|dns-svc|8107")
for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT <<< "$row"
  PLAIN=$(grep '^POLAR_PLUGIN_TOKEN=' "$HOME/$SVC.env" | cut -d= -f2)
  HASH=$(printf '%s' "$PLAIN" | shasum -a 256 | awk '{print $1}')
  $PSQL -d polar_dock -v ON_ERROR_STOP=1 -v nm="$NAME" -v ep="127.0.0.1:$PORT" -v h="$HASH" \
    -c "INSERT INTO plugin_modules(name,display_name,enabled,endpoint,version,plugin_key_hash,config,created_at)
        VALUES(:'nm',:'nm',TRUE,:'ep','',:'h','{}',now())
        ON CONFLICT(name) DO UPDATE SET enabled=TRUE,endpoint=EXCLUDED.endpoint,plugin_key_hash=EXCLUDED.plugin_key_hash;"
  RC=$?
  echo "  $NAME → hash ${HASH:0:12}… rc=$RC"
done

# --- D. kick all svcs ---
echo "=== kickstart ==="
for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT <<< "$row"
  launchctl kickstart -k "gui/${UID_NUM}/polar.${SVC}" 2>/dev/null && echo "  $SVC kicked"
done
sleep 8
echo "=== healthz ==="
for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT <<< "$row"
  CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/healthz")
  echo "  $NAME :$PORT → $CODE"
done
