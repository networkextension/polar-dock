package dock

// Zone bootstrap delivery — the thin dock hook for the Polar Zone Release System
// (see networkextension/polar-zone). A bare box runs:
//
//	curl -fsSL https://<seed>/api/zone/install.sh | sh -s -- --channel stable
//
// which fetches the polar-zone binary from /api/zone/binary and runs
// `polar-zone bootstrap` against this dock's polar-release. Both endpoints are
// public by design (same posture as /api/agent/binary): the binary is
// open-source and every artifact it pulls is ed25519-verified before install.
//
// Binaries are served from a prebuilt dir only ($POLAR_ZONE_DIST_DIR or
// workDir/dist/zone/, naming "polar-zone_<os>_<arch>") — polar-zone is its own
// repo, so there's no in-tree source to JIT-build.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

var zoneReleaseProxyClient = &http.Client{Timeout: 10 * time.Second}

// handleReleaseResolveProxy publicly fronts polar-release's read-only endpoints
// (/release/resolve, /release/pubkey) — which are designed to be public but on a
// loopback-only release svc. This makes dock the single public seed origin for
// `polar-zone bootstrap`: it gets the ed25519-signed manifest + signed asset
// dl_url here, fetches the (already-public) bytes from the provider, and verifies
// the chain itself. Read-only; writes (publish/finalize) stay internal/admin.
func (s *Server) handleReleaseResolveProxy(c *gin.Context) {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("POLAR_RELEASE_BASE")), "/")
	if base == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "release registry not configured (POLAR_RELEASE_BASE unset)"})
		return
	}
	upstream := base + c.Request.URL.Path // /release/resolve or /release/pubkey
	if q := c.Request.URL.RawQuery; q != "" {
		upstream += "?" + q
	}
	resp, err := zoneReleaseProxyClient.Get(upstream)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "release upstream: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	c.Data(resp.StatusCode, ct, body)
}

func (s *Server) handleZoneBinary(c *gin.Context) {
	osName := strings.TrimSpace(strings.ToLower(c.Query("os")))
	arch := strings.TrimSpace(strings.ToLower(c.Query("arch")))
	if osName == "" || arch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "os and arch query params required"})
		return
	}
	if !isAllowedAgentTarget(osName, arch) { // same target matrix as agents
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported os/arch: " + osName + "/" + arch})
		return
	}
	path, ok := s.findPrebuiltZoneBinary(osName, arch)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "polar-zone binary unavailable: stage prebuilt polar-zone_<os>_<arch> in " +
				"$POLAR_ZONE_DIST_DIR or <workDir>/dist/zone/, or fetch from " +
				"https://github.com/networkextension/polar-zone/releases",
		})
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename=\"polar-zone_"+osName+"_"+arch+"\"")
	c.File(path)
}

func (s *Server) findPrebuiltZoneBinary(osName, arch string) (string, bool) {
	candidates := []string{}
	if d := strings.TrimSpace(os.Getenv("POLAR_ZONE_DIST_DIR")); d != "" {
		candidates = append(candidates, d)
	}
	if s.workDir != "" {
		candidates = append(candidates, filepath.Join(s.workDir, "dist", "zone"))
	}
	name := "polar-zone_" + osName + "_" + arch
	for _, dir := range candidates {
		full := filepath.Join(dir, name)
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full, true
		}
	}
	return "", false
}

// handleZoneInstall returns a POSIX-sh shim: detect platform → download +
// chmod the polar-zone binary from this dock → run `bootstrap` against it.
// `?channel=` selects the release channel (default stable).
func (s *Server) handleZoneInstall(c *gin.Context) {
	channel := strings.TrimSpace(c.Query("channel"))
	if channel != "beta" && channel != "dev" {
		channel = "stable"
	}
	base := serverPublicURL(c)
	script := `#!/bin/sh
# polar-zone bootstrap installer — reproduces a Polar zone from a signed bundle.
set -eu
SEED="` + base + `"
CHANNEL="` + channel + `"
PREFIX="${PREFIX:-$HOME/.local/bin}"
for a in "$@"; do case "$a" in --channel=*) CHANNEL="${a#*=}";; --prefix=*) PREFIX="${a#*=}";; esac; done

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
A=$(uname -m); case "$A" in x86_64|amd64) A=amd64;; arm64|aarch64) A=arm64;; esac
mkdir -p "$PREFIX"
echo "→ fetching polar-zone ($OS/$A) from $SEED"
curl -fsSL "$SEED/api/zone/binary?os=$OS&arch=$A" -o "$PREFIX/polar-zone"
chmod +x "$PREFIX/polar-zone"
echo "→ polar-zone bootstrap --server $SEED --channel $CHANNEL"
exec "$PREFIX/polar-zone" bootstrap --server "$SEED" --channel "$CHANNEL" --prefix "$PREFIX"
`
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.String(http.StatusOK, script)
}
