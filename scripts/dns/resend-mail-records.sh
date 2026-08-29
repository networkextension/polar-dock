#!/usr/bin/env bash
# Add the Resend (SES) sending records for 4950.store via the name.com API.
# Requires: NAMECOM_USER + NAMECOM_TOKEN in the environment.
#   NAMECOM_USER=you NAMECOM_TOKEN=xxxx bash scripts/dns/resend-mail-records.sh
# Idempotent: skips a record if an identical host+type already exists.
set -euo pipefail

DOMAIN="${DOMAIN:-4950.store}"
TTL="${TTL:-300}"
API="https://api.name.com/v4/domains/${DOMAIN}/records"
: "${NAMECOM_USER:?set NAMECOM_USER}"
: "${NAMECOM_TOKEN:?set NAMECOM_TOKEN}"
AUTH="-u ${NAMECOM_USER}:${NAMECOM_TOKEN}"

existing="$(curl -fsS $AUTH "$API")"

ensure() { # host type answer [priority]
  local host="$1" type="$2" answer="$3" prio="${4:-}"
  if printf '%s' "$existing" | python3 -c "
import sys,json
host,typ='$host','$type'
d=json.load(sys.stdin)
for r in d.get('records',[]):
    if r.get('host','')==host and r.get('type')==typ:
        print('hit'); break
" | grep -q hit; then
    echo "skip   $type ${host:-@}.$DOMAIN (already present)"; return
  fi
  local body
  if [ -n "$prio" ]; then
    body=$(printf '{"host":"%s","type":"%s","answer":"%s","ttl":%s,"priority":%s}' "$host" "$type" "$answer" "$TTL" "$prio")
  else
    body=$(printf '{"host":"%s","type":"%s","answer":"%s","ttl":%s}' "$host" "$type" "$answer" "$TTL")
  fi
  curl -fsS $AUTH -H 'Content-Type: application/json' -X POST "$API" -d "$body" >/dev/null
  echo "create $type ${host:-@}.$DOMAIN -> $answer ${prio:+(prio $prio)}"
}

# Resend / Amazon SES records for 4950.store
ensure "resend._domainkey" TXT "p=MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDpCZjVWMqRMDGjqTM+dszjuzjQVLU2bpOLRPv5X1366kn/urv1OFpfeHkkwJvcqrgasC5eVyXkIgw3MSeOBgcmWfDVD++W1L4lQvx+IJHu3lX2y7xQHrS7qIBp1fxZb1GQVIwapX9xfdZ8EaBMRu7os1Q9cEMd9wQzlDtSPuordwIDAQAB"
ensure "send"             TXT "v=spf1 include:amazonses.com ~all"
ensure "send"             MX  "feedback-smtp.us-east-1.amazonses.com" 10

echo "done."
