#!/usr/bin/env bash
set -uo pipefail
export PATH=/opt/homebrew/bin:/opt/homebrew/opt/postgresql@16/bin:$PATH
export PGPASSWORD=$(cat "$HOME/everest-db.secret")
PSQL="psql -h127.0.0.1 -U polar -d polar_dock"
UID_NUM=$(id -u)
FLEET=("wg|wg-svc|8090" "lawyer|lawyer-svc|8099" "buildings|buildings-svc|8100" "release|release-svc|8101" "stock|stock-svc|8103" "screen|screen-svc|8105" "film|film-svc|8106" "dns|dns-svc|8107")
for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT <<< "$row"
  PLAIN=$(grep '^POLAR_PLUGIN_TOKEN=' "$HOME/$SVC.env" | cut -d= -f2)
  HASH=$(printf '%s' "$PLAIN" | shasum -a 256 | awk '{print $1}')
  $PSQL -c "INSERT INTO plugin_modules(name,display_name,enabled,endpoint,version,plugin_key_hash,config,created_at) VALUES('$NAME','$NAME',TRUE,'127.0.0.1:$PORT','','$HASH','{}',now()) ON CONFLICT(name) DO UPDATE SET enabled=TRUE,endpoint=EXCLUDED.endpoint,plugin_key_hash=EXCLUDED.plugin_key_hash;" && echo "  $NAME registered" || echo "  $NAME FAIL"
done
echo "=== kickstart + wait ==="
for row in "${FLEET[@]}"; do IFS='|' read -r NAME SVC PORT <<< "$row"; launchctl kickstart -k "gui/${UID_NUM}/polar.${SVC}" 2>/dev/null; done
sleep 10
for row in "${FLEET[@]}"; do IFS='|' read -r NAME SVC PORT <<< "$row"; echo "  $NAME :$PORT → $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:${PORT}/healthz)"; done
