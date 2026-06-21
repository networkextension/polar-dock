#!/usr/bin/env python3
"""Polar alert receiver — consumes Alertmanager webhooks and fans out:

  1. macOS notification (osascript) — every firing/resolved alert.
  2. Plane work item — critical firing → create (dedup by fingerprint);
     resolved → comment "已恢复" + mark Done.
  3. dock /internal/v1/notify — Polar in-app (degrades gracefully while the
     dock notify endpoint is still a stub; becomes real in P0b).

Loopback only (127.0.0.1:9095). stdlib only. State in STATE_FILE so a restart
doesn't reopen already-filed tickets.

Config via env (see polar.alert-receiver.plist):
  ALERT_PLANE_PROJECT   Plane project id for tickets ("" = skip Plane)
  PLANE_SECRETS         path to plane-secrets.env (PLANE_API_TOKEN/_BASE_URL/_WS_SLUG)
  DOCK_NOTIFY_URL       dock notify endpoint ("" = skip)
  DOCK_INTERNAL_KEY     internal plugin bearer for dock notify (P0b)
  ALERT_ROOM_ID         dock chat room id for in-app delivery (P0b)
  STATE_FILE            fingerprint→ticket map
"""
import json, os, subprocess, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, HTTPServer

PLANE_PROJECT = os.environ.get("ALERT_PLANE_PROJECT", "").strip()
PLANE_SECRETS = os.environ.get("PLANE_SECRETS", "/Users/local/plane-secrets.env")
# Polar in-app: post to a chat room via the open-api ops key (works on zen —
# the open-api-token middleware is wired to /api/*; the token's owner is the
# room creator/member so the post is authorized).
POLAR_API_BASE = os.environ.get("POLAR_API_BASE", "http://127.0.0.1:8080").rstrip("/")
POLAR_OPS_TOKEN = os.environ.get("POLAR_OPS_TOKEN", "").strip()
POLAR_WS = os.environ.get("POLAR_WS", "").strip()
ALERT_ROOM_ID = os.environ.get("ALERT_ROOM_ID", "").strip()
STATE_FILE = os.environ.get("STATE_FILE", "/Users/local/var/alert-state.json")


def load_env_file(path):
    out = {}
    try:
        with open(path) as f:
            for line in f:
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, v = line.split("=", 1)
                out[k.strip()] = v.strip().strip('"').strip("'")
    except OSError:
        pass
    return out


PLANE = load_env_file(PLANE_SECRETS)
PLANE_TOKEN = PLANE.get("PLANE_API_TOKEN", "")
# Prefer the on-zen loopback Plane API; the secrets file holds the public URL.
PLANE_BASE = os.environ.get("ALERT_PLANE_BASE", PLANE.get("PLANE_BASE_URL", "http://127.0.0.1:8000")).rstrip("/")

# These hosts route python/urllib through a dead system proxy — bypass it.
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
PLANE_WS = PLANE.get("PLANE_WS_SLUG", "polar")
_done_state = {"id": None}  # cached Plane "Done" state id


def load_state():
    try:
        with open(STATE_FILE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(s):
    os.makedirs(os.path.dirname(STATE_FILE), exist_ok=True)
    tmp = STATE_FILE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(s, f)
    os.replace(tmp, STATE_FILE)


def http(method, url, headers=None, body=None, timeout=8):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with _opener.open(req, timeout=timeout) as r:
            raw = r.read().decode()
            return r.status, (json.loads(raw) if raw else {})
    except urllib.error.HTTPError as e:
        return e.code, {"error": e.read().decode()[:200]}
    except Exception as e:  # noqa
        return 0, {"error": str(e)}


def notify_macos(title, message):
    # escape double quotes for AppleScript string literals
    t = title.replace('"', "'")[:120]
    m = message.replace('"', "'")[:240]
    try:
        subprocess.run(
            ["osascript", "-e", f'display notification "{m}" with title "{t}"'],
            timeout=8, check=False,
        )
    except Exception:  # noqa
        pass


def plane_headers():
    return {"X-Api-Key": PLANE_TOKEN, "Content-Type": "application/json"}


def plane_done_state_id():
    if _done_state["id"] is not None:
        return _done_state["id"]
    url = f"{PLANE_BASE}/api/v1/workspaces/{PLANE_WS}/projects/{PLANE_PROJECT}/states/"
    st, body = http("GET", url, plane_headers())
    if st == 200:
        for s in body.get("results", []):
            if s.get("group") == "completed":
                _done_state["id"] = s["id"]
                break
    return _done_state["id"]


def plane_create(name, desc_html, priority="urgent"):
    url = f"{PLANE_BASE}/api/v1/workspaces/{PLANE_WS}/projects/{PLANE_PROJECT}/issues/"
    st, body = http("POST", url, plane_headers(),
                    {"name": name[:240], "description_html": desc_html, "priority": priority})
    return body.get("id") if st in (200, 201) else None


def plane_comment(issue_id, html):
    url = f"{PLANE_BASE}/api/v1/workspaces/{PLANE_WS}/projects/{PLANE_PROJECT}/issues/{issue_id}/comments/"
    http("POST", url, plane_headers(), {"comment_html": html})


def plane_mark_done(issue_id):
    sid = plane_done_state_id()
    if not sid:
        return
    url = f"{PLANE_BASE}/api/v1/workspaces/{PLANE_WS}/projects/{PLANE_PROJECT}/issues/{issue_id}/"
    http("PATCH", url, plane_headers(), {"state": sid})


def polar_room_post(text):
    """Polar 应用内: post to the alerts chat room via the ops key."""
    if not (POLAR_OPS_TOKEN and ALERT_ROOM_ID):
        return
    url = f"{POLAR_API_BASE}/api/rooms/{ALERT_ROOM_ID}/messages"
    http("POST", url, {"Authorization": "Bearer " + POLAR_OPS_TOKEN,
                       "X-Workspace-Id": POLAR_WS, "Content-Type": "application/json"},
         {"content": text[:1800], "message_type": "text"})


def handle_alert(a, state):
    lbl = a.get("labels", {})
    ann = a.get("annotations", {})
    fp = a.get("fingerprint") or (lbl.get("alertname", "") + "/" + lbl.get("host", ""))
    name = lbl.get("alertname", "alert")
    host = lbl.get("host", "")
    sev = lbl.get("severity", "")
    status = a.get("status", "firing")
    summary = ann.get("summary", name)
    desc = ann.get("description", "")
    title = f"[{sev or status}] {name} {host}".strip()

    if name == "Watchdog":
        return  # heartbeat handled by the external deadman, not here

    if status == "firing":
        notify_macos(title, summary)
        icon = "🔴" if sev == "critical" else "🟠"
        polar_room_post(f"{icon} {title}\n{summary}" + ("\n" + desc if desc else ""))
        if sev == "critical" and PLANE_PROJECT and fp not in state:
            html = f"<p><b>{summary}</b></p><p>{desc}</p><pre>{json.dumps(lbl, ensure_ascii=False)}</pre>"
            iid = plane_create(f"[告警] {name} {host}", html)
            if iid:
                state[fp] = iid
                save_state(state)
    else:  # resolved
        notify_macos("已恢复 " + title, summary)
        polar_room_post(f"✅ 已恢复 {title}\n{summary}")
        iid = state.pop(fp, None)
        if iid and PLANE_PROJECT:
            plane_comment(iid, f"<p>✅ 已恢复:{summary}</p>")
            plane_mark_done(iid)
            save_state(state)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):  # quieter logs
        pass

    def do_POST(self):
        if self.path.rstrip("/") != "/alert":
            self.send_response(404); self.end_headers(); return
        try:
            n = int(self.headers.get("Content-Length", 0))
            payload = json.loads(self.rfile.read(n) or "{}")
        except Exception:  # noqa
            self.send_response(400); self.end_headers(); return
        state = load_state()
        for a in payload.get("alerts", []):
            try:
                handle_alert(a, state)
            except Exception as e:  # noqa
                print("alert handling error:", e, flush=True)
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")

    def do_GET(self):
        # health
        self.send_response(200); self.end_headers(); self.wfile.write(b"ok")


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", 9095), Handler).serve_forever()
