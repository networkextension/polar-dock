package dock

// HTTP handlers for /api/prompts. Admin-only; the management UI
// reads + writes through these. Three endpoints:
//
//   GET  /api/prompts            → list every slug + content + default
//   GET  /api/prompts/:slug      → single slug detail
//   PUT  /api/prompts/:slug      → upsert content (just the text)
//
// PUT refreshes the in-memory cache so subsequent chat requests
// pick up the change without a server restart — that's the whole
// point of moving these to DB.

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handlePromptTemplateList(c *gin.Context) {
	items, err := s.listPromptTemplates()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prompts": items})
}

func (s *Server) handlePromptTemplateGet(c *gin.Context) {
	slug := strings.TrimSpace(c.Param("slug"))
	p, err := s.getPromptTemplate(slug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "prompt 不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"prompt": p})
}

type promptTemplateUpdateRequest struct {
	Content string `json:"content"`
}

func (s *Server) handlePromptTemplateUpdate(c *gin.Context) {
	// Site-level gate: site-admin always; when POLAR_ENFORCE_RESOURCE_RBAC is on,
	// also a root-team prompt:admin grant (delegation). Replaces the route's old
	// AdminMiddleware — default (flag off) is still site-admin only.
	if !s.siteGateCan(c, "prompt:admin", s.enforceResourceRBAC) {
		return
	}
	slug := strings.TrimSpace(c.Param("slug"))
	var req promptTemplateUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	before, _ := s.getPromptTemplate(slug)
	if err := s.updatePromptTemplateContent(slug, req.Content); err != nil {
		if err.Error() == "prompt slug not found" {
			c.JSON(http.StatusNotFound, gin.H{"error": "prompt 不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	updated, _ := s.getPromptTemplate(slug)
	// Audit: log slug + content size, not the full prompt body. Prompt
	// templates are operator-authored config but can run long; the size
	// delta is enough to spot tampering, the full body is in DB anyway.
	var beforeLen int
	if before != nil {
		beforeLen = len(before.Content)
	}
	var afterLen int
	if updated != nil {
		afterLen = len(updated.Content)
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "prompt_template.update",
		TargetKind: "site_setting",
		TargetID:   slug,
		Payload: map[string]any{
			"before": map[string]any{"content_len": beforeLen},
			"after":  map[string]any{"content_len": afterLen},
		},
	})
	c.JSON(http.StatusOK, gin.H{"prompt": updated})
}
