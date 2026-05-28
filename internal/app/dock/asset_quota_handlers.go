package dock

// asset_quota_handlers.go — three routes for the per-workspace
// asset quota. Two admin, one tenant:
//
//   GET   /api/asset-quota                  tenant: caller's own
//   GET   /api/admin/asset-quotas           admin: all workspaces
//   PATCH /api/admin/asset-quotas/:ws       admin: set new cap

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAssetQuotaSelf(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	q, err := s.getOrCreateAssetQuota(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"quota": q})
}

func (s *Server) handleAssetQuotaAdminList(c *gin.Context) {
	rows, err := s.listAllAssetQuotas()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	cfg, err := s.assetRootQuotaConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"quotas":      rows,
		"root_config": cfg,
		"root_cap_bytes": cfg.rootCapBytes(),
	})
}

func (s *Server) handleAssetQuotaAdminSet(c *gin.Context) {
	targetWS := c.Param("ws")
	if targetWS == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ws required"})
		return
	}
	var body struct {
		QuotaBytes int64 `json:"quota_bytes"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.QuotaBytes < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "quota_bytes must be >= 0"})
		return
	}
	// Refresh usage first so the over_quota flip is based on the
	// authoritative SUM, not the cached value (which may be drifted).
	if err := s.recomputeAssetUsage(targetWS); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	q, overQuota, err := s.setAssetQuota(targetWS, body.QuotaBytes)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	resp := gin.H{"quota": q}
	if overQuota {
		needFree := q.UsageBytes - q.QuotaBytes
		resp["warning"] = fmt.Sprintf(
			"workspace currently uses %.2f GB; new cap %.2f GB. uploads disabled until %.2f GB freed.",
			float64(q.UsageBytes)/float64(1<<30),
			float64(q.QuotaBytes)/float64(1<<30),
			float64(needFree)/float64(1<<30),
		)
	}
	// Best-effort audit log on the platform-admin side.
	if user, ok := c.Get("user_id"); ok {
		s.LogAdminAction(c, AdminAuditIn{
			Category:     AuditCatConfig,
			Action:       "asset_quota_set",
			TargetTeamID: targetWS,
			TargetKind:   "asset_quota",
			TargetID:     targetWS,
			Payload: map[string]any{
				"quota_bytes": body.QuotaBytes,
				"usage_bytes": q.UsageBytes,
				"over_quota":  overQuota,
				"actor":       user,
			},
		})
	}
	c.JSON(http.StatusOK, resp)
}
