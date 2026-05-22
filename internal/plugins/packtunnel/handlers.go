package packtunnel

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// mustSystemWorkspaceID returns the cached system workspace ID
// (pre-fetched in plugin.New from /internal/v1/users/system/workspace).
// If empty (dock hadn't bootstrapped the system user at startup),
// returns 500 — better than inserting a row with empty workspace.
func (p *Plugin) mustSystemWorkspaceID(c *gin.Context) string {
	if p.SystemWorkspaceID == "" {
		log.Printf("packtunnel: system workspace id not initialized; dock /internal/v1/users/system/workspace failed at startup")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return ""
	}
	return p.SystemWorkspaceID
}

type packTunnelProfileRequest struct {
	ID        string                    `json:"id"`
	Name      string                    `json:"name" binding:"required"`
	Type      string                    `json:"type" binding:"required"`
	Server    PackTunnelServerEndpoint  `json:"server"`
	Auth      PackTunnelAuth            `json:"auth"`
	Options   PackTunnelOptions         `json:"options"`
	Transport *PackTunnelTransport      `json:"transport,omitempty"`
	Metadata  packTunnelMetadataRequest `json:"metadata"`
}

type packTunnelMetadataRequest struct {
	Priority    int    `json:"priority"`
	Enabled     *bool  `json:"enabled"`
	Editable    *bool  `json:"editable"`
	Source      string `json:"source"`
	CountryCode string `json:"country_code"`
	CountryFlag string `json:"country_flag"`
	IsActive    bool   `json:"is_active"`
}

func (p *Plugin) handlePackTunnelProfileList(c *gin.Context) {
	items, err := p.listPackTunnelProfiles(p.SystemWorkspaceID)
	if err != nil {
		log.Printf("packtunnel list profiles failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	active, err := p.getActivePackTunnelProfile(p.SystemWorkspaceID)
	if err != nil {
		log.Printf("packtunnel get active profile failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"profiles":       items,
		"active_profile": active,
	})
}

func (p *Plugin) handlePackTunnelProfileGet(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}
	item, err := p.getPackTunnelProfile(p.SystemWorkspaceID, id)
	if err != nil {
		log.Printf("packtunnel get profile failed id=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": item})
}

func (p *Plugin) handlePackTunnelActiveProfileGet(c *gin.Context) {
	item, err := p.getActivePackTunnelProfile(p.SystemWorkspaceID)
	if err != nil {
		log.Printf("packtunnel active get failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "当前没有启用配置"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": item})
}

func (p *Plugin) handlePackTunnelProfileCreate(c *gin.Context) {
	item, ok := buildPackTunnelProfileFromRequest(c)
	if !ok {
		return
	}

	created, err := p.createPackTunnelProfile(p.SystemWorkspaceID, systemUserID, item, time.Now())
	if err != nil {
		log.Printf("packtunnel create failed name=%s type=%s: %v", item.Name, item.Type, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"profile": created, "message": "配置已创建"})
}

func (p *Plugin) handlePackTunnelProfileUpdate(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}

	item, ok := buildPackTunnelProfileFromRequest(c)
	if !ok {
		return
	}
	item.ID = id

	updated, err := p.updatePackTunnelProfile(p.SystemWorkspaceID, id, item, time.Now())
	if err != nil {
		log.Printf("packtunnel update failed id=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": updated, "message": "配置已更新"})
}

func (p *Plugin) handlePackTunnelProfileDelete(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}
	ok, err := p.deletePackTunnelProfile(p.SystemWorkspaceID, id)
	if err != nil {
		log.Printf("packtunnel delete failed id=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "配置已删除"})
}

func (p *Plugin) handlePackTunnelProfileActivate(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效配置"})
		return
	}

	item, err := p.setActivePackTunnelProfile(p.SystemWorkspaceID, id, time.Now())
	if err != nil {
		log.Printf("packtunnel activate failed id=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "启用失败"})
		return
	}
	if item == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "配置不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"profile": item, "message": "配置已启用"})
}

func buildPackTunnelProfileFromRequest(c *gin.Context) (PackTunnelProfile, bool) {
	var req packTunnelProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return PackTunnelProfile{}, false
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Type = strings.TrimSpace(req.Type)
	req.Server.Address = strings.TrimSpace(req.Server.Address)
	req.Auth.Password = strings.TrimSpace(req.Auth.Password)
	req.Auth.Method = strings.TrimSpace(req.Auth.Method)
	req.Metadata.Source = strings.TrimSpace(req.Metadata.Source)
	req.Metadata.CountryCode = strings.TrimSpace(req.Metadata.CountryCode)
	req.Metadata.CountryFlag = strings.TrimSpace(req.Metadata.CountryFlag)

	if req.Name == "" || req.Type == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "名称和类型不能为空"})
		return PackTunnelProfile{}, false
	}
	if req.Server.Address == "" || req.Server.Port <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "服务器地址和端口不能为空"})
		return PackTunnelProfile{}, false
	}
	if req.Metadata.Source == "" {
		req.Metadata.Source = "local"
	}
	enabled := true
	if req.Metadata.Enabled != nil {
		enabled = *req.Metadata.Enabled
	}
	editable := true
	if req.Metadata.Editable != nil {
		editable = *req.Metadata.Editable
	}
	if req.Transport != nil {
		req.Transport.Kind = strings.TrimSpace(req.Transport.Kind)
		if req.Transport.Kind == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "传输层类型不能为空"})
			return PackTunnelProfile{}, false
		}
		if req.Transport.Kind == "kcptun" && req.Transport.KCPTun == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "kcptun 配置不能为空"})
			return PackTunnelProfile{}, false
		}
	}

	return PackTunnelProfile{
		ID:   strings.TrimSpace(req.ID),
		Name: req.Name,
		Type: req.Type,
		Server: PackTunnelServerEndpoint{
			Address: req.Server.Address,
			Port:    req.Server.Port,
		},
		Auth: PackTunnelAuth{
			Password: req.Auth.Password,
			Method:   req.Auth.Method,
		},
		Options:   req.Options,
		Transport: req.Transport,
		Metadata: PackTunnelProfileMetadata{
			Priority:    req.Metadata.Priority,
			Enabled:     enabled,
			Editable:    editable,
			Source:      req.Metadata.Source,
			CountryCode: req.Metadata.CountryCode,
			CountryFlag: req.Metadata.CountryFlag,
			IsActive:    req.Metadata.IsActive,
		},
	}, true
}
