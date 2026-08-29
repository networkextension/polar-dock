#!/usr/bin/env sh
# shellcheck disable=SC2034
#
# acme.sh DNS hook for the Polar DNS control-plane (dns.4950.store).
#
# Polar DNS is a provider-first control plane: it writes the record at the
# real authoritative provider (Cloudflare / name.com) and mirrors it locally.
# That makes it a drop-in DNS-01 backend for acme.sh — no direct Cloudflare
# token needed on the box, just a dock access token + workspace id.
#
# Install:
#   cp dns_polar.sh ~/.acme.sh/dnsapi/dns_polar.sh
#
# Configure (saved into account.conf after first run, so renewals are hands-off):
#   export POLAR_DNS_TOKEN="<dock access token>"
#   export POLAR_DNS_WS="<workspace id, e.g. lzhu>"
#   export POLAR_DNS_BASE="https://dns.4950.store:2443/api/dns"   # optional, this is the default
#
# Use:
#   acme.sh --issue --dns dns_polar -d opera.daotian.ai -d '*.opera.daotian.ai' --server letsencrypt
#
# Unlike --yes-I-know-dns-manual-mode, this AUTO-RENEWS: acme.sh re-invokes
# the add/rm hooks on its cron without any manual TXT editing.

POLAR_DNS_DEFAULT_BASE="https://dns.4950.store:2443/api/dns"

########  Public hooks  ########

#Usage: dns_polar_add  _acme-challenge.opera.daotian.ai  "txt-value"
dns_polar_add() {
  fulldomain=$1
  txtvalue=$2
  _polar_init || return 1
  _polar_get_zone "$fulldomain" || return 1
  _info "Polar DNS: adding TXT $_sub_domain in zone $_zone_name (id $_zone_id)"

  _body="{\"type\":\"TXT\",\"name\":\"$_sub_domain\",\"content\":\"$txtvalue\",\"ttl\":120}"
  _response="$(_post "$_body" "$POLAR_DNS_BASE/zones/$_zone_id/records" "" "POST")"
  _debug2 _response "$_response"
  if _contains "$_response" '"id":"dr_'; then
    _info "Polar DNS: TXT record created."
    return 0
  fi
  _err "Polar DNS: failed to add TXT record: $_response"
  return 1
}

#Usage: dns_polar_rm  _acme-challenge.opera.daotian.ai  "txt-value"
dns_polar_rm() {
  fulldomain=$1
  txtvalue=$2
  _polar_init || return 1
  _polar_get_zone "$fulldomain" || return 1

  _list="$(_get "$POLAR_DNS_BASE/zones/$_zone_id/records")"
  _debug2 _list "$_list"
  # Split the JSON array into one object per line, isolate the record whose
  # content matches our challenge value, pull its dr_ id.
  _rid="$(echo "$_list" | sed 's/},{/}\
{/g' | grep -F "\"content\":\"$txtvalue\"" | _egrep_o '"id":"dr_[^"]*"' | cut -d'"' -f4 | head -n1)"
  if [ -z "$_rid" ]; then
    _info "Polar DNS: no matching TXT record to delete (already gone?)."
    return 0
  fi
  _info "Polar DNS: deleting TXT record $_rid"
  _response="$(_post "" "$POLAR_DNS_BASE/records/$_rid" "" "DELETE")"
  _debug2 _response "$_response"
  if _contains "$_response" '"deleted":true'; then
    _info "Polar DNS: TXT record deleted."
    return 0
  fi
  _err "Polar DNS: failed to delete TXT record: $_response"
  return 1
}

########  Internal  ########

_polar_init() {
  POLAR_DNS_BASE="${POLAR_DNS_BASE:-$(_readaccountconf_mutable POLAR_DNS_BASE)}"
  POLAR_DNS_TOKEN="${POLAR_DNS_TOKEN:-$(_readaccountconf_mutable POLAR_DNS_TOKEN)}"
  POLAR_DNS_WS="${POLAR_DNS_WS:-$(_readaccountconf_mutable POLAR_DNS_WS)}"

  [ -z "$POLAR_DNS_BASE" ] && POLAR_DNS_BASE="$POLAR_DNS_DEFAULT_BASE"

  if [ -z "$POLAR_DNS_TOKEN" ] || [ -z "$POLAR_DNS_WS" ]; then
    POLAR_DNS_TOKEN=""
    POLAR_DNS_WS=""
    _err "POLAR_DNS_TOKEN and POLAR_DNS_WS must be set."
    _err "  export POLAR_DNS_TOKEN=<dock access token>"
    _err "  export POLAR_DNS_WS=<workspace id>"
    return 1
  fi

  # Persist for unattended renewals.
  _saveaccountconf_mutable POLAR_DNS_BASE "$POLAR_DNS_BASE"
  _saveaccountconf_mutable POLAR_DNS_TOKEN "$POLAR_DNS_TOKEN"
  _saveaccountconf_mutable POLAR_DNS_WS "$POLAR_DNS_WS"

  # Headers consumed by acme.sh's _get/_post.
  export _H1="Authorization: Bearer $POLAR_DNS_TOKEN"
  export _H2="X-Workspace-Id: $POLAR_DNS_WS"
  export _H3="Content-Type: application/json"
}

# Resolve the longest registered zone that is a suffix of $1.
# Sets: _zone_id, _zone_name, _sub_domain (name relative to the zone apex).
_polar_get_zone() {
  domain=$1
  _zones="$(_get "$POLAR_DNS_BASE/zones")"
  _debug2 _zones "$_zones"
  if ! _contains "$_zones" '"zone_name"'; then
    _err "Polar DNS: could not list zones (check token/workspace): $_zones"
    return 1
  fi
  _zlines="$(echo "$_zones" | sed 's/},{/}\
{/g')"

  i=1
  while true; do
    _h="$(printf "%s" "$domain" | cut -d . -f "$i"-100)"
    [ -z "$_h" ] && break
    _block="$(echo "$_zlines" | grep -F "\"zone_name\":\"$_h\"" | head -n1)"
    if [ -n "$_block" ]; then
      _zone_id="$(echo "$_block" | _egrep_o '"id":"dz_[^"]*"' | cut -d'"' -f4 | head -n1)"
      _zone_name="$_h"
      _sub_domain="$(printf "%s" "$domain" | sed "s/\\.${_h}\$//")"
      [ "$_sub_domain" = "$domain" ] && _sub_domain="@"
      return 0
    fi
    i="$(_math "$i" + 1)"
  done
  _err "Polar DNS: no zone in workspace matches $domain"
  return 1
}
