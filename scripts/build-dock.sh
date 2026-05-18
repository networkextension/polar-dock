#!/usr/bin/env bash
# Build polar-dock with git/build metadata baked in via -ldflags.
# The injected vars are read by sysinfo.go (buildGitSHA / buildGitBranch /
# buildGitDirty / buildBuildTime) and surfaced on /api/system/info.
#
# Usage:
#   scripts/build-dock.sh                                  # native build
#   GOOS=darwin GOARCH=arm64 scripts/build-dock.sh         # cross-compile
#   OUT=/tmp/polar-dock scripts/build-dock.sh              # custom output
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

OUT="${OUT:-./polar-dock}"

GIT_SHA=$(git rev-parse --short HEAD 2>/dev/null || echo "")
GIT_BRANCH=$(git symbolic-ref --short -q HEAD 2>/dev/null || echo "detached")
# Dirty if there are uncommitted changes OR untracked files in tracked dirs.
if [ -z "$(git status --porcelain 2>/dev/null)" ]; then
  GIT_DIRTY="false"
else
  GIT_DIRTY="true"
fi
BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS="-w -s"
LDFLAGS="$LDFLAGS -X gin-auth-app/internal/app/dock.buildGitSHA=$GIT_SHA"
LDFLAGS="$LDFLAGS -X gin-auth-app/internal/app/dock.buildGitBranch=$GIT_BRANCH"
LDFLAGS="$LDFLAGS -X gin-auth-app/internal/app/dock.buildGitDirty=$GIT_DIRTY"
LDFLAGS="$LDFLAGS -X gin-auth-app/internal/app/dock.buildBuildTime=$BUILD_TIME"

echo "build polar-dock @ ${GIT_BRANCH}/${GIT_SHA}${GIT_DIRTY:+ ($([ "$GIT_DIRTY" = "true" ] && echo dirty || echo clean))}"
echo "  -> $OUT"
# Polar dock has no cgo dependencies (lib/pq + gin are pure-Go). Pin
# CGO_ENABLED=0 so the build doesn't require a matching macOS SDK
# version — Go 1.26 wants MacOSX26.sdk for runtime/cgo and the build
# errors out without it even though no Go code calls into C. Static
# binary is also nicer for cross-deploy.
CGO_ENABLED=0 go build -ldflags "$LDFLAGS" -o "$OUT" ./cmd/dock
ls -lh "$OUT"
