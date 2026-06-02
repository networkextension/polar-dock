#!/usr/bin/env bash
# Deploy the shared official-module chrome (one stylesheet + assets for every
# plugin subdomain + dock). Canonical source = polar-ui-common/static/.
# Run after changing polar-ui-common styles. See snippets/polar-plugins.conf
# (location = /styles.css, location ^~ /assets/).
set -euo pipefail
ZEN="${1:-local@10.88.0.1}"
UIC="$(cd "$(dirname "$0")/../modules/polar-ui-common" && pwd)"
( cd "$UIC" && npm run build >/dev/null )
ssh "$ZEN" 'mkdir -p /Users/local/var/polar-ui-shared'
rsync -az "$UIC/static/" "$ZEN:/Users/local/var/polar-ui-shared/"
echo "deployed shared UI chrome to $ZEN:/Users/local/var/polar-ui-shared/"
