package dock

// plugin_binary_handler.go — Track 3 (module OTA). Serves a published
// module binary for self-update.
//
//   GET /api/plugin/:name/binary?version=&os=&arch=
//
// Modeled on agent_binary_handler.go: streams a prebuilt file from a dist
// dir, no JIT build (modules are published via the publish endpoint, not
// cross-compiled on demand). The file lives at
// <dist>/<name>/<dist_rel_path> where <dist> is POLAR_PLUGIN_DIST_DIR (or
// workDir/dist/plugins). The plugin_releases row is the manifest: it pins
// the exact dist_rel_path + sha256, which we re-verify before serving so a
// truncated/corrupt rsync can't ship a bad binary.
//
// No auth, by the same reasoning as the agent binary: the binary isn't
// secret (it's the open-source module), and the SDK fetches it with a plain
// GET off the heartbeat directive. The directive only points here for a
// version dock itself chose to publish.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handlePluginBinary(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	version := strings.TrimSpace(c.Query("version"))
	osName := strings.ToLower(strings.TrimSpace(c.Query("os")))
	arch := strings.ToLower(strings.TrimSpace(c.Query("arch")))
	if name == "" || version == "" || osName == "" || arch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name, version, os, arch required"})
		return
	}

	rel, err := s.getPluginRelease(name, version, osName, arch)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_releases.lookup_failed"})
		return
	}
	if rel == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such release: " + name + "@" + version + " " + osName + "/" + arch})
		return
	}

	path, ok := s.resolvePluginBinaryPath(name, rel.DistRelPath)
	if !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "binary file missing for " + name + "@" + version +
				"; set POLAR_PLUGIN_DIST_DIR to the dir holding <name>/<file> and rsync the build in",
		})
		return
	}

	// Re-verify sha256 against the manifest before serving — a corrupt or
	// half-synced file must not reach a self-updating module.
	if sum, err := sha256File(path); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hash failed: " + err.Error()})
		return
	} else if sum != rel.SHA256 {
		c.JSON(http.StatusConflict, gin.H{"error": "on-disk binary sha256 != manifest; re-publish or re-sync"})
		return
	}

	filename := name + "_" + osName + "_" + arch
	if osName == "windows" {
		filename += ".exe"
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("X-Polar-Plugin-Version", rel.Version)
	c.Header("X-Polar-Plugin-SHA256", rel.SHA256)
	c.File(path)
}

// resolvePluginBinaryPath locates <dist>/<name>/<distRel> across the dist
// dir candidates, guarding against path traversal in distRel. Returns
// ("", false) if no readable file is found.
func (s *Server) resolvePluginBinaryPath(name, distRel string) (string, bool) {
	// distRel is operator-supplied (publish payload); never let it escape
	// the module's own dir.
	clean := filepath.Clean("/" + distRel) // force-absolute then strip leading sep
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || strings.HasPrefix(clean, "..") {
		return "", false
	}

	bases := []string{}
	if d := strings.TrimSpace(os.Getenv("POLAR_PLUGIN_DIST_DIR")); d != "" {
		bases = append(bases, d)
	}
	if s.workDir != "" {
		bases = append(bases, filepath.Join(s.workDir, "dist", "plugins"))
	}
	for _, base := range bases {
		full := filepath.Join(base, name, clean)
		// Confirm the resolved path stays under base/name.
		if rel, err := filepath.Rel(filepath.Join(base, name), full); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if info, err := os.Stat(full); err == nil && !info.IsDir() {
			return full, true
		}
	}
	return "", false
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
