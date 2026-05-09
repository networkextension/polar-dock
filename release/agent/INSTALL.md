# polar-agent install + service guide

This package contains the polar-agent binary plus reference scripts
and service unit templates for running it as a long-lived background
service on macOS and FreeBSD. Linux + Windows users can use the
`start.sh` / `stop.sh` shell helpers (or wire their own
systemd / NSSM unit — the binary is a normal long-running process).

## 1. Pick the binary

See `agents/README.md` for the OS / arch matrix. Drop the matching
binary onto PATH:

```sh
# macOS Apple Silicon
sudo cp agents/polar-agent_darwin_arm64 /usr/local/bin/polar-agent
sudo chmod +x /usr/local/bin/polar-agent

# FreeBSD amd64
sudo cp agents/polar-agent_freebsd_amd64 /usr/local/bin/polar-agent
sudo chmod +x /usr/local/bin/polar-agent
```

## 2. Set up config

Once per host:

```sh
polar-agent login --server=https://your-dock.example.com --token=<raw-token-from-/api/agent/tokens>
```

That writes `~/.polar/agent.toml`. If you'd rather drop the file in
place manually (headless box, automation), copy
`dist/agent.toml.example` to `~/.polar/agent.toml` and fill in the
two values.

## 3. Run

Three options:

### A. Quick test (foreground)

```sh
polar-agent attach --bot=bot_xxx --workdir=$HOME/polar-work --tool=kimi
```

Hold the terminal open. Ctrl+C to stop.

### B. Generic background (start.sh / stop.sh)

```sh
BOT_ID=bot_xxx WORKDIR=$HOME/polar-work TOOL=kimi ./dist/start.sh
./dist/stop.sh
```

PID + log default to `/tmp/polar-agent.{pid,log}` — override with
`PIDFILE=` / `LOGFILE=` env vars.

### C. Service (auto-start at boot/login)

#### macOS — launchd LaunchAgent

```sh
# 1. Edit dist/polar-agent.macos.plist — replace BOT_ID_PLACEHOLDER /
#    WORKDIR_PLACEHOLDER / HOME_PLACEHOLDER, and the --tool= value
#    if you want claude or codex instead of kimi.

cp dist/polar-agent.macos.plist ~/Library/LaunchAgents/com.polar.agent.plist
launchctl load ~/Library/LaunchAgents/com.polar.agent.plist

# Verify
launchctl list | grep com.polar.agent
tail -f /tmp/polar-agent.log
```

To uninstall:

```sh
launchctl unload ~/Library/LaunchAgents/com.polar.agent.plist
rm ~/Library/LaunchAgents/com.polar.agent.plist
```

#### FreeBSD — rc.d

```sh
sudo cp dist/polar-agent.rc /usr/local/etc/rc.d/polar_agent
sudo chmod +x /usr/local/etc/rc.d/polar_agent
```

Add to `/etc/rc.conf`:

```sh
polar_agent_enable="YES"
polar_agent_user="polar"           # service user
polar_agent_bot="bot_xxx"
polar_agent_workdir="/var/lib/polar-agent/work"
polar_agent_tool="kimi"
```

Run:

```sh
sudo service polar_agent start
sudo service polar_agent status
sudo service polar_agent stop
```

The script supervises via FreeBSD's `daemon(8)`, which handles
pidfile + log + restart-on-exit automatically (stacks with the
agent's own WS reconnect loop).

## 4. Verify it's actually running

In the dock UI under **Bot Management**, the bot you attached to
should show an "attached" badge and the agent's host info. If not:

- check `~/.polar/agent.toml` has the right server + token
- check the log (`tail -f /tmp/polar-agent.log` or
  `/var/log/polar-agent.log` for FreeBSD service)
- check the dock URL is reachable from this host
  (`curl -I <server>/api/me` should return HTTP 401, not connection
  refused)

## 5. Update later

Updating the binary needs a service restart so launchd / rc.d picks
up the new file:

```sh
# macOS
sudo cp agents/polar-agent_darwin_arm64 /usr/local/bin/polar-agent
launchctl unload ~/Library/LaunchAgents/com.polar.agent.plist
launchctl load ~/Library/LaunchAgents/com.polar.agent.plist

# FreeBSD
sudo cp agents/polar-agent_freebsd_amd64 /usr/local/bin/polar-agent
sudo service polar_agent restart
```
