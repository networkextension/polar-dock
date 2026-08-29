#!/usr/bin/env bash
# Issue (or renew) a Let's Encrypt cert for opera.daotian.ai via the Polar DNS
# control-plane (dns.4950.store) DNS-01 hook. Run ON the opera box.
#
# The DNS-01 TXT records are written through Polar DNS (which proxies to the
# real provider — Cloudflare for daotian.ai), so NO Cloudflare token lives on
# this box. acme.sh auto-renews by re-invoking the hook on its cron.
#
# Prereq: ~/.acme.sh/dnsapi/dns_polar.sh installed (scp it from the repo).
#
# Usage:
#   export POLAR_DNS_TOKEN='<dock access token>'   # required, never commit this
#   ./issue-opera-cert.sh
#
set -euo pipefail

: "${POLAR_DNS_TOKEN:?set POLAR_DNS_TOKEN to a dock access token}"
export POLAR_DNS_WS="${POLAR_DNS_WS:-6e94ff215bf0fb1f2002d12b175d408b}"   # lzhu workspace
export POLAR_DNS_BASE="${POLAR_DNS_BASE:-https://dns.4950.store:2443/api/dns}"

ACME="${ACME:-$HOME/.acme.sh/acme.sh}"
DOMAIN="opera.daotian.ai"
TLS_DIR="${TLS_DIR:-/Users/local/polar/tls}"
RELOAD_CMD="${RELOAD_CMD:-nginx -s reload}"

if [ ! -x "$ACME" ]; then
  echo "acme.sh not found at $ACME" >&2
  exit 1
fi
if [ ! -f "$HOME/.acme.sh/dnsapi/dns_polar.sh" ]; then
  echo "dns_polar.sh hook missing at ~/.acme.sh/dnsapi/dns_polar.sh — scp it first" >&2
  exit 1
fi

mkdir -p "$TLS_DIR"

echo "==> Issuing $DOMAIN (+ wildcard) via dns_polar / Let's Encrypt ..."
"$ACME" --issue --dns dns_polar \
  -d "$DOMAIN" -d "*.$DOMAIN" \
  --server letsencrypt

echo "==> Installing cert to $TLS_DIR and reloading nginx ..."
"$ACME" --install-cert -d "$DOMAIN" \
  --key-file       "$TLS_DIR/$DOMAIN.key" \
  --fullchain-file "$TLS_DIR/$DOMAIN.crt" \
  --reloadcmd      "$RELOAD_CMD"

echo "==> Done. Cert SANs:"
openssl x509 -in "$TLS_DIR/$DOMAIN.crt" -noout -text | grep -A1 "Subject Alternative Name" || true
