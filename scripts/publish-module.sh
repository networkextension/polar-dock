#!/usr/bin/env bash
# publish-module.sh — build a plugin module binary, stage it where dock
# serves OTA binaries, and register the release manifest (Track 3).
#
# The pull model (doc/arch/module-platform-plan.md): dock hosts binaries
# from POLAR_PLUGIN_DIST_DIR/<name>/<file>; a module's heartbeat learns a
# newer build is available and self-updates. This script does the publish
# half — build → sha256 → place file → POST the manifest row.
#
# Usage:
#   scripts/publish-module.sh <name> <version> <module-src-dir> [main-pkg]
#
# Example (buildings, from the dock repo root):
#   DOCK_BASE=http://127.0.0.1:8080 \
#   DOCK_ADMIN_TOKEN=polar_xxx \
#   PLUGIN_DIST_DIR=/Users/local/.local/share/polar-plugin-dist \
#   scripts/publish-module.sh buildings 0.0.2 ../polar-buildings ./cmd/buildings-svc
#
# Env:
#   DOCK_BASE         dock base URL (default http://127.0.0.1:8080)
#   DOCK_ADMIN_TOKEN  a site-admin access token (Bearer) — required for the POST
#   PLUGIN_DIST_DIR   local dir dock reads (POLAR_PLUGIN_DIST_DIR). The binary
#                     is written to $PLUGIN_DIST_DIR/<name>/<file>.
#   RSYNC_DEST        optional rsync target for the dist dir when dock runs on
#                     another box, e.g. local@10.88.0.5:/Users/local/.local/share/polar-plugin-dist
#                     When set, the file is rsync'd there instead of (or in
#                     addition to) the local PLUGIN_DIST_DIR.
#   GOOS, GOARCH      build target (default darwin/arm64)
#   CHANNEL           release channel (default stable)
set -euo pipefail

NAME="${1:?usage: publish-module.sh <name> <version> <module-src-dir> [main-pkg]}"
VERSION="${2:?version required}"
SRC_DIR="${3:?module source dir required}"
MAIN_PKG="${4:-./cmd/${NAME}-svc}"

GOOS="${GOOS:-darwin}"
GOARCH="${GOARCH:-arm64}"
CHANNEL="${CHANNEL:-stable}"
DOCK_BASE="${DOCK_BASE:-http://127.0.0.1:8080}"

FILE="${NAME}_${GOOS}_${GOARCH}_${VERSION}"
[ "$GOOS" = "windows" ] && FILE="${FILE}.exe"

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
out="${workdir}/${FILE}"

echo "==> building ${NAME} ${VERSION} (${GOOS}/${GOARCH}) from ${SRC_DIR}/${MAIN_PKG}"
( cd "$SRC_DIR" && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags "-X main.version=${VERSION}" -o "$out" "$MAIN_PKG" )

# sha256 (portable: shasum on macOS, sha256sum on Linux).
if command -v sha256sum >/dev/null 2>&1; then
  SHA="$(sha256sum "$out" | awk '{print $1}')"
else
  SHA="$(shasum -a 256 "$out" | awk '{print $1}')"
fi
echo "==> sha256 ${SHA}"

# Stage the binary where dock serves it: <dist>/<name>/<file>.
if [ -n "${PLUGIN_DIST_DIR:-}" ]; then
  dest_dir="${PLUGIN_DIST_DIR}/${NAME}"
  mkdir -p "$dest_dir"
  cp "$out" "${dest_dir}/${FILE}"
  echo "==> staged ${dest_dir}/${FILE}"
fi
if [ -n "${RSYNC_DEST:-}" ]; then
  # RSYNC_DEST is the dist root on the remote; module subdir is created there.
  rsync -av --mkpath "$out" "${RSYNC_DEST%/}/${NAME}/${FILE}" 2>/dev/null \
    || rsync -av "$out" "${RSYNC_DEST%/}/${NAME}/${FILE}"
  echo "==> rsynced to ${RSYNC_DEST%/}/${NAME}/${FILE}"
fi
if [ -z "${PLUGIN_DIST_DIR:-}" ] && [ -z "${RSYNC_DEST:-}" ]; then
  echo "!! neither PLUGIN_DIST_DIR nor RSYNC_DEST set — binary not staged; dock can't serve it" >&2
  exit 2
fi

# Register the manifest row. dist_rel_path is the file name relative to
# <dist>/<name>/ (the binary handler joins them).
if [ -z "${DOCK_ADMIN_TOKEN:-}" ]; then
  echo "!! DOCK_ADMIN_TOKEN unset — skipping manifest POST. Binary is staged; register it with:" >&2
  echo "   curl -X POST ${DOCK_BASE}/api/admin/plugin-modules/${NAME}/releases -H 'Authorization: Bearer <tok>' \\" >&2
  echo "        -d '{\"version\":\"${VERSION}\",\"os\":\"${GOOS}\",\"arch\":\"${GOARCH}\",\"sha256\":\"${SHA}\",\"dist_rel_path\":\"${FILE}\"}'" >&2
  exit 3
fi

echo "==> POST release manifest to ${DOCK_BASE}"
curl -fsS -X POST "${DOCK_BASE}/api/admin/plugin-modules/${NAME}/releases" \
  -H "Authorization: Bearer ${DOCK_ADMIN_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"version\":\"${VERSION}\",\"os\":\"${GOOS}\",\"arch\":\"${GOARCH}\",\"sha256\":\"${SHA}\",\"dist_rel_path\":\"${FILE}\",\"channel\":\"${CHANNEL}\"}"
echo
echo "==> published ${NAME}@${VERSION}. Pin the roll with: POST /api/admin/plugin-modules/${NAME}/update {\"version\":\"${VERSION}\"}"
