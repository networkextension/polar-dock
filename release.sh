#!/usr/bin/env bash
set -euo pipefail

# Release packaging — builds dock for one target + polar-agent for
# every supported platform (operators may run agents on different
# OS/arch than the dock host). Result is a single tarball:
#
#   polar-<version>-<dock-os>-<dock-arch>.tar.gz
#     ├── dock_<arch>_<os>                 # server binary (this OS only)
#     ├── ui/                              # built static + express server
#     ├── scripts/                         # eval_start.sh, db_init.sql, ...
#     ├── doc/                             # eval-quickstart, deploy-local
#     ├── agents/
#     │   ├── polar-agent_darwin_arm64
#     │   ├── polar-agent_darwin_amd64
#     │   ├── polar-agent_linux_amd64
#     │   ├── polar-agent_linux_arm64
#     │   ├── polar-agent_windows_amd64.exe
#     │   ├── tools.example.json
#     │   └── README.md                    # picker guide
#     ├── QUICKSTART.md
#     └── LICENSE / README.md
#
# Usage:
#   ./release.sh                              # auto version + default dock target
#   ./release.sh v1.2.3                       # explicit version
#   TARGET=linux/amd64 ./release.sh v1.2.3
#   ./release.sh v1.2.3 --upload              # gh release create + upload
#   AGENT_TARGETS_OVERRIDE="darwin/arm64 linux/amd64" ./release.sh   # subset
#
# Override AGENT_TARGETS_OVERRIDE (space-separated os/arch) to skip
# unwanted platforms — useful when iterating locally and you don't
# need all 5 builds.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCK_PATH="./cmd/dock"
AGENT_PATH="./cmd/polar-agent"
OUT_DIR="dist"
DEFAULT_TARGET="freebsd/amd64"
TARGET="${TARGET:-$DEFAULT_TARGET}"
VERSION="${1:-}"
UPLOAD=false

# Default agent targets. Mac arm64 first since that's the most
# common dev box; Windows last since it's the rarest in this
# project. Customize via AGENT_TARGETS_OVERRIDE env var.
DEFAULT_AGENT_TARGETS=(
  "darwin/arm64"
  "darwin/amd64"
  "linux/amd64"
  "linux/arm64"
  "freebsd/amd64"
  "windows/amd64"
)
if [ -n "${AGENT_TARGETS_OVERRIDE:-}" ]; then
  read -r -a AGENT_TARGETS <<< "${AGENT_TARGETS_OVERRIDE}"
else
  AGENT_TARGETS=("${DEFAULT_AGENT_TARGETS[@]}")
fi

# parse remaining flags
for arg in "${@:2}"; do
  case "${arg}" in
    --upload) UPLOAD=true ;;
    --help|-h)
      echo "Usage: $0 [version] [--upload]"
      echo "  TARGET=os/arch                       override dock build target (default: ${DEFAULT_TARGET})"
      echo "  AGENT_TARGETS_OVERRIDE='os/arch ...'  pick a subset of agent platforms"
      exit 0
      ;;
    *) ;;
  esac
done

if [ -z "${VERSION}" ]; then
  if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    if tag=$(git describe --tags --abbrev=0 2>/dev/null || true); then
      VERSION="${tag:-$(git rev-parse --short HEAD)}"
    else
      VERSION="$(git rev-parse --short HEAD)"
    fi
  else
    VERSION="local-$(date -u +%Y%m%d%H%M%S)"
  fi
fi

OS="${TARGET%%/*}"
ARCH="${TARGET##*/}"

DOCK_BIN_NAME="dock_${ARCH}_${OS}"

echo "Release: version=${VERSION}"
echo "  dock target  : ${OS}/${ARCH} → ${DOCK_BIN_NAME}"
echo "  agent targets: ${AGENT_TARGETS[*]}"
echo

# ---- build dock --------------------------------------------------
pushd "$REPO_ROOT" >/dev/null
echo "Building dock for ${OS}/${ARCH}..."
CGO_ENABLED=0 GOOS="${OS}" GOARCH="${ARCH}" go build -o "${DOCK_BIN_NAME}" "${DOCK_PATH}"
popd >/dev/null

# ---- build UI ----------------------------------------------------
if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1; then
  pushd "${REPO_ROOT}/ui" >/dev/null
  npm install --silent
  npm run build
  popd >/dev/null
fi

# ---- prepare package layout --------------------------------------
PKG_DIR="${OUT_DIR}/${VERSION}/${OS}_${ARCH}"
rm -rf "${PKG_DIR}"
mkdir -p "${PKG_DIR}/agents"
mv "${REPO_ROOT}/${DOCK_BIN_NAME}" "${PKG_DIR}/"

# ---- build polar-agent for every target --------------------------
echo
echo "Building polar-agent for ${#AGENT_TARGETS[@]} platforms..."
for T in "${AGENT_TARGETS[@]}"; do
  AOS="${T%%/*}"
  AARCH="${T##*/}"
  AEXT=""
  [ "$AOS" = "windows" ] && AEXT=".exe"
  AGENT_NAME="polar-agent_${AOS}_${AARCH}${AEXT}"
  echo "  → ${AGENT_NAME}"
  pushd "$REPO_ROOT" >/dev/null
  CGO_ENABLED=0 GOOS="${AOS}" GOARCH="${AARCH}" \
    go build -o "${PKG_DIR}/agents/${AGENT_NAME}" "${AGENT_PATH}"
  popd >/dev/null
done

# Ship the tools.json template alongside the agent binaries so
# operators have a copy-paste-ready customization starter.
if [ -f "${REPO_ROOT}/cmd/polar-agent/tools.example.json" ]; then
  cp "${REPO_ROOT}/cmd/polar-agent/tools.example.json" "${PKG_DIR}/agents/tools.example.json"
fi

# Service unit templates, start/stop wrappers, install guide,
# config example. Live in agents/dist/ so the picker README at
# agents/README.md stays the first thing operators read; service
# install instructions live one level deeper.
if [ -d "${REPO_ROOT}/release/agent" ]; then
  mkdir -p "${PKG_DIR}/agents/dist"
  cp "${REPO_ROOT}/release/agent/agent.toml.example"      "${PKG_DIR}/agents/dist/agent.toml.example"
  cp "${REPO_ROOT}/release/agent/start.sh"                "${PKG_DIR}/agents/dist/start.sh"
  cp "${REPO_ROOT}/release/agent/stop.sh"                 "${PKG_DIR}/agents/dist/stop.sh"
  cp "${REPO_ROOT}/release/agent/polar-agent.macos.plist" "${PKG_DIR}/agents/dist/polar-agent.macos.plist"
  cp "${REPO_ROOT}/release/agent/polar-agent.rc"          "${PKG_DIR}/agents/dist/polar-agent.rc"
  cp "${REPO_ROOT}/release/agent/INSTALL.md"              "${PKG_DIR}/agents/dist/INSTALL.md"
  chmod +x "${PKG_DIR}/agents/dist/start.sh" "${PKG_DIR}/agents/dist/stop.sh" "${PKG_DIR}/agents/dist/polar-agent.rc"
fi

# Drop a tiny picker guide in agents/ so the operator knows which
# binary their machine wants — saves a "what's the difference"
# email from every recipient.
cat > "${PKG_DIR}/agents/README.md" <<'AGENTS_README'
# polar-agent binaries

Pick the binary matching your machine, copy it somewhere on PATH
(e.g. `~/.local/bin/polar-agent`), and run `polar-agent help`.

| File                              | OS / arch                |
|-----------------------------------|--------------------------|
| `polar-agent_darwin_arm64`        | macOS Apple Silicon (M1+)|
| `polar-agent_darwin_amd64`        | macOS Intel              |
| `polar-agent_linux_amd64`         | Linux x86_64             |
| `polar-agent_linux_arm64`         | Linux ARM (Raspberry Pi 4+, AWS Graviton, Azure Ampere) |
| `polar-agent_freebsd_amd64`       | FreeBSD x86_64           |
| `polar-agent_windows_amd64.exe`   | Windows x86_64           |

Quick install (Unix):

```bash
# macOS Apple Silicon example
mkdir -p ~/.local/bin
cp agents/polar-agent_darwin_arm64 ~/.local/bin/polar-agent
chmod +x ~/.local/bin/polar-agent

# verify
polar-agent help
```

Then `polar-agent login --server=<dock URL> --token=<token from
/api/agent/tokens>` once, and `polar-agent attach --bot=<id>
--workdir=<path> --tool=<kimi|claude|codex>` to start a passthrough
session. See `tools.example.json` for adding custom tools or
per-tool env (proxy, API keys, etc.).

## Running as a service

The interactive `attach` form is fine for testing, but for daily use
you'll want it auto-started. See `dist/INSTALL.md` for OS-specific
recipes:

- **macOS** — `dist/polar-agent.macos.plist` (launchd LaunchAgent)
- **FreeBSD** — `dist/polar-agent.rc` (rc.d service)
- **Generic / Linux / Windows** — `dist/start.sh` + `dist/stop.sh`
  helpers (nohup-style background runner with pidfile)

Plus `dist/agent.toml.example` for headless config drop-in.
AGENTS_README

# ---- top-level docs / license -----------------------------------
for f in LICENSE README.md; do
  if [ -f "${REPO_ROOT}/${f}" ]; then
    cp "${REPO_ROOT}/${f}" "${PKG_DIR}/"
  fi
done

# ---- UI dist + express server -----------------------------------
if [ -d "${REPO_ROOT}/ui/dist" ]; then
  mkdir -p "${PKG_DIR}/ui"
  cp -r "${REPO_ROOT}/ui/dist" "${PKG_DIR}/ui/dist"
  cp "${REPO_ROOT}/ui/server.js" "${PKG_DIR}/ui/server.js"
  cp "${REPO_ROOT}/ui/package.json" "${PKG_DIR}/ui/package.json"
  if [ -f "${REPO_ROOT}/ui/package-lock.json" ]; then
    cp "${REPO_ROOT}/ui/package-lock.json" "${PKG_DIR}/ui/package-lock.json"
  fi
fi

# ---- scripts ----------------------------------------------------
mkdir -p "${PKG_DIR}/scripts"
for f in db_init.sql eval_start.sh migrate_db_to_ideamesh.sh start_agents.sh start_agents.env.example; do
  if [ -f "${REPO_ROOT}/scripts/${f}" ]; then
    cp "${REPO_ROOT}/scripts/${f}" "${PKG_DIR}/scripts/${f}"
    case "$f" in *.sh) chmod +x "${PKG_DIR}/scripts/${f}" ;; esac
  fi
done

# ---- docs (eval subset) ------------------------------------------
mkdir -p "${PKG_DIR}/doc"
for f in eval-quickstart.md deploy-local.md; do
  if [ -f "${REPO_ROOT}/doc/${f}" ]; then
    cp "${REPO_ROOT}/doc/${f}" "${PKG_DIR}/doc/${f}"
  fi
done

# Sales-friendly QUICKSTART at the package root.
if [ -f "${PKG_DIR}/doc/eval-quickstart.md" ]; then
  cp "${PKG_DIR}/doc/eval-quickstart.md" "${PKG_DIR}/QUICKSTART.md"
fi

# ---- tarball + checksum -----------------------------------------
TAR_NAME="polar-${VERSION}-${OS}-${ARCH}.tar.gz"
pushd "${OUT_DIR}/${VERSION}" >/dev/null
tar -czf "${TAR_NAME}" "${OS}_${ARCH}"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "${TAR_NAME}" > "${TAR_NAME}.sha256"
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "${TAR_NAME}" > "${TAR_NAME}.sha256"
fi
popd >/dev/null

echo
echo "Packaged: ${OUT_DIR}/${VERSION}/${TAR_NAME}"
if [ -f "${OUT_DIR}/${VERSION}/${TAR_NAME}.sha256" ]; then
  echo "Checksum: ${OUT_DIR}/${VERSION}/${TAR_NAME}.sha256"
fi

# ---- optional gh release upload ---------------------------------
if [ "$UPLOAD" = true ]; then
  if ! command -v gh >/dev/null 2>&1; then
    echo "Error: gh CLI not found. Install and authenticate (gh auth login) to upload." >&2
    exit 1
  fi
  GITHUB_REPO="${GITHUB_REPOSITORY:-}"
  if [ -z "${GITHUB_REPO}" ]; then
    echo "GITHUB_REPOSITORY environment variable not set. Set it to 'owner/repo' to upload." >&2
    exit 1
  fi
  RELEASE_NAME="${VERSION}"
  ASSET_PATH="${OUT_DIR}/${VERSION}/${TAR_NAME}"
  echo "Creating/updating release ${RELEASE_NAME} in ${GITHUB_REPO} and uploading ${ASSET_PATH}..."
  gh release create "${RELEASE_NAME}" "${ASSET_PATH}" \
    --repo "${GITHUB_REPO}" \
    --title "${RELEASE_NAME}" \
    --notes "Release ${RELEASE_NAME}"
  echo "Upload finished."
fi

echo "Done."
