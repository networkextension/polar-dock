#!/usr/bin/env bash
# Create / update DNS A records via name.com API v4.
#
# Used to bootstrap the per-plugin subdomains under 4950.store. Default
# behavior: create one A record per plugin subdomain (project / expense /
# latch / hosts / iosdist / library / video / packtunnel / wg) pointing
# at the .57 public IP. Idempotent — skips records that already exist
# with the right value; updates if the value differs.
#
# Auth via env (never on the command line, never logged):
#   NAMECOM_USER   name.com account login (the username you SSH into
#                  the panel with, NOT an email).
#   NAMECOM_TOKEN  API token from https://www.name.com/account/settings/api
#
# Usage:
#   NAMECOM_USER=... NAMECOM_TOKEN=... ./name-com-records.sh
#       Bulk-create 9 plugin subdomains -> $TARGET_IP.
#
#   NAMECOM_USER=... NAMECOM_TOKEN=... ./name-com-records.sh --wildcard
#       Instead of 9 records, create one '*' record (wildcard A record).
#
#   NAMECOM_USER=... NAMECOM_TOKEN=... ./name-com-records.sh --dry-run
#       Print what would change without calling the mutating endpoints.
#
#   NAMECOM_USER=... NAMECOM_TOKEN=... ./name-com-records.sh --list
#       Read-only: dump existing A/CNAME records for the domain.
#
# --dry-run does NOT require creds or hit the API — it just prints the
# records it would create.
#
# Override the domain / IP via env:
#   DOMAIN=4950.store TARGET_IP=58.37.118.182 ./name-com-records.sh
#
# ENV — environment prefix for a non-prod tier. When set, records are
# created under <sub>.<ENV>.<DOMAIN> and the core `dock` host is added.
# Empty (default) = the flat <sub>.<DOMAIN> layout (zen / prod).
#
#   # dev environment, all hosts -> the dev box WG IP:
#   ENV=dev TARGET_IP=10.88.0.5 NAMECOM_USER=... NAMECOM_TOKEN=... \
#       ./name-com-records.sh --wildcard      # one *.dev record (recommended)
#   ENV=dev TARGET_IP=10.88.0.5 ... ./name-com-records.sh   # explicit per-host
#   ENV=dev TARGET_IP=10.88.0.5 ./name-com-records.sh --dry-run   # preview, no creds

set -euo pipefail

DOMAIN="${DOMAIN:-4950.store}"
TARGET_IP="${TARGET_IP:-58.37.118.182}"
TTL="${TTL:-300}"
ENV="${ENV:-}"
API="https://api.name.com/v4/domains/${DOMAIN}/records"

SUBDOMAINS=(project expense latch hosts iosdist library video packtunnel wg assets)

# is_private_ip: true for RFC1918 / loopback addresses (10/8, 172.16/12,
# 192.168/16, 127/8). Used to guard the whole-domain wildcard.
is_private_ip() {
    case "$1" in
        10.*|192.168.*|127.*) return 0 ;;
        172.1[6-9].*|172.2[0-9].*|172.3[0-1].*) return 0 ;;
        *) return 1 ;;
    esac
}

MODE="bulk"
DRY_RUN=0
FORCE=0
for arg in "$@"; do
    case "$arg" in
        --wildcard) MODE="wildcard" ;;
        --dry-run)  DRY_RUN=1 ;;
        --force)    FORCE=1 ;;
        --list)     MODE="list" ;;
        -h|--help)
            sed -n '2,30p' "$0"
            exit 0
            ;;
        *)
            echo "unknown arg: $arg" >&2
            exit 64
            ;;
    esac
done

# --dry-run is creds-free and never calls the API, so the guard only
# applies when we're actually going to mutate / read records.
if [ "$DRY_RUN" -eq 0 ]; then
    : "${NAMECOM_USER:?NAMECOM_USER not set — your name.com account login}"
    : "${NAMECOM_TOKEN:?NAMECOM_TOKEN not set — see https://www.name.com/account/settings/api}"
fi

curl_api() {
    # Quiet, fail-on-http-error, basic auth via env (-u "$U:$T" stays out
    # of the process list because curl reads creds via the -u flag value
    # which it does NOT echo). Token + user are NOT included in the
    # generated request log printed by --verbose.
    curl -fsS -u "${NAMECOM_USER}:${NAMECOM_TOKEN}" \
         -H "Content-Type: application/json" \
         "$@"
}

list_records() {
    # name.com paginates with ?perPage=N and nextPage. 1000 is plenty
    # for a personal domain.
    curl_api "${API}?perPage=1000"
}

# JSON parsing: prefer jq when present (cleaner); fall back to python3
# which ships with macOS by default. python3 is plenty for this volume.
parse_record() {
    # stdin: full /records JSON
    # args: host_to_match  type_to_match (e.g. "expense" "A" or "*" "A")
    # output: tab-separated <id>\t<answer>  for each match. Empty if none.
    local host="$1" rtype="$2"
    if command -v jq >/dev/null 2>&1; then
        jq -r --arg host "$host" --arg type "$rtype" \
            '.records[]? | select((.host // "") == $host) | select(.type == $type) | "\(.id)\t\(.answer)"'
    else
        python3 -c "
import json, sys
data = json.load(sys.stdin)
host = sys.argv[1]
rtype = sys.argv[2]
for r in data.get('records', []):
    if r.get('host', '') == host and r.get('type') == rtype:
        print(f\"{r['id']}\t{r['answer']}\")
" "$host" "$rtype"
    fi
}

create_record() {
    local host="$1" rtype="$2" answer="$3"
    local label="${host:+${host}.}${DOMAIN}"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "[dry-run] create ${rtype} ${label} -> ${answer}"
        return 0
    fi
    local body
    body=$(printf '{"host":"%s","type":"%s","answer":"%s","ttl":%d}' "$host" "$rtype" "$answer" "$TTL")
    curl_api -X POST -d "$body" "$API" >/dev/null
    echo "created ${rtype} ${label} -> ${answer}"
}

update_record() {
    local id="$1" host="$2" rtype="$3" answer="$4"
    local label="${host:+${host}.}${DOMAIN}"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "[dry-run] update ${rtype} ${label} (id=${id}) -> ${answer}"
        return 0
    fi
    local body
    body=$(printf '{"host":"%s","type":"%s","answer":"%s","ttl":%d}' "$host" "$rtype" "$answer" "$TTL")
    curl_api -X PUT -d "$body" "${API}/${id}" >/dev/null
    echo "updated ${rtype} ${label} (id=${id}) -> ${answer}"
}

ensure_record() {
    local host="$1" answer="$2"
    local label="${host:+${host}.}${DOMAIN}"
    local existing
    existing=$(echo "$ALL_RECORDS" | parse_record "$host" "A")
    if [ -z "$existing" ]; then
        create_record "$host" "A" "$answer"
        return 0
    fi
    local id current
    id=$(echo "$existing" | awk -F '\t' '{print $1; exit}')
    current=$(echo "$existing" | awk -F '\t' '{print $2; exit}')
    if [ "$current" = "$answer" ]; then
        echo "ok       A ${label} already -> ${answer}"
    else
        update_record "$id" "$host" "A" "$answer"
    fi
}

# Single fetch of the record set, reused across ensure_record calls so we
# don't burn the API budget with N read calls. Skipped on --dry-run
# (which is creds-free): an empty set makes ensure_record preview every
# host as a "create".
if [ "$DRY_RUN" -eq 1 ]; then
    ALL_RECORDS='{"records":[]}'
else
    ALL_RECORDS=$(list_records)
fi

case "$MODE" in
    list)
        echo "${ALL_RECORDS}" | (
            if command -v jq >/dev/null 2>&1; then
                jq -r '.records[]? | "\(.type)\t\(.host // "@")\t\(.answer)\t(ttl=\(.ttl))"'
            else
                python3 -c "
import json, sys
d = json.load(sys.stdin)
for r in d.get('records', []):
    print(f\"{r.get('type','?')}\t{r.get('host') or '@'}\t{r.get('answer','')}\t(ttl={r.get('ttl','?')})\")
"
            fi
        )
        ;;
    wildcard)
        # ENV=dev -> "*.dev" (one record covers dock + every module under
        # dev.4950.store, routed by Host on the target box).
        whost="*"
        [ -n "$ENV" ] && whost="*.${ENV}"
        # Guard rail: the bare "*" wildcard is the WHOLE-DOMAIN catch-all.
        # Pointing it at a private/RFC1918 IP (e.g. a 10.88.0.x WG address)
        # silently breaks every unspecified public subdomain. Refuse unless
        # --force. (A *.<ENV> wildcard is scoped + exempt — that's the
        # intended dev/stage use.) This exact mistake repointed
        # *.4950.store from the public IP to 10.88.0.5; never again.
        if [ "$whost" = "*" ] && is_private_ip "$TARGET_IP" && [ "$FORCE" -ne 1 ]; then
            echo "REFUSING: bare '*' (whole-domain) wildcard -> private IP ${TARGET_IP}." >&2
            echo "  This would break every unspecified public subdomain of ${DOMAIN}." >&2
            echo "  Did you mean a scoped wildcard? Set ENV=<tier> for *.\${ENV}.${DOMAIN}." >&2
            echo "  To override intentionally, re-run with --force." >&2
            exit 65
        fi
        ensure_record "$whost" "$TARGET_IP"
        ;;
    bulk)
        # In an ENV tier, include the core `dock` host and suffix every
        # subdomain with .<ENV> (e.g. dock.dev, wg.dev, assets.dev ...).
        hosts=("${SUBDOMAINS[@]}")
        [ -n "$ENV" ] && hosts=(dock "${SUBDOMAINS[@]}")
        for sub in "${hosts[@]}"; do
            host="$sub"
            [ -n "$ENV" ] && host="${sub}.${ENV}"
            ensure_record "$host" "$TARGET_IP"
        done
        ;;
esac
