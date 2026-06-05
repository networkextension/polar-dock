package dock

// asset_internal_handlers.go — the /internal/v1/assets/* surface that
// the polar-sdk Assets client and modules (release / video / iosdist…)
// consume over HMAC plugin auth. Unlike /api/assets/* (browser session
// auth), these endpoints take the acting workspace explicitly in the
// request, since the caller is a trusted plugin acting on its behalf.
//
// Two halves:
//
//   read  — by-name / by-sha256 / by-id / blob / delete: wrap the
//           existing store funcs, emit the SDK's bare AssetMeta shape.
//
//   write — the "bytes bypass dock" path (doc/arch/release-system.md):
//             POST /upload-grant   → {asset_id, put_url}  (signed PUT)
//             PUT  bytes straight to the provider (never touches dock)
//             POST /finalize       → mark location ready, bump usage
//           and the download counterpart:
//             GET  /:id/download-url → {url} (signed GET) so the SDK
//             streams the blob direct from the provider.
//
// Flag-gated by polarAssetsUseProvider: with providers off, upload-grant
// 503s (there's no provider to PUT to) and callers fall back to the
// through-dock multipart upload.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// assetMetaDTO is the wire shape the SDK's AssetMeta unmarshals from.
// It differs from the internal Asset only in metadata: Asset stores
// raw JSON bytes; the SDK wants a JSON object, so we pass it through
// as json.RawMessage (an object on the wire) rather than base64 bytes.
type assetMetaDTO struct {
	ID          int64           `json:"id"`
	WorkspaceID *string         `json:"workspace_id,omitempty"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	SHA256      string          `json:"sha256"`
	SizeBytes   int64           `json:"size_bytes"`
	Mime        string          `json:"mime"`
	Visibility  string          `json:"visibility"`
	SourceURL   *string         `json:"source_url,omitempty"`
	UploadedBy  string          `json:"uploaded_by"`
	Pinned      bool            `json:"pinned"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

func toAssetMetaDTO(a *Asset) assetMetaDTO {
	meta := json.RawMessage(a.Metadata)
	if len(meta) == 0 {
		meta = json.RawMessage(`{}`)
	}
	return assetMetaDTO{
		ID:          a.ID,
		WorkspaceID: a.WorkspaceID,
		Kind:        a.Kind,
		Name:        a.Name,
		Version:     a.Version,
		SHA256:      a.SHA256,
		SizeBytes:   a.SizeBytes,
		Mime:        a.Mime,
		Visibility:  a.Visibility,
		SourceURL:   a.SourceURL,
		UploadedBy:  a.UploadedBy,
		Pinned:      a.Pinned,
		Metadata:    meta,
		CreatedAt:   a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   a.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// isHexSHA256 reports whether s is a 64-char lowercase hex string.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// ── reads ────────────────────────────────────────────────────────

func (s *Server) handleInternalAssetGet(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if a == nil || !canCallerReadAsset(a, c.Query("workspace_id")) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, toAssetMetaDTO(a))
}

func (s *Server) handleInternalAssetGetByName(c *gin.Context) {
	kind := strings.TrimSpace(c.Query("kind"))
	name := strings.TrimSpace(c.Query("name"))
	if kind == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind + name required"})
		return
	}
	version := strings.TrimSpace(c.Query("version"))
	if version == "" {
		version = "v1"
	}
	ws := strings.TrimSpace(c.Query("workspace_id"))
	var lookupWS *string
	if ws != "" {
		lookupWS = &ws
	}
	a, err := s.getAssetByName(lookupWS, kind, name, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if a == nil || !canCallerReadAsset(a, ws) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, toAssetMetaDTO(a))
}

func (s *Server) handleInternalAssetStatBySHA(c *gin.Context) {
	sha := strings.ToLower(strings.TrimSpace(c.Param("sha256")))
	if !isHexSHA256(sha) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sha256 must be 64 hex chars"})
		return
	}
	a, err := s.getAssetBySHA256(sha)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, toAssetMetaDTO(a))
}

func (s *Server) handleInternalAssetDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	if err := s.deleteAsset(id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": id})
}

// handleInternalAssetBlob is the through-dock download fallback the SDK
// uses when it can't (or won't) take a signed direct-download URL. With
// providers on it 302s to a signed provider GET; with them off it
// streams from the local stub dir. Prefer /:id/download-url to bypass
// dock entirely.
func (s *Server) handleInternalAssetBlob(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	ws := c.Query("workspace_id")
	if a == nil || !canCallerReadAsset(a, ws) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if s.polarAssetsUseProvider {
		s.redirectToProviderBlob(c, a, s.regionOfWorkspace(ws))
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider mode off; use /api blob path"})
}

// handleInternalAssetDownloadURL mints a short-lived signed GET URL so
// the caller streams the blob straight from the provider — dock never
// sees the bytes. 503 if no provider holds the asset yet.
func (s *Server) handleInternalAssetDownloadURL(c *gin.Context) {
	if !s.polarAssetsUseProvider {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider mode off; no signed download URL"})
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	ws := c.Query("workspace_id")
	if a == nil || !canCallerReadAsset(a, ws) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	provider, needsWarmPull, err := s.pickProviderForAsset(a.ID, s.regionOfWorkspace(ws))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "provider lookup failed"})
		return
	}
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no provider has this asset (yet)"})
		return
	}
	ttl := defaultAssetSignedURLTTL
	url, err := buildSignedBlobURL(provider.BaseURL, a.SHA256, ttl, []byte(provider.HMACToken))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if needsWarmPull {
		s.triggerAssetWarmPullBackground(a.ID, s.regionOfWorkspace(ws))
	}
	c.JSON(http.StatusOK, gin.H{
		"url":        url,
		"sha256":     a.SHA256,
		"size_bytes": a.SizeBytes,
		"mime":       a.Mime,
		"expires_at": time.Now().Add(ttl).UTC().Format(time.RFC3339),
	})
}

// ── writes: signed-PUT upload grant + finalize ──────────────────

type assetUploadGrantRequest struct {
	WorkspaceID string         `json:"workspace_id"`
	Kind        string         `json:"kind"`
	Name        string         `json:"name"`
	Version     string         `json:"version"`
	SHA256      string         `json:"sha256"`
	SizeBytes   int64          `json:"size_bytes"`
	Mime        string         `json:"mime"`
	Visibility  string         `json:"visibility"`
	Pinned      bool           `json:"pinned"`
	SourceURL   *string        `json:"source_url"`
	UploadedBy  string         `json:"uploaded_by"`
	Metadata    map[string]any `json:"metadata"`
}

// handleInternalAssetUploadGrant authorizes a direct client→provider
// upload. It ensures a catalog row for (workspace,kind,name,version),
// then — unless the bytes are already present — picks a provider and
// returns a scoped signed-PUT URL. The client PUTs the bytes straight
// to the provider and then calls /finalize. Bytes never touch dock.
func (s *Server) handleInternalAssetUploadGrant(c *gin.Context) {
	if !s.polarAssetsUseProvider {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "provider mode off; use through-dock upload"})
		return
	}
	var req assetUploadGrantRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	req.Kind = strings.TrimSpace(req.Kind)
	req.Name = strings.TrimSpace(req.Name)
	req.SHA256 = strings.ToLower(strings.TrimSpace(req.SHA256))
	if req.Kind == "" || req.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind, name required"})
		return
	}
	if !isHexSHA256(req.SHA256) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "sha256 must be 64 hex chars"})
		return
	}
	if req.SizeBytes <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "size_bytes must be > 0"})
		return
	}
	if req.Version == "" {
		req.Version = "v1"
	}
	if req.UploadedBy == "" {
		req.UploadedBy = "system"
	}
	// workspace_id == "" means a platform-owned asset (module binaries,
	// release artifacts, LLM weights): WorkspaceID NULL, no tenant quota,
	// default visibility public so it's distributable via signed URL.
	platform := req.WorkspaceID == ""
	var lookupWS *string
	if platform {
		if req.Visibility == "" {
			req.Visibility = "public"
		}
	} else {
		lookupWS = &req.WorkspaceID
		if req.Visibility == "" {
			req.Visibility = "workspace"
		}
		// checkAssetUploadAllowed returns 200 on success (NOT 0) and a 4xx/5xx
		// with a message on rejection — same convention the /api/assets and
		// asset_insert callers use. Comparing against 0 treated the 200-OK
		// allow as a rejection, aborting EVERY tenant (workspace-scoped)
		// asset upload with "200 {\"error\":\"\"}" — which the SDK then failed
		// on at the meta step. Platform assets (workspace_id=="") skip this
		// branch, which is why only tenant uploads (music/lawyer/expense) broke.
		if code, msg := s.checkAssetUploadAllowed(req.WorkspaceID, req.SizeBytes); code != http.StatusOK {
			c.JSON(code, gin.H{"error": msg})
			return
		}
	}

	// Ensure the catalog row exists for this logical key.
	existing, err := s.getAssetByName(lookupWS, req.Kind, req.Name, req.Version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	var assetID int64
	if existing != nil {
		if existing.SHA256 != req.SHA256 {
			c.JSON(http.StatusConflict, gin.H{
				"error": "name+version already exists with different content; bump version",
			})
			return
		}
		assetID = existing.ID
		// Bytes already confirmed somewhere? Then nothing to upload.
		if p, _, perr := s.pickProviderForAsset(assetID, ""); perr == nil && p != nil {
			c.JSON(http.StatusOK, gin.H{
				"asset_id": assetID,
				"sha256":   req.SHA256,
				"exists":   true,
			})
			return
		}
	} else {
		var meta []byte
		if len(req.Metadata) > 0 {
			meta, _ = json.Marshal(req.Metadata)
		}
		var wsPtr *string
		if !platform {
			ws := req.WorkspaceID
			wsPtr = &ws
		}
		a := &Asset{
			WorkspaceID: wsPtr,
			Kind:        req.Kind,
			Name:        req.Name,
			Version:     req.Version,
			SHA256:      req.SHA256,
			SizeBytes:   req.SizeBytes,
			Mime:        req.Mime,
			Visibility:  req.Visibility,
			SourceURL:   req.SourceURL,
			UploadedBy:  req.UploadedBy,
			Pinned:      req.Pinned,
			Metadata:    meta,
		}
		if _, err := s.insertAsset(a); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "insert asset: " + err.Error()})
			return
		}
		assetID = a.ID
	}

	provider, err := s.pickAnyHealthyProvider(s.regionOfWorkspace(req.WorkspaceID))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "provider lookup failed"})
		return
	}
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no healthy asset provider registered"})
		return
	}
	putURL, err := buildSignedPutURL(provider.BaseURL, req.SHA256, defaultAssetSignedURLTTL, req.SizeBytes, req.Mime, []byte(provider.HMACToken))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"asset_id":    assetID,
		"sha256":      req.SHA256,
		"exists":      false,
		"put_url":     putURL,
		"provider_id": provider.ID,
		"expires_at":  time.Now().Add(defaultAssetSignedURLTTL).UTC().Format(time.RFC3339),
	})
}

type assetFinalizeRequest struct {
	AssetID    int64 `json:"asset_id"`
	ProviderID int64 `json:"provider_id"`
}

// handleInternalAssetFinalize records that the bytes landed on the
// provider: flips the location to ready and bumps workspace usage —
// once. A re-sent finalize (location already ready) is a no-op so
// quota can't be double-counted.
func (s *Server) handleInternalAssetFinalize(c *gin.Context) {
	var req assetFinalizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	if req.AssetID <= 0 || req.ProviderID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "asset_id + provider_id required"})
		return
	}
	a, err := s.getAssetByID(req.AssetID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "asset not found"})
		return
	}
	prev, err := s.assetLocationStatus(req.AssetID, req.ProviderID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "server error"})
		return
	}
	if err := s.markAssetLocationReady(req.AssetID, req.ProviderID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mark ready failed"})
		return
	}
	// First time this (asset,provider) becomes ready → count the bytes.
	if prev != "ready" && a.WorkspaceID != nil {
		if err := s.bumpAssetUsage(*a.WorkspaceID, a.SizeBytes); err != nil {
			// Non-fatal: the blob is stored + addressable; usage drift
			// is recoverable by a recount sweep.
			c.Header("X-Quota-Bump", "failed")
		}
	}
	c.JSON(http.StatusOK, toAssetMetaDTO(a))
}
