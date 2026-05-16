package app

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type tunnelProfileJSON struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	IsActive  bool   `json:"is_active"`
	Version   int    `json:"version"`
	CreatedAt any    `json:"created_at"`
	UpdatedAt any    `json:"updated_at"`
}

// GET /api/packtunnel/profiles/active
// Returns the authenticated user's active proxy config as a tunnel profile.
func (s *Server) handlePacketTunnelActiveProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg, err := s.getActiveProxyConfig(userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if cfg == nil {
		jsonError(c, http.StatusNotFound, "no active profile")
		return
	}
	jsonOK(c, gin.H{"profile": tunnelProfileJSON{
		ID:        fmt.Sprintf("%d", cfg.ID),
		UserID:    cfg.UserID,
		Name:      cfg.Name,
		IsActive:  cfg.IsActive,
		Version:   cfg.Version,
		CreatedAt: cfg.CreatedAt,
		UpdatedAt: cfg.UpdatedAt,
	}})
}

// GET /api/packtunnel/rules
// Returns the raw Surge-format rules text of the active proxy config.
func (s *Server) handlePacketTunnelRules(c *gin.Context) {
	userID := c.GetString("user_id")
	cfg, err := s.getActiveProxyConfig(userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "server error")
		return
	}
	if cfg == nil {
		jsonError(c, http.StatusNotFound, "no active profile")
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(cfg.Content))
}
