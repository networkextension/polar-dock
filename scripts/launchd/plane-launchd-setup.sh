#!/bin/bash
# Install launchd agents for the bare-metal Plane stack on the opera box so the
# services survive reboot (replacing the nohup processes). Idempotent.
#   redis(:6380) minio(:9000) api(:8000) worker beat space(:3002) live(:3100)
# web/admin are static SPAs served by nginx — no process needed.
set -u
HOME_DIR="/Users/local"
LA="$HOME_DIR/Library/LaunchAgents"
LOGS="$HOME_DIR/plane-logs"
API="$HOME_DIR/plane-src/apps/api"
SRC="$HOME_DIR/plane-src"
UID_NUM=$(id -u)
BREW="export PATH=/opt/homebrew/bin:/opt/homebrew/sbin:\$PATH"
mkdir -p "$LA" "$LOGS"

# Common env preamble for the Django/celery processes.
APIENV="cd $API && source .venv/bin/activate && set -a && source .env && set +a && export DJANGO_SETTINGS_MODULE=plane.settings.production && export OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES && export MACHINE_SIGNATURE=\$(echo opera-plane-\$(hostname) | shasum -a 256 | cut -d' ' -f1)"

write_plist() {
  local label="$1" cmd="$2"
  cat > "$LA/$label.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>
    <string>/bin/bash</string><string>-lc</string>
    <string>$BREW &amp;&amp; $cmd</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$LOGS/${label}.log</string>
  <key>StandardErrorPath</key><string>$LOGS/${label}.log</string>
  <key>WorkingDirectory</key><string>$SRC</string>
</dict></plist>
PLIST
}

write_plist "com.plane.redis"  "exec redis-server --port 6380 --bind 127.0.0.1 --dir $HOME_DIR/plane-redis-data --save '' --appendonly no"
write_plist "com.plane.minio"  "export MINIO_ROOT_USER=planeminio MINIO_ROOT_PASSWORD=planeminio12345 && exec minio server $HOME_DIR/plane-minio-data --address 127.0.0.1:9000 --console-address 127.0.0.1:9001"
write_plist "com.plane.api"    "$APIENV && exec gunicorn -w 2 -k uvicorn.workers.UvicornWorker plane.asgi:application --bind 127.0.0.1:8000 --max-requests 1200 --max-requests-jitter 1000"
write_plist "com.plane.worker" "$APIENV && exec celery -A plane worker -l info --concurrency=4 --without-gossip --without-mingle"
write_plist "com.plane.beat"   "$APIENV && exec celery -A plane beat -l info"
write_plist "com.plane.space"  "cd $SRC && exec pnpm --filter space start"
write_plist "com.plane.live"   "cd $SRC && exec pnpm --filter live start"

echo "=== stop existing nohup processes (launchd will re-own them) ==="
pkill -f "gunicorn.*plane.asgi" 2>/dev/null
pkill -f "celery -A plane" 2>/dev/null
pkill -f "react-router-serve" 2>/dev/null
pkill -f "filter space start" 2>/dev/null
pkill -f "apps/live" 2>/dev/null
pkill -f "redis-server --port 6380" 2>/dev/null
pkill -f "minio server" 2>/dev/null
sleep 3

echo "=== bootstrap launchd agents ==="
for l in com.plane.redis com.plane.minio com.plane.api com.plane.worker com.plane.beat com.plane.space com.plane.live; do
  launchctl bootout "gui/$UID_NUM/$l" 2>/dev/null
  launchctl bootstrap "gui/$UID_NUM" "$LA/$l.plist" 2>/dev/null && echo "  loaded $l" || echo "  FAILED $l"
done

echo "=== wait + status ==="
sleep 12
for l in com.plane.redis com.plane.minio com.plane.api com.plane.worker com.plane.beat com.plane.space com.plane.live; do
  st=$(launchctl print "gui/$UID_NUM/$l" 2>/dev/null | grep -E 'state =' | head -1 | tr -s ' ')
  printf "  %-20s %s\n" "$l" "${st:-NOT LOADED}"
done
echo "=== port health ==="
for p in 6380 9000 8000 3002 3100; do
  /usr/sbin/lsof -nP -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1 && echo "  :$p UP" || echo "  :$p DOWN"
done
