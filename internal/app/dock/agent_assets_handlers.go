package dock

// agent_assets_handlers.go — let a headless compute agent (polar_agent_ token)
// store an output blob in polar-assets and get a download URL, by having dock
// broker the grant. Agents can't call polar-assets directly (it validates
// session/polar_ tokens, not agent tokens), and only dock holds the provider
// HMAC signing key. Mirrors the internal grant/finalize/download path
// (asset_internal_handlers.go) but under PolarAgentAuthMiddleware, scoped to the
// token owner's personal workspace. See polar-task T2c.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type agentAssetGrantRequest struct {
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	Mime      string `json:"mime"`
	Name      string `json:"name"` // optional logical name; content-addressed by sha256 if empty
}

// POST /api/agent/assets/grant — content-addressed upload grant for a task artifact.
func (s *Server) handleAgentAssetGrant(c *gin.Context) {
	userIDVal, _ := c.Get("agent_user_id")
	userID, _ := userIDVal.(string)
	if userID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent context missing"})
		return
	}
	if !s.polarAssetsUseProvider {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "asset provider mode off"})
		return
	}
	var req agentAssetGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad json"})
		return
	}
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	if !isHexSHA256(req.SHA256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sha256 must be 64 hex chars"})
		return
	}
	if req.SizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "size_bytes must be > 0"})
		return
	}
	ws, err := s.personalWorkspaceForUser(userID)
	if err != nil || ws == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "personal workspace lookup failed"})
		return
	}
	mime := strings.TrimSpace(req.Mime)
	if mime == "" {
		mime = "application/octet-stream"
	}
	// Content-addressed catalog name → identical bytes dedup, distinct bytes are
	// distinct assets. kind=task_artifact keeps them out of module namespaces.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "task-artifact/" + req.SHA256
	}
	if code, msg := s.checkAssetUploadAllowed(ws, req.SizeBytes); code != http.StatusOK {
		c.JSON(code, gin.H{"error": msg})
		return
	}

	existing, err := s.getAssetByName(&ws, "task_artifact", name, "v1")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	var assetID int64
	if existing != nil {
		if existing.SHA256 != req.SHA256 {
			c.JSON(http.StatusConflict, gin.H{"error": "name exists with different content"})
			return
		}
		assetID = existing.ID
		if p, _, perr := s.pickProviderForAsset(assetID, ""); perr == nil && p != nil {
			c.JSON(http.StatusOK, gin.H{"asset_id": assetID, "sha256": req.SHA256, "exists": true})
			return
		}
	} else {
		wsPtr := ws
		a := &Asset{
			WorkspaceID: &wsPtr, Kind: "task_artifact", Name: name, Version: "v1",
			SHA256: req.SHA256, SizeBytes: req.SizeBytes, Mime: mime,
			Visibility: "workspace", UploadedBy: "agent",
		}
		if _, err := s.insertAsset(a); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "insert asset"})
			return
		}
		assetID = a.ID
	}
	provider, err := s.pickAnyHealthyProvider(s.regionOfWorkspace(ws))
	if err != nil || provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no healthy asset provider"})
		return
	}
	putURL, err := buildSignedPutURL(provider.BaseURL, req.SHA256, defaultAssetSignedURLTTL, req.SizeBytes, mime, []byte(provider.HMACToken))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"asset_id": assetID, "sha256": req.SHA256, "exists": false,
		"put_url": putURL, "provider_id": provider.ID,
	})
}

// POST /api/agent/assets/finalize — mark the uploaded blob ready.
func (s *Server) handleAgentAssetFinalize(c *gin.Context) {
	if _, ok := c.Get("agent_user_id"); !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent context missing"})
		return
	}
	var req struct {
		AssetID    int64 `json:"asset_id"`
		ProviderID int64 `json:"provider_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AssetID <= 0 || req.ProviderID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "asset_id + provider_id required"})
		return
	}
	a, err := s.getAssetByID(req.AssetID)
	if err != nil || a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "asset not found"})
		return
	}
	prev, _ := s.assetLocationStatus(req.AssetID, req.ProviderID)
	if err := s.markAssetLocationReady(req.AssetID, req.ProviderID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mark ready failed"})
		return
	}
	if prev != "ready" && a.WorkspaceID != nil {
		_ = s.bumpAssetUsage(*a.WorkspaceID, a.SizeBytes)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// assetDownloadURL mints a signed GET URL for an artifact's asset (used when
// listing a task's artifacts). Empty string if unavailable.
func (s *Server) assetDownloadURL(assetID int64, workspaceID string) string {
	if !s.polarAssetsUseProvider || assetID <= 0 {
		return ""
	}
	a, err := s.getAssetByID(assetID)
	if err != nil || a == nil {
		return ""
	}
	provider, _, err := s.pickProviderForAsset(a.ID, s.regionOfWorkspace(workspaceID))
	if err != nil || provider == nil {
		return ""
	}
	url, err := buildSignedBlobURL(provider.BaseURL, a.SHA256, defaultAssetSignedURLTTL, []byte(provider.HMACToken))
	if err != nil {
		return ""
	}
	return url
}
