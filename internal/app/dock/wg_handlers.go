package dock

// HTTP layer for wg-mac join control plane.
//
// Public /v1/* — see JOIN_PROTOCOL.md §2:
//   POST /v1/register         token-in-body auth
//   GET  /v1/peers            Bearer device-token + X-Device-Id
//   POST /v1/heartbeat        Bearer device-token + X-Device-Id
//   POST /v1/leave            Bearer device-token + X-Device-Id
//   POST /v1/token/refresh    Bearer device-token + X-Device-Id
//   GET  /v1/install          unauthenticated
//   GET  /v1/install/:version unauthenticated
//   GET  /v1/bundle           unauthenticated (302 to /v1/bundle/<latest>)
//   GET  /v1/bundle/:version  unauthenticated
//
// Admin /api/admin/wg-* — AuthMiddleware + AdminMiddleware.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- helpers ----

func wgServerBaseURL(c *gin.Context) string {
	// Prefer X-Forwarded-Proto / Host so curl-bash users get a URL
	// that matches what they're hitting through nginx.
	scheme := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
	if scheme == "" {
		if c.Request.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = c.Request.Host
	}
	return scheme + "://" + host
}

func wgBundleBlobDir(s *Server) string {
	return s.uploadDir + "/wg-bundles"
}

// ---- /v1/register ----

type wgRegisterRequest struct {
	Token     string      `json:"token" binding:"required"`
	Pubkey    string      `json:"pubkey" binding:"required"`
	Hostname  string      `json:"hostname"`
	OS        string      `json:"os"`
	Arch      string      `json:"arch"`
	AgentVer  string      `json:"agent_ver"`
	LANAddrs  []WGLanAddr `json:"lan_addrs"`
	WGListen  int         `json:"wg_listen"`
	SiteSlug  string      `json:"site_slug"`
}

type wgPeerResponse struct {
	Pubkey       string   `json:"pubkey"`
	WGIP         string   `json:"wg_ip,omitempty"`
	Endpoint     string   `json:"endpoint"`
	SiteSlug     string   `json:"site_id,omitempty"`
	Hostname     string   `json:"hostname,omitempty"`
	AllowedExtra []string `json:"allowed_extra,omitempty"`
}

type wgHubResponse struct {
	Pubkey   string `json:"pubkey"`
	Endpoint string `json:"endpoint"`
	WGIP     string `json:"wg_ip"`
}

type wgRegisterResponse struct {
	DeviceID     string           `json:"device_id"`
	DeviceIP     string           `json:"device_ip"`
	SiteSlug     string           `json:"site_id"`
	MeshCIDR     string           `json:"mesh_cidr"`
	Hub          wgHubResponse    `json:"hub"`
	Peers        []wgPeerResponse `json:"peers"`
	KeepaliveSec int              `json:"keepalive_sec"`
	RefreshSec   int              `json:"refresh_sec"`
	Token        string           `json:"token"`
	TokenExpires *time.Time       `json:"token_expires,omitempty"`
}

func (s *Server) handleWGRegister(c *gin.Context) {
	var req wgRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	publicIP := extractPublicIP(
		c.GetHeader("X-Forwarded-For"),
		c.GetHeader("X-Real-IP"),
		c.Request.RemoteAddr,
	)
	in := wgRegisterInput{
		Token:        req.Token,
		Pubkey:       req.Pubkey,
		Hostname:     req.Hostname,
		OS:           req.OS,
		Arch:         req.Arch,
		AgentVer:     req.AgentVer,
		LANAddrs:     req.LANAddrs,
		WGListenPort: req.WGListen,
		SiteSlug:     req.SiteSlug,
		PublicIP:     publicIP,
	}
	res, err := s.allocateWGDevice(c.Request.Context(), in)
	if err != nil {
		switch {
		case errors.Is(err, errWGTokenInvalid), errors.Is(err, errWGTokenExpired), errors.Is(err, errWGTokenRevoked):
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		case errors.Is(err, errWGTokenAlreadyBound):
			c.JSON(http.StatusConflict, gin.H{"error": "token_already_bound"})
		case errors.Is(err, errWGPubkeyTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "pubkey_already_registered"})
		case errors.Is(err, errWGSiteExhausted), errors.Is(err, errWGSiteSlotsExhausted):
			c.JSON(http.StatusInsufficientStorage, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "register failed: " + err.Error()})
		}
		return
	}
	// Re-fetch site so we have its slug (re-register path may not populate).
	siteSlug := res.Device.SiteSlug
	if siteSlug == "" {
		if sites, err := s.listWGSites(); err == nil {
			for _, s := range sites {
				if s.ID == res.Device.SiteID {
					siteSlug = s.Slug
					break
				}
			}
		}
	}
	resp, err := s.buildPeerListResponse(res.Device, siteSlug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "peer list: " + err.Error()})
		return
	}
	resp.DeviceID = res.Device.DeviceID
	resp.Token = res.PlaintextToken
	resp.TokenExpires = res.TokenExpiresAt
	c.JSON(http.StatusOK, resp)
}

// ---- /v1/peers ----

// extractDeviceBearer returns the bearer device-token + X-Device-Id
// from the request. On missing/invalid auth, writes an error response
// and returns (nil, false).
func (s *Server) extractDeviceBearer(c *gin.Context) (*WGDevice, bool) {
	raw := extractBearerToken(c)
	if raw == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return nil, false
	}
	devIDClaimed := strings.TrimSpace(c.GetHeader("X-Device-Id"))
	if devIDClaimed == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing X-Device-Id"})
		return nil, false
	}
	dev, err := s.getWGDeviceByTokenHash(hashWGToken(raw))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return nil, false
	}
	if dev == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device token"})
		return nil, false
	}
	if dev.DeviceID != devIDClaimed {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token does not match X-Device-Id"})
		return nil, false
	}
	if dev.TokenExpiresAt != nil && time.Now().UTC().After(*dev.TokenExpiresAt) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
		return nil, false
	}
	return dev, true
}

func (s *Server) handleWGPeers(c *gin.Context) {
	dev, ok := s.extractDeviceBearer(c)
	if !ok {
		return
	}
	siteSlug := ""
	if sites, err := s.listWGSites(); err == nil {
		for _, st := range sites {
			if st.ID == dev.SiteID {
				siteSlug = st.Slug
				break
			}
		}
	}
	resp, err := s.buildPeerListResponse(dev, siteSlug)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, resp)
}

// buildPeerListResponse computes the per-device peer list per
// JOIN_PROTOCOL §5.
func (s *Server) buildPeerListResponse(dev *WGDevice, siteSlug string) (*wgRegisterResponse, error) {
	hub, err := s.getWGHub()
	if err != nil {
		return nil, fmt.Errorf("hub: %w", err)
	}
	lanPeers, err := s.listWGDevicesInSite(dev.SiteID, dev.ID)
	if err != nil {
		return nil, fmt.Errorf("lan peers: %w", err)
	}
	peerOut := make([]wgPeerResponse, 0, len(lanPeers)+1)
	for _, p := range lanPeers {
		endpoint := ""
		if len(p.LANAddrs) > 0 {
			ip := firstLANIP(p.LANAddrs[0].CIDR)
			if ip != "" {
				endpoint = fmt.Sprintf("%s:%d", ip, p.WGListenPort)
			}
		}
		peerOut = append(peerOut, wgPeerResponse{
			Pubkey:   p.Pubkey,
			WGIP:     p.DeviceIP,
			Endpoint: endpoint,
			Hostname: p.Hostname,
		})
	}
	// Hub entry.
	otherIdx, err := s.otherSiteIndices(dev.SiteID)
	if err != nil {
		return nil, fmt.Errorf("other site indices: %w", err)
	}
	allowedExtra := make([]string, 0, len(otherIdx)+1)
	allowedExtra = append(allowedExtra, "10.88.0.0/24")
	for _, idx := range otherIdx {
		allowedExtra = append(allowedExtra, fmt.Sprintf("10.88.%d.0/24", idx))
	}
	peerOut = append(peerOut, wgPeerResponse{
		Pubkey:       hub.Pubkey,
		WGIP:         hub.WGIP,
		Endpoint:     hub.Endpoint,
		SiteSlug:     "hub",
		AllowedExtra: allowedExtra,
	})
	return &wgRegisterResponse{
		DeviceID:     dev.DeviceID,
		DeviceIP:     dev.DeviceIP,
		SiteSlug:     siteSlug,
		MeshCIDR:     hub.MeshCIDR,
		Hub: wgHubResponse{
			Pubkey:   hub.Pubkey,
			Endpoint: hub.Endpoint,
			WGIP:     hub.WGIP,
		},
		Peers:        peerOut,
		KeepaliveSec: hub.KeepaliveSec,
		RefreshSec:   hub.RefreshSec,
		TokenExpires: dev.TokenExpiresAt,
	}, nil
}

// firstLANIP extracts the IP portion of a "192.168.1.5/24" CIDR. Used
// to build the LAN-direct peer endpoint URL.
func firstLANIP(cidr string) string {
	if i := strings.Index(cidr, "/"); i > 0 {
		return cidr[:i]
	}
	return cidr
}

// ---- /v1/heartbeat ----

type wgHeartbeatRequest struct {
	LANAddrs   []WGLanAddr `json:"lan_addrs"`
	WGEndpoint string      `json:"wg_endpoint"`
	Stats      *struct {
		RXBytes          *int64 `json:"rx_bytes"`
		TXBytes          *int64 `json:"tx_bytes"`
		LastHandshakeSec *int64 `json:"last_handshake_sec"`
	} `json:"stats"`
}

func (s *Server) handleWGHeartbeat(c *gin.Context) {
	dev, ok := s.extractDeviceBearer(c)
	if !ok {
		return
	}
	var req wgHeartbeatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	rec := &WGHeartbeatRecord{
		DeviceID:   dev.ID,
		WGEndpoint: strings.TrimSpace(req.WGEndpoint),
		LANAddrs:   req.LANAddrs,
	}
	if req.Stats != nil {
		rec.RXBytes = req.Stats.RXBytes
		rec.TXBytes = req.Stats.TXBytes
		rec.LastHandshakeSec = req.Stats.LastHandshakeSec
	}
	if err := s.insertWGHeartbeat(rec, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "heartbeat: " + err.Error()})
		return
	}
	c.Status(http.StatusOK)
}

// ---- /v1/leave ----

func (s *Server) handleWGLeave(c *gin.Context) {
	dev, ok := s.extractDeviceBearer(c)
	if !ok {
		return
	}
	if err := s.markWGDeviceRemoved(dev.ID, time.Now().UTC()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "leave: " + err.Error()})
		return
	}
	c.Status(http.StatusOK)
}

// ---- /v1/token/refresh ----

type wgTokenRefreshResponse struct {
	Token   string     `json:"token"`
	Expires *time.Time `json:"expires,omitempty"`
}

func (s *Server) handleWGTokenRefresh(c *gin.Context) {
	dev, ok := s.extractDeviceBearer(c)
	if !ok {
		return
	}
	plaintext, _ := newWGTokenPlaintext()
	newHash := hashWGToken(plaintext)
	// New token inherits the same TTL as the old one (sliding); if the
	// old was non-expiring, the new one is too.
	var newExpires *time.Time
	if dev.TokenExpiresAt != nil {
		// Slide by the same lifetime the old token had. Original
		// lifetime is unknown, so default to 90 days for now —
		// matches the JOIN_PROTOCOL §3 "issued for 90d" comment.
		v := time.Now().UTC().Add(90 * 24 * time.Hour)
		newExpires = &v
	}
	if err := s.updateWGDeviceTokenHash(dev.ID, newHash, newExpires); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refresh: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, wgTokenRefreshResponse{Token: plaintext, Expires: newExpires})
}

// ---- /v1/install (+ /:version) ----

func (s *Server) handleWGInstallScript(c *gin.Context) {
	version := strings.TrimSpace(c.Param("version"))
	if version == "" {
		latest, err := s.getLatestWGBundle()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup latest: " + err.Error()})
			return
		}
		if latest == nil {
			version = "latest"
		} else {
			version = latest.Version
		}
	}
	hub, err := s.getWGHub()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hub: " + err.Error()})
		return
	}
	script, err := renderWGInstallScript(wgInstallScriptInput{
		Server:        wgServerBaseURL(c),
		BundleVersion: version,
		MeshCIDR:      hub.MeshCIDR,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "render: " + err.Error()})
		return
	}
	c.Header("Content-Type", "text/x-shellscript; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, script)
}

// ---- admin: hub config ----

func (s *Server) handleAdminWGHubGet(c *gin.Context) {
	h, err := s.getWGHub()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hub": h, "configured": h.Configured()})
}

func (s *Server) handleAdminWGHubUpdate(c *gin.Context) {
	var req struct {
		Pubkey       string `json:"pubkey"`
		Endpoint     string `json:"endpoint"`
		WGIP         string `json:"wg_ip"`
		MeshCIDR     string `json:"mesh_cidr"`
		KeepaliveSec int    `json:"keepalive_sec"`
		RefreshSec   int    `json:"refresh_sec"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	h := &WGHub{
		Pubkey:       req.Pubkey,
		Endpoint:     req.Endpoint,
		WGIP:         req.WGIP,
		MeshCIDR:     req.MeshCIDR,
		KeepaliveSec: req.KeepaliveSec,
		RefreshSec:   req.RefreshSec,
	}
	out, err := s.updateWGHub(h, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"hub": out, "configured": out.Configured()})
}

// ---- admin: tokens ----

func (s *Server) handleAdminWGTokenList(c *gin.Context) {
	toks, err := s.listWGTokens()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tokens": toks})
}

func (s *Server) handleAdminWGTokenCreate(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	var req struct {
		Label     string `json:"label" binding:"required"`
		TTLDays   int    `json:"ttl_days"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_input"})
		return
	}
	var expiresAt *time.Time
	if req.TTLDays > 0 {
		v := time.Now().UTC().Add(time.Duration(req.TTLDays) * 24 * time.Hour)
		expiresAt = &v
	}
	tok, err := s.createWGToken(req.Label, userIDStr, expiresAt, time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token":   tok,
		"warning": "plaintext 只显示一次，关闭后无法找回。请立即复制保存。",
	})
}

func (s *Server) handleAdminWGTokenRevoke(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := s.revokeWGToken(id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- admin: devices ----

func (s *Server) handleAdminWGDeviceList(c *gin.Context) {
	includeRemoved := c.Query("include_removed") == "1"
	devs, err := s.listWGDevices(includeRemoved)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"devices": devs})
}

func (s *Server) handleAdminWGDeviceRemove(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_id"})
		return
	}
	if err := s.markWGDeviceRemoved(id, time.Now().UTC()); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- admin: sites ----

func (s *Server) handleAdminWGSiteList(c *gin.Context) {
	sites, err := s.listWGSites()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"sites": sites})
}
