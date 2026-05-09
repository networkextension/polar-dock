package dock

// GET /api/agent/binary?os=darwin&arch=arm64 — serves a polar-agent
// binary for the requested platform. Two source paths in priority
// order:
//
//  1. Pre-built dir: $POLAR_AGENT_DIST_DIR or workDir/dist/agents/
//     Looks for "polar-agent_<os>_<arch>" (matching release.sh naming).
//     Used in production where release.sh has been run.
//
//  2. JIT cross-compile: if cmd/polar-agent source is on disk + go
//     toolchain is available, run `go build` with GOOS/GOARCH for
//     the requested platform. Output cached under
//     $TMPDIR/polar-agent-build-cache/<os>_<arch>/polar-agent so
//     subsequent fetches don't rebuild.
//
// No auth on this endpoint by design — install script needs to
// fetch BEFORE the user has a working `polar-agent login` (the
// raw bearer in the script is what authorizes the actual agent
// connection later). The binary itself isn't sensitive (open-source);
// only the token paired with it grants access. Rate-limit upstream
// at the load balancer if abuse becomes a concern.

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAgentBinary(c *gin.Context) {
	osName := strings.TrimSpace(strings.ToLower(c.Query("os")))
	arch := strings.TrimSpace(strings.ToLower(c.Query("arch")))
	if osName == "" || arch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "os and arch query params required"})
		return
	}
	if !isAllowedAgentTarget(osName, arch) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported os/arch combination: " + osName + "/" + arch})
		return
	}

	// Path 1: prebuilt dir.
	if path, ok := s.findPrebuiltAgentBinary(osName, arch); ok {
		serveAgentBinary(c, path, osName, arch)
		return
	}

	// Path 2: JIT cross-compile if source + toolchain available.
	path, err := s.buildAgentBinary(osName, arch)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "polar-agent binary unavailable: " + err.Error() +
				". Configure POLAR_AGENT_DIST_DIR pointing at release.sh's dist/agents/, or run dock from the source repo so JIT cross-compile works.",
		})
		return
	}
	serveAgentBinary(c, path, osName, arch)
}

func isAllowedAgentTarget(os, arch string) bool {
	switch os {
	case "darwin":
		return arch == "amd64" || arch == "arm64"
	case "linux":
		return arch == "amd64" || arch == "arm64"
	case "windows":
		return arch == "amd64"
	}
	return false
}

// findPrebuiltAgentBinary searches dist directories for a binary
// matching release.sh's naming. Returns ("", false) if no match.
func (s *Server) findPrebuiltAgentBinary(osName, arch string) (string, bool) {
	candidates := []string{}
	if d := strings.TrimSpace(os.Getenv("POLAR_AGENT_DIST_DIR")); d != "" {
		candidates = append(candidates, d)
	}
	if s.workDir != "" {
		candidates = append(candidates, filepath.Join(s.workDir, "dist", "agents"))
		candidates = append(candidates, filepath.Join(s.workDir, "agents"))
	}
	binaryName := "polar-agent_" + osName + "_" + arch
	if osName == "windows" {
		binaryName += ".exe"
	}
	for _, dir := range candidates {
		full := filepath.Join(dir, binaryName)
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full, true
		}
	}
	return "", false
}

// buildAgentBinary cross-compiles cmd/polar-agent for the requested
// target. Cached by (os, arch); subsequent calls reuse the artifact
// unless the source changes. Returns an error if go isn't on PATH
// or cmd/polar-agent doesn't exist (e.g. dock running from a
// stripped binary distribution).
func (s *Server) buildAgentBinary(osName, arch string) (string, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("go toolchain not on PATH for JIT build")
	}
	srcDir := filepath.Join(s.workDir, "cmd", "polar-agent")
	if info, err := os.Stat(srcDir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("cmd/polar-agent source not found at %s", srcDir)
	}
	cacheRoot := filepath.Join(os.TempDir(), "polar-agent-build-cache")
	cacheDir := filepath.Join(cacheRoot, osName+"_"+arch)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}
	out := filepath.Join(cacheDir, "polar-agent")
	if osName == "windows" {
		out += ".exe"
	}

	// Cache invalidation: rebuild if any .go file under cmd/polar-agent
	// is newer than the cached binary. Keep it simple — single mtime
	// comparison covers the common case (operator runs `git pull`
	// then a teammate hits the install endpoint).
	if needsRebuild, err := agentCacheNeedsRebuild(out, srcDir); err == nil && !needsRebuild {
		return out, nil
	}

	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./cmd/polar-agent")
	cmd.Dir = s.workDir
	cmd.Env = append(os.Environ(),
		"GOOS="+osName,
		"GOARCH="+arch,
		"CGO_ENABLED=0",
	)
	if combined, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s/%s: %v: %s", osName, arch, err, strings.TrimSpace(string(combined)))
	}
	return out, nil
}

func agentCacheNeedsRebuild(out, srcDir string) (bool, error) {
	outStat, err := os.Stat(out)
	if err != nil {
		return true, nil
	}
	rebuild := false
	walkErr := filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		if info.ModTime().After(outStat.ModTime()) {
			rebuild = true
		}
		return nil
	})
	return rebuild, walkErr
}

func serveAgentBinary(c *gin.Context, path, osName, arch string) {
	filename := "polar-agent_" + osName + "_" + arch
	if osName == "windows" {
		filename += ".exe"
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.File(path)
}
