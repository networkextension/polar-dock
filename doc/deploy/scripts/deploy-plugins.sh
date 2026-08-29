#!/bin/bash
# Deploy all 11 polar plugin svcs on the dev box: per-plugin DB, minted token,
# plugin_modules registration, env file, launchd. Idempotent.
set -u
export PATH="/opt/homebrew/opt/postgresql@16/bin:/opt/homebrew/bin:$PATH"
ENVDIR=~/.config/polar; mkdir -p "$ENVDIR"
LA=~/Library/LaunchAgents
UID_NUM=$(id -u)

# polar role password from dock.env
PW=$(grep '^POSTGRES_DSN=' ~/.config/polar/dock.env | sed -E 's#.*postgres://polar:([^@]+)@.*#\1#')
psql_super() { psql -h 127.0.0.1 -p 5432 -d postgres -tAc "$1"; }
psql_dock()  { PGPASSWORD="$PW" psql -h 127.0.0.1 -p 5432 -U polar -d polar_dock -tAc "$1"; }

# name port dock_env_var(BASE|URL) extra_env
PLUGINS="
wg 8090 BASE POLAR_WG_DATA_DIR=/Users/local/wg-svc-data
iosdist 8091 BASE
video 8092 BASE
packtunnel 8093 BASE
library 8094 BASE
hosts 8095 BASE
projects 8096 BASE
expense 8097 BASE
latch 8098 URL
lawyer 8099 BASE
buildings 8100 BASE
"

echo "$PLUGINS" | while read -r name port dockvar extra; do
  [ -z "$name" ] && continue
  UP=$(echo "$name" | tr a-z A-Z)
  db="polar_${name}"
  echo "===== $name (port $port db $db) ====="

  # 1) database
  if [ "$(psql_super "SELECT 1 FROM pg_database WHERE datname='$db'")" != "1" ]; then
    psql_super "CREATE DATABASE $db OWNER polar" >/dev/null && echo "  db created"
  else echo "  db exists"; fi

  # 2) mint token + register in plugin_modules (idempotent: re-mint + update)
  plaintext="polar_plugin_$(openssl rand -hex 16)"
  hash=$(printf '%s' "$plaintext" | openssl dgst -sha256 | awk '{print $NF}')
  endpoint="http://127.0.0.1:${port}"
  existing=$(psql_dock "SELECT 1 FROM plugin_modules WHERE name='$name'")
  if [ "$existing" = "1" ]; then
    psql_dock "UPDATE plugin_modules SET plugin_key_hash='$hash', endpoint='$endpoint', enabled=TRUE WHERE name='$name'" >/dev/null
    echo "  module updated (token re-minted)"
  else
    psql_dock "INSERT INTO plugin_modules (name, display_name, enabled, endpoint, version, plugin_key_hash, config, created_at) VALUES ('$name','$name',TRUE,'$endpoint','', '$hash','{}'::jsonb, now())" >/dev/null
    echo "  module registered"
  fi

  # 3) env file
  envf="$ENVDIR/${name}-svc.env"
  {
    echo "POLAR_${UP}_DB_DSN=postgres://polar:${PW}@127.0.0.1:5432/${db}?sslmode=disable"
    echo "POLAR_DOCK_${dockvar}=http://127.0.0.1:8080"
    echo "POLAR_PLUGIN_NAME=${name}"
    echo "POLAR_PLUGIN_TOKEN=${plaintext}"
    echo "POLAR_${UP}_LISTEN=127.0.0.1:${port}"
    [ -n "$extra" ] && echo "$extra"
  } > "$envf"
  chmod 600 "$envf"
  [ "$name" = "wg" ] && mkdir -p /Users/local/wg-svc-data

  # 4) launchd plist
  plist="$LA/com.polar.${name}-svc.plist"
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.polar.${name}-svc</string>
  <key>ProgramArguments</key><array>
    <string>/bin/bash</string><string>-lc</string>
    <string>set -a; source "\$HOME/.config/polar/${name}-svc.env"; exec "\$HOME/.local/bin/${name}-svc"</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>StandardOutPath</key><string>/Users/local/polar/${name}-svc.log</string>
  <key>StandardErrorPath</key><string>/Users/local/polar/${name}-svc.log</string>
  <key>ThrottleInterval</key><integer>5</integer>
</dict></plist>
PLIST
  launchctl bootout gui/$UID_NUM/com.polar.${name}-svc 2>/dev/null
  launchctl bootstrap gui/$UID_NUM "$plist" 2>/dev/null && echo "  launchd loaded"
done

sleep 5
echo; echo "===== STATUS ====="
echo "$PLUGINS" | while read -r name port _ _; do
  [ -z "$name" ] && continue
  st=$(launchctl print gui/$UID_NUM/com.polar.${name}-svc 2>/dev/null | grep -E 'state =' | head -1 | tr -s ' ')
  code=$(curl -sS -m3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${port}/" 2>/dev/null)
  printf "  %-11s port %s  http=%s  %s\n" "$name" "$port" "$code" "$st"
done
echo "registered modules: $(psql_dock "SELECT count(*) FROM plugin_modules")"
