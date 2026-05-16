package dock

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// /api/llm-proxy/tokens : admin-gated token CRUD.

func (s *Server) handleLLMProxyTokenList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	items, err := s.listLLMProxyTokens(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": items})
}

func (s *Server) handleLLMProxyTokenCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	if strings.TrimSpace(userIDStr) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未登录"})
		return
	}
	var req struct {
		Name               string  `json:"name" binding:"required"`
		DefaultLLMConfigID *int64  `json:"default_llm_config_id"`
		RateLimitRPM       int     `json:"rate_limit_rpm"`
		MonthlyQuotaUSD    float64 `json:"monthly_quota_usd"`
		Scopes             string  `json:"scopes"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 必填"})
		return
	}
	// If a default_llm_config_id was supplied, sanity-check it belongs
	// to this workspace before storing — defense against a forged id
	// from a non-admin trying to cross-tenant via a hand-crafted POST.
	if req.DefaultLLMConfigID != nil && *req.DefaultLLMConfigID > 0 {
		cfg, _, err := s.getLLMConfigForOwner(workspaceID, *req.DefaultLLMConfigID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		if cfg == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "default_llm_config_id 不存在或不属于本 workspace"})
			return
		}
	}
	tok, err := s.createLLMProxyToken(workspaceID, userIDStr, name, req.DefaultLLMConfigID, req.RateLimitRPM, req.MonthlyQuotaUSD, req.Scopes, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":   tok,
		"warning": "plaintext 只显示一次，关闭后无法找回。请立即复制保存。",
	})
}

// GET /api/llm-proxy/tokens/:id/calls?limit=N
func (s *Server) handleLLMProxyTokenCalls(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	// Ownership check: token must belong to the workspace.
	tok, err := s.getLLMProxyToken(workspaceID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if tok == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token 不存在"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	calls, err := s.listLLMProxyCalls(workspaceID, id, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"calls": calls})
}

// GET /api/llm-proxy/tokens/:id/calls/aggregate?days=N
func (s *Server) handleLLMProxyTokenCallsAggregate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	tok, err := s.getLLMProxyToken(workspaceID, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if tok == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token 不存在"})
		return
	}
	days, _ := strconv.Atoi(c.Query("days"))
	agg, err := s.aggregateLLMProxyCalls(workspaceID, id, days)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"aggregate": agg, "days": days})
}

func (s *Server) handleLLMProxyTokenRevoke(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 id"})
		return
	}
	tok, err := s.revokeLLMProxyToken(workspaceID, id, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if tok == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "token 不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": tok})
}
