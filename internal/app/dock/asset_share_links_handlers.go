package dock

// asset_share_links_handlers.go — 4 routes:
//
//   POST   /api/assets/:id/share   tenant creates a share for own asset
//   GET    /api/share-links        caller's own list
//   DELETE /api/share-links/:id    creator-only revoke
//   GET    /s/:short_code          PUBLIC anonymous download

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// handleAssetShareCreate — POST /api/assets/:id/share. Tenant body:
//   { expires_at?: ISO8601, max_downloads?: int, require_login?: bool }
func (s *Server) handleAssetShareCreate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	assetID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || assetID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	asset, err := s.getAssetByID(assetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Don't leak existence — caller must own the asset.
	if asset == nil || asset.WorkspaceID == nil || *asset.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		ExpiresAt    *time.Time `json:"expires_at,omitempty"`
		MaxDownloads *int       `json:"max_downloads,omitempty"`
		RequireLogin bool       `json:"require_login,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.MaxDownloads != nil && *body.MaxDownloads <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "max_downloads must be > 0 (omit for unlimited)"})
		return
	}
	if body.ExpiresAt != nil && body.ExpiresAt.Before(time.Now()) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "expires_at is in the past"})
		return
	}
	link := &AssetShareLink{
		AssetID:      assetID,
		CreatedBy:    userID,
		ExpiresAt:    body.ExpiresAt,
		MaxDownloads: body.MaxDownloads,
		RequireLogin: body.RequireLogin,
	}
	if _, err := s.insertAssetShareLink(link); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{
		"link":       link,
		"public_url": "/s/" + link.ShortCode,
	})
}

func (s *Server) handleAssetShareList(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	rows, err := s.listAssetShareLinksByCaller(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"links": rows})
}

func (s *Server) handleAssetShareDelete(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	link, err := s.getAssetShareLinkByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// 404 not 403 on non-owner — don't leak existence.
	if link == nil || link.CreatedBy != userID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := s.deleteAssetShareLink(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleAssetSharePublicDownload — GET /s/:short_code. PUBLIC route
// (no AuthMiddleware on this one). Validates expiry + max_downloads
// + require_login, then streams the blob from the same local-FS
// stub as /api/assets/:id/blob.
func (s *Server) handleAssetSharePublicDownload(c *gin.Context) {
	code := strings.TrimSpace(c.Param("short_code"))
	if code == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	link, err := s.getAssetShareLinkByCode(code)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if link == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		c.JSON(http.StatusGone, gin.H{"error": "link expired"})
		return
	}
	if link.MaxDownloads != nil && link.DownloadCount >= *link.MaxDownloads {
		c.JSON(http.StatusGone, gin.H{"error": "download limit reached"})
		return
	}
	if link.RequireLogin {
		// Best-effort check — AuthMiddleware doesn't run here. If no
		// session is bound, redirect to login with the link as next.
		if _, ok := c.Get("user_id"); !ok {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error":    "login required",
				"login_to": "/?next=/s/" + code,
			})
			return
		}
	}
	asset, err := s.getAssetByID(link.AssetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if asset == nil {
		c.JSON(http.StatusGone, gin.H{"error": "underlying asset removed"})
		return
	}
	// Increment BEFORE streaming so a refresh-spam race doesn't let
	// max_downloads get exceeded. Risk of over-count on interrupted
	// streams is acceptable for v1.
	if _, err := s.incrementShareLinkDownload(link.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Same branch as handleAssetBlob: provider 302 when the flag is
	// on; local-FS stream otherwise. The signed URL for the public
	// download is identical to the auth'd download — the recipient
	// just doesn't see /api/assets/:id/blob in their URL bar.
	if s.polarAssetsUseProvider {
		// Public share recipients aren't a workspace member, so we
		// can't infer a region. Pass "" — the picker treats it as
		// any-region and skips the warm-pull signal.
		s.redirectToProviderBlob(c, asset, "")
		return
	}
	path := assetBlobPath(s.uploadDir, asset.WorkspaceID, asset.SHA256)
	f, err := os.Open(path)
	if err != nil {
		c.JSON(http.StatusGone, gin.H{"error": "blob missing on disk"})
		return
	}
	defer f.Close()
	c.Header("Content-Type", asset.Mime)
	c.Header("Content-Length", strconv.FormatInt(asset.SizeBytes, 10))
	c.Header("X-Asset-SHA256", asset.SHA256)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}
