#!/usr/bin/env bash
# everest plugin fleet deploy — run ON 192.168.11.82
set -uo pipefail
export PATH=/opt/homebrew/bin:/opt/homebrew/opt/postgresql@16/bin:$PATH
PGPW=$(cat "$HOME/everest-db.secret")
export PGPASSWORD="$PGPW"
PSQL="psql -h127.0.0.1 -U polar"
BIN=/tmp/zone-bin
MIG=/tmp/zone-migrate
UID_NUM=$(id -u)

# name svc port db repo version
FLEET=(
  "wg|wg-svc|8090|polar_wg|polar-wg|4da7b52"
  "lawyer|lawyer-svc|8099|polar_lawyer|polar-lawyer|10b6ee7"
  "buildings|buildings-svc|8100|polar_buildings|polar-buildings|9532eaa"
  "release|release-svc|8101|polar_release|polar-release|615b6d1"
  "stock|stock-svc|8103|polar_stock|polar-stock|ef8a747"
  "screen|screen-svc|8105|polar_screen|polar-screen|1618b12"
  "film|film-svc|8106|polar_film|polar-film|524ccf5"
  "dns|dns-svc|8107|polar_dns|polar-dns|f500312"
)

for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT DB REPO VER <<< "$row"
  UPPER=$(echo "$NAME" | tr '[:lower:]' '[:upper:]')
  echo "================ $NAME ($SVC :$PORT $DB) ================"

  # 1. binary
  cp "$BIN/$SVC" "$HOME/.local/bin/$SVC" && chmod +x "$HOME/.local/bin/$SVC"
  echo "  [bin] installed"

  # 2. migrations (each .sql as polar, DB owner)
  if [ -d "$MIG/$REPO" ]; then
    for sql in "$MIG/$REPO"/*.sql; do
      [ -f "$sql" ] || continue
      OUT=$($PSQL -d "$DB" -v ON_ERROR_STOP=0 -f "$sql" 2>&1 | grep -iE "ERROR" | grep -viE "already exists" | head -3)
      if [ -n "$OUT" ]; then echo "  [migrate] $(basename $sql): $OUT"; else echo "  [migrate] $(basename $sql) ok"; fi
    done
  fi

  # 3. plugin token (mint + upsert hash)
  PLAIN="polar_plugin_$(openssl rand -hex 16)"
  HASH=$(printf '%s' "$PLAIN" | shasum -a 256 | awk '{print $1}')
  $PSQL -d polar_dock -v ON_ERROR_STOP=1 \
    -v nm="$NAME" -v dn="$NAME" -v ep="127.0.0.1:$PORT" -v ver="$VER" -v h="$HASH" \
    -c "INSERT INTO plugin_modules(name,display_name,enabled,endpoint,version,plugin_key_hash,config,created_at)
        VALUES(:'nm',:'dn',TRUE,:'ep',:'ver',:'h','{}',now())
        ON CONFLICT(name) DO UPDATE SET enabled=TRUE,endpoint=EXCLUDED.endpoint,version=EXCLUDED.version,plugin_key_hash=EXCLUDED.plugin_key_hash;" >/dev/null 2>&1 \
    && echo "  [token] minted+registered" || echo "  [token] FAILED"

  # 4. env file
  cat > "$HOME/$SVC.env" <<EOF
POLAR_${UPPER}_DB_DSN=postgres://polar:${PGPW}@127.0.0.1:5432/${DB}?sslmode=disable
POLAR_DOCK_BASE=http://127.0.0.1:8080
POLAR_PLUGIN_NAME=${NAME}
POLAR_PLUGIN_TOKEN=${PLAIN}
POLAR_${UPPER}_LISTEN=127.0.0.1:${PORT}
POLAR_${UPPER}_VERSION=${VER}
POLAR_${UPPER}_PUBLIC_BASE_URL=https://${NAME}.everest.4950.store
EOF
  echo "  [env] written"

  # 5. wrapper
  cat > "$HOME/.local/bin/polar-${NAME}-launch.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
set -a; source \$HOME/$SVC.env; set +a
cd \$HOME; exec \$HOME/.local/bin/$SVC
EOF
  chmod +x "$HOME/.local/bin/polar-${NAME}-launch.sh"

  # 6. plist
  cat > "$HOME/Library/LaunchAgents/polar.${SVC}.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>polar.${SVC}</string>
<key>ProgramArguments</key><array><string>${HOME}/.local/bin/polar-${NAME}-launch.sh</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>StandardOutPath</key><string>/tmp/polar.${SVC}.log</string><key>StandardErrorPath</key><string>/tmp/polar.${SVC}.log</string>
<key>ThrottleInterval</key><integer>10</integer></dict></plist>
EOF
  echo "  [plist] written"

  # 7. (re)load launchd
  launchctl bootout "gui/${UID_NUM}/polar.${SVC}" 2>/dev/null || true
  launchctl bootstrap "gui/${UID_NUM}" "$HOME/Library/LaunchAgents/polar.${SVC}.plist" 2>/dev/null \
    && echo "  [launchd] bootstrapped" || { launchctl load "$HOME/Library/LaunchAgents/polar.${SVC}.plist" 2>/dev/null; echo "  [launchd] load fallback"; }
  launchctl kickstart -k "gui/${UID_NUM}/polar.${SVC}" 2>/dev/null || true
done

echo "================ waiting 8s for boot ================"
sleep 8
echo "================ healthz ================"
for row in "${FLEET[@]}"; do
  IFS='|' read -r NAME SVC PORT DB REPO VER <<< "$row"
  CODE=$(curl -s -o /dev/null -w "%{http_code}" "http://127.0.0.1:${PORT}/healthz" 2>/dev/null)
  echo "  $NAME :$PORT /healthz → $CODE"
done
