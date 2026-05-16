package app

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// GET /api/proxy-configs
func (s *Server) handleProxyConfigList(c *gin.Context) {
	userID := c.GetString("user_id")
	configs, err := s.listProxyConfigs(userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if configs == nil {
		configs = []*ProxyConfig{}
	}
	jsonOK(c, gin.H{"configs": configs})
}

// POST /api/proxy-configs
func (s *Server) handleProxyConfigCreate(c *gin.Context) {
	userID := c.GetString("user_id")

	var req struct {
		Name     string `json:"name" binding:"required,min=1,max=64"`
		Content  string `json:"content" binding:"required"`
		IsActive bool   `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid input")
		return
	}

	// If this config is marked active, deactivate others first
	if req.IsActive {
		if _, err := s.db.Exec(
			`UPDATE proxy_configs SET is_active = false WHERE user_id = $1`, userID,
		); err != nil {
			jsonError(c, http.StatusInternalServerError, "server error")
			return
		}
	}

	cfg := &ProxyConfig{
		UserID:   userID,
		Name:     req.Name,
		Content:  req.Content,
		IsActive: req.IsActive,
	}
	if err := s.createProxyConfig(cfg); err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	c.JSON(http.StatusCreated, cfg)
}

// GET /api/proxy-configs/active
func (s *Server) handleProxyConfigGetActive(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg, err := s.getActiveProxyConfig(userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if cfg == nil {
		jsonError(c, http.StatusNotFound, "no active config")
		return
	}
	jsonOK(c, cfg)
}

// GET /api/proxy-configs/:id
func (s *Server) handleProxyConfigGet(c *gin.Context) {
	userID := c.GetString("user_id")
	id, err := parseID(c)
	if err != nil {
		return
	}
	cfg, err := s.getProxyConfig(id, userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if cfg == nil {
		jsonError(c, http.StatusNotFound, "not found")
		return
	}
	jsonOK(c, cfg)
}

// PUT /api/proxy-configs/:id
func (s *Server) handleProxyConfigUpdate(c *gin.Context) {
	userID := c.GetString("user_id")
	id, err := parseID(c)
	if err != nil {
		return
	}

	var req struct {
		Name    string `json:"name" binding:"required,min=1,max=64"`
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "invalid input")
		return
	}

	cfg := &ProxyConfig{ID: id, UserID: userID, Name: req.Name, Content: req.Content}
	if err := s.updateProxyConfig(cfg); err != nil {
		if err.Error() == "not found" {
			jsonError(c, http.StatusNotFound, "not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}

	// return updated record
	updated, _ := s.getProxyConfig(id, userID)
	jsonOK(c, updated)
}

// DELETE /api/proxy-configs/:id
func (s *Server) handleProxyConfigDelete(c *gin.Context) {
	userID := c.GetString("user_id")
	id, err := parseID(c)
	if err != nil {
		return
	}
	if err := s.deleteProxyConfig(id, userID); err != nil {
		if err.Error() == "not found" {
			jsonError(c, http.StatusNotFound, "not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// POST /api/proxy-configs/:id/activate
func (s *Server) handleProxyConfigActivate(c *gin.Context) {
	userID := c.GetString("user_id")
	id, err := parseID(c)
	if err != nil {
		return
	}
	if err := s.activateProxyConfig(id, userID); err != nil {
		if err.Error() == "not found" {
			jsonError(c, http.StatusNotFound, "not found")
			return
		}
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	cfg, _ := s.getProxyConfig(id, userID)
	jsonOK(c, cfg)
}

func parseID(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		jsonError(c, http.StatusBadRequest, "invalid id")
		return 0, err
	}
	return id, nil
}
