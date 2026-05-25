package dock

// skill_install_handlers.go — P1a-dock: REST trigger for
// dock-initiated bundle install/uninstall via WS to the agent.
//
//   POST /api/hosts/:host_id/skills/install    body={catalog_id: "scat_..."}
//   POST /api/hosts/:host_id/skills/uninstall  body={publisher,kind,version,force?}
//
// Both endpoints are admin-gated (per host workspace ownership) and
// fire-and-forget: agent reports via skill.install.result on its WS
// channel; the next skill.advertise tick confirms the new state in
// polar-hosts.host_skills.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type skillInstallRequest struct {
	CatalogID string `json:"catalog_id"`
}

type skillUninstallRequest struct {
	Publisher string `json:"publisher"`
	SkillKind string `json:"skill_kind"`
	Version   string `json:"version"`
	Force     bool   `json:"force"`
}

// handleHostSkillInstall — admin asks dock to install a catalog
// bundle on a specific host. dock fetches the catalog row from
// polar-hosts, then frames a skill.install for the host's agent.
func (s *Server) handleHostSkillInstall(c *gin.Context) {
	if !isAdmin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
		return
	}
	hostID := strings.TrimSpace(c.Param("host_id"))
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id required"})
		return
	}
	var req skillInstallRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	req.CatalogID = strings.TrimSpace(req.CatalogID)
	if req.CatalogID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "catalog_id required"})
		return
	}

	// P3: visibility is enforced by polar-hosts when we pass the
	// operator's session workspace_id as a query param. A 404 from the
	// catalog endpoint may mean "doesn't exist" OR "exists but belongs
	// to another workspace" — surface as the same 404 to avoid leaking
	// the existence of private bundles from other workspaces.
	workspaceID := strings.TrimSpace(c.GetString("workspace_id"))
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "active workspace required"})
		return
	}
	entry, err := s.fetchCatalogEntry(req.CatalogID, workspaceID)
	if err != nil {
		if errors.Is(err, errCatalogNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "catalog entry not found or not visible to this workspace"})
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "polar-hosts catalog lookup failed: " + err.Error()})
		return
	}

	conn := s.agentHub.lookupByHostID(hostID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":   "agent not attached for host",
			"host_id": hostID,
		})
		return
	}

	installID := "ins_" + generateResourceID()
	frame := map[string]any{
		"kind":             "skill.install",
		"install_id":       installID,
		"publisher":        entry.Publisher,
		"skill_kind":       entry.SkillKind,
		"version":          entry.Version,
		"sha256":           entry.SHA256,
		"download_url":     entry.DownloadURL,
		"size_bytes":       entry.SizeBytes,
		"manifest_preview": entry.ManifestJSON,
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal: " + err.Error()})
		return
	}
	select {
	case conn.send <- payload:
		log.Printf("[skill.install] sent host=%s catalog=%s id=%s", hostID, req.CatalogID, installID)
	case <-conn.close:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
		return
	case <-time.After(5 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "agent send timeout"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"install_id":   installID,
		"host_id":      hostID,
		"catalog_id":   req.CatalogID,
		"sent_at":      time.Now().UTC(),
		"hint":         "result lands via WS skill.install.result + the next skill.advertise tick (~60s) reflects in host_skills",
	})
}

// handleHostSkillUninstall — admin asks agent to remove an installed
// bundle. Frames a skill.uninstall on the agent's WS.
func (s *Server) handleHostSkillUninstall(c *gin.Context) {
	if !isAdmin(c) {
		c.JSON(http.StatusForbidden, gin.H{"error": "admin only"})
		return
	}
	hostID := strings.TrimSpace(c.Param("host_id"))
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "host_id required"})
		return
	}
	var req skillUninstallRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json: " + err.Error()})
		return
	}
	req.Publisher = strings.TrimSpace(req.Publisher)
	req.SkillKind = strings.TrimSpace(req.SkillKind)
	req.Version = strings.TrimSpace(req.Version)
	if req.Publisher == "" || req.SkillKind == "" || req.Version == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "publisher, skill_kind, version all required"})
		return
	}
	conn := s.agentHub.lookupByHostID(hostID)
	if conn == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent not attached for host"})
		return
	}
	uninstallID := "uni_" + generateResourceID()
	frame := map[string]any{
		"kind":       "skill.uninstall",
		"install_id": uninstallID,
		"publisher":  req.Publisher,
		"skill_kind": req.SkillKind,
		"version":    req.Version,
		"force":      req.Force,
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal: " + err.Error()})
		return
	}
	select {
	case conn.send <- payload:
		log.Printf("[skill.uninstall] sent host=%s %s/%s@%s id=%s force=%v", hostID, req.Publisher, req.SkillKind, req.Version, uninstallID, req.Force)
	case <-conn.close:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent disconnected"})
		return
	case <-time.After(5 * time.Second):
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "agent send timeout"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"uninstall_id": uninstallID,
		"host_id":      hostID,
		"sent_at":      time.Now().UTC(),
	})
}

// --- catalog fetch shim ---

// fetchCatalogEntry resolves a catalog ID to its full row by hitting
// polar-hosts /api/skill-catalog/:id. polar-hosts owns the catalog;
// dock only needs the bundle details to populate the skill.install
// frame. Uses the operator's existing session cookie via internal
// hop URL (loopback to plugin svc).
type catalogEntry struct {
	ID           string          `json:"id"`
	Publisher    string          `json:"publisher"`
	SkillKind    string          `json:"skill_kind"`
	Version      string          `json:"version"`
	SHA256       string          `json:"sha256"`
	DownloadURL  string          `json:"download_url"`
	SizeBytes    int64           `json:"size_bytes"`
	ManifestJSON json.RawMessage `json:"manifest_json"`
}

var errCatalogNotFound = errors.New("catalog entry not found")

func (s *Server) fetchCatalogEntry(id, workspaceID string) (*catalogEntry, error) {
	base := strings.TrimSpace(s.polarHostsTouchURL)
	if base == "" {
		return nil, errors.New("POLAR_HOSTS_TOUCH_URL not configured")
	}
	// touchURL is /internal/v1/hosts/touch — strip the path to get
	// the plugin svc origin. We don't have a dedicated catalog config
	// var; piggy-back on the existing one (same host:port).
	idx := strings.Index(base, "/internal/")
	origin := base
	if idx > 0 {
		origin = base[:idx]
	}
	// P3: use the loopback-trusted internal endpoint that takes
	// workspace_id as a query param and applies the same visibility
	// rule as the auth-gated /api/skill-catalog/:id.
	url := fmt.Sprintf("%s/internal/v1/skill-catalog/%s?workspace_id=%s",
		origin, id, workspaceID)
	resp, err := http.Get(url) //nolint:gosec // operator-controlled host
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errCatalogNotFound
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("catalog GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var envelope struct {
		Item catalogEntry `json:"item"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode catalog response: %w", err)
	}
	if envelope.Item.ID == "" {
		return nil, errCatalogNotFound
	}
	return &envelope.Item, nil
}

func isAdmin(c *gin.Context) bool {
	role, _ := c.Get("role")
	if r, ok := role.(string); ok {
		switch strings.ToLower(strings.TrimSpace(r)) {
		case "admin", "owner", "platform-admin":
			return true
		}
	}
	return false
}
