package dock

// asset_handlers.go — HTTP catalog surface for the assets module.
//
//	POST   /api/assets                    multipart upload (workspace-scoped)
//	GET    /api/assets                    list (workspace + public-platform)
//	GET    /api/assets/:id                row only
//	GET    /api/assets/:id/blob           STUB: streams from local dir
//	GET    /api/assets/by-name/:kind/*name  convenience lookup
//	DELETE /api/assets/:id                delete row + blob file
//	GET    /api/assets/folders            path-prefix groupby for fake folders
//
// All routes go through AuthMiddleware. Workspace mismatch on
// download = 404 (don't leak existence across tenants). Platform-
// asset writes (workspace_id IS NULL) are refused in v1 — admin gate
// lands in P1b.
//
// Blob layout (stub):
//
//	<server.uploadDir>/assets/<workspace_id>/<sha256[:2]>/<sha256>
//
// For platform assets (workspace_id IS NULL) the path component is
// the literal "_platform".

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"
)

// Max upload size for v1: 5 GiB. Bigger comes when we wire chunked
// uploads in v2 (per design doc out-of-scope list).
const assetMaxUploadBytes int64 = 5 << 30

// validAssetKinds + validAssetVisibilities — keep in sync with the
// table's column comment.
var validAssetKinds = map[string]struct{}{
	"model": {}, "package": {}, "lib": {}, "media": {}, "app_data": {},
}
var validAssetVisibilities = map[string]struct{}{
	"private": {}, "workspace": {}, "link": {}, "public": {},
}

// ── routes ──────────────────────────────────────────────────────────

func (s *Server) handleAssetUpload(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	if s.uploadDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "upload dir not configured"})
		return
	}

	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 file 字段"})
		return
	}
	if file.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "文件为空"})
		return
	}
	if file.Size > assetMaxUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("文件超过 %d GiB 上限 (v1)", assetMaxUploadBytes/(1<<30)),
		})
		return
	}

	// Quota gate (workspace cap + root cap). Checked BEFORE we touch
	// the disk so a rejected upload doesn't leave staging cruft.
	if code, msg := s.checkAssetUploadAllowed(workspaceID, file.Size); code != http.StatusOK {
		c.JSON(code, gin.H{"error": msg})
		return
	}

	kind := strings.TrimSpace(c.PostForm("kind"))
	if _, ok := validAssetKinds[kind]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind 必填且必须是 model|package|lib|media|app_data"})
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name 必填"})
		return
	}
	version := strings.TrimSpace(c.PostForm("version"))
	if version == "" {
		version = "v1"
	}
	visibility := strings.TrimSpace(c.PostForm("visibility"))
	if visibility == "" {
		visibility = "private"
	}
	if _, ok := validAssetVisibilities[visibility]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "visibility 必须是 private|workspace|link|public"})
		return
	}
	mime := strings.TrimSpace(c.PostForm("mime"))
	if mime == "" {
		// Detect a bit if the operator didn't say. Default is
		// octet-stream which the store layer will fall back to.
		if ct := file.Header.Get("Content-Type"); ct != "" {
			mime = ct
		}
	}
	sourceURL := strings.TrimSpace(c.PostForm("source_url"))
	var sourceURLPtr *string
	if sourceURL != "" {
		sourceURLPtr = &sourceURL
	}
	var metaBytes []byte
	if raw := strings.TrimSpace(c.PostForm("metadata")); raw != "" {
		// Round-trip through json to validate it's parseable JSON.
		var probe any
		if err := json.Unmarshal([]byte(raw), &probe); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "metadata 必须是有效 JSON"})
			return
		}
		metaBytes = []byte(raw)
	}

	// Stage to a temp path; hash; move into content-addressed location.
	stageDir := filepath.Join(s.uploadDir, "assets-staging")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	stagePath := filepath.Join(stageDir, generateResourceID())
	if err := c.SaveUploadedFile(file, stagePath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件保存失败"})
		return
	}
	defer os.Remove(stagePath)

	sha, size, err := hashFileSHA256(stagePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "hash 失败"})
		return
	}
	if size != file.Size {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传大小不一致"})
		return
	}

	// Persist the blob. Two paths gated by the integration flag:
	//
	//   POLAR_ASSETS_USE_PROVIDER=true  → POST to a polar-assets-svc
	//                                     provider's /v1/receive; on
	//                                     success, write asset_locations.
	//   POLAR_ASSETS_USE_PROVIDER=false → keep the P1 local-FS stub
	//                                     (move stage → canonical path
	//                                     under uploadDir).
	//
	// The flag is the only difference; everything before (staging,
	// hashing) and after (asset row insert, quota bump) is identical.
	var provider *AssetProvider
	if s.polarAssetsUseProvider {
		provider, err = s.pickAnyHealthyProvider("")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "provider lookup failed"})
			return
		}
		if provider == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no healthy asset provider registered"})
			return
		}
		if err := s.uploadAssetToProvider(provider, sha, stagePath, file.Filename); err != nil {
			log.Printf("assets: uploadAssetToProvider provider=%s sha=%s err=%v", provider.Slug, sha, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "upstream provider rejected upload"})
			return
		}
	} else {
		// Move to canonical local path. If it already exists (same
		// sha somewhere else uploaded), reuse; identical content.
		dstPath := assetBlobPath(s.uploadDir, &workspaceID, sha)
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o700); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		}
		if _, err := os.Stat(dstPath); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(stagePath, dstPath); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "写入失败"})
				return
			}
		}
		// else: dedup hit; keep existing file, drop stage via defer.
	}

	asset := &Asset{
		WorkspaceID: &workspaceID,
		Kind:        kind,
		Name:        name,
		Version:     version,
		SHA256:      sha,
		SizeBytes:   size,
		Mime:        mime,
		Visibility:  visibility,
		SourceURL:   sourceURLPtr,
		UploadedBy:  userID,
		Metadata:    metaBytes,
	}
	if _, err := s.insertAsset(asset); err != nil {
		// pq unique-violation → 409 (caller already has the row).
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			existing, _ := s.getAssetByName(&workspaceID, kind, name, version)
			c.JSON(http.StatusConflict, gin.H{
				"error":    "asset with that (workspace, kind, name, version) already exists",
				"existing": existing,
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Bump cached usage_bytes. Best-effort: a recompute sweep will
	// reconcile any drift every 6h, so logging the error is enough.
	if err := s.bumpAssetUsage(workspaceID, size); err != nil {
		log.Printf("assets: bumpAssetUsage (+%d) workspace=%s failed: %v", size, workspaceID, err)
	}
	// Record provider location so the picker can route future downloads.
	if provider != nil {
		if err := s.markAssetLocationReady(asset.ID, provider.ID); err != nil {
			log.Printf("assets: markAssetLocationReady asset=%d provider=%d failed: %v", asset.ID, provider.ID, err)
		}
	}
	c.JSON(http.StatusCreated, gin.H{"asset": asset})
}

func (s *Server) handleAssetList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	filter := AssetListFilter{
		WorkspaceID:  &workspaceID,
		Kind:         strings.TrimSpace(c.Query("kind")),
		NamePrefix:   strings.TrimSpace(c.Query("name_prefix")),
		WantPlatform: true, // tenants always see public platform assets
	}
	if v, err := strconv.Atoi(c.Query("limit")); err == nil {
		filter.Limit = v
	}
	if v, err := strconv.Atoi(c.Query("offset")); err == nil {
		filter.Offset = v
	}
	rows, err := s.listAssets(filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"assets": rows})
}

func (s *Server) handleAssetGet(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if a == nil || !canCallerReadAsset(a, workspaceID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"asset": a})
}

func (s *Server) handleAssetGetByName(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	kind := strings.TrimSpace(c.Param("kind"))
	name := strings.TrimPrefix(c.Param("name"), "/")
	if kind == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "kind + name required"})
		return
	}
	version := strings.TrimSpace(c.Query("version"))
	if version == "" {
		version = "v1"
	}
	// Try caller's workspace first; fall back to platform if caller
	// asks for ?scope=platform.
	scope := c.Query("scope")
	var lookupWS *string
	if scope == "platform" {
		lookupWS = nil
	} else {
		lookupWS = &workspaceID
	}
	a, err := s.getAssetByName(lookupWS, kind, name, version)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if a == nil || !canCallerReadAsset(a, workspaceID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"asset": a})
}

// handleAssetBlob serves the blob. With POLAR_ASSETS_USE_PROVIDER=true
// it issues a 302 to a polar-assets-svc provider's /v1/blob endpoint
// with a short-lived HMAC-signed token. With the flag off (default)
// it streams from the local uploadDir stub (the P1 behavior).
func (s *Server) handleAssetBlob(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if a == nil || !canCallerReadAsset(a, workspaceID) {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if s.polarAssetsUseProvider {
		s.redirectToProviderBlob(c, a, s.regionOfWorkspace(workspaceID))
		return
	}
	path := assetBlobPath(s.uploadDir, a.WorkspaceID, a.SHA256)
	f, err := os.Open(path)
	if err != nil {
		c.JSON(http.StatusGone, gin.H{"error": "blob missing on disk (catalog row orphaned)"})
		return
	}
	defer f.Close()
	c.Header("Content-Type", a.Mime)
	c.Header("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	c.Header("X-Asset-SHA256", a.SHA256)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

// redirectToProviderBlob picks a provider holding the asset, mints a
// signed URL, and 302s the caller. Used by both /api/assets/:id/blob
// and the public /s/:code paths. On lookup miss → 503; on signing
// failure → 500.
//
// callerRegion is plumbed from teams.region (via regionOfWorkspace).
// Empty string means "unknown" — picker treats as any-region; no
// warm-pull. When callerRegion is set but no in-region provider has
// the asset, the picker signals needsWarmPull and we fire-and-forget
// a P4c warm-pull onto a same-region target so the next request is
// local.
func (s *Server) redirectToProviderBlob(c *gin.Context, a *Asset, callerRegion string) {
	provider, needsWarmPull, err := s.pickProviderForAsset(a.ID, callerRegion)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "provider lookup failed"})
		return
	}
	if provider == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no provider has this asset (yet)"})
		return
	}
	url, err := buildSignedBlobURL(provider.BaseURL, a.SHA256, defaultAssetSignedURLTTL, []byte(provider.HMACToken))
	if err != nil {
		log.Printf("assets: signed URL build failed asset=%d: %v", a.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.Header("X-Asset-SHA256", a.SHA256)
	c.Redirect(http.StatusFound, url)
	if needsWarmPull {
		s.triggerAssetWarmPullBackground(a.ID, callerRegion)
	}
}

func (s *Server) handleAssetDelete(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id invalid"})
		return
	}
	a, err := s.getAssetByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if a == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	// Tenants can only delete their own. Platform-asset delete is
	// admin-only; that gate ships in P1b. For now we refuse any
	// platform delete with 403.
	if a.WorkspaceID == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "platform asset delete is admin-only (P1b)"})
		return
	}
	if *a.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := s.deleteAsset(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Best-effort blob removal. If another asset shares this sha
	// (dedup), it'll re-upload-and-write on next access; we don't
	// reference-count here because P3 owns blob storage.
	_ = os.Remove(assetBlobPath(s.uploadDir, a.WorkspaceID, a.SHA256))
	// Bump cached usage_bytes downward. Same best-effort posture
	// as the upload bump.
	if a.WorkspaceID != nil {
		if err := s.bumpAssetUsage(*a.WorkspaceID, -a.SizeBytes); err != nil {
			log.Printf("assets: bumpAssetUsage (-%d) workspace=%s failed: %v", a.SizeBytes, *a.WorkspaceID, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleAssetFolders(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	prefix := c.Query("prefix")
	entries, err := s.listAssetFolders(&workspaceID, prefix)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entries": entries})
}

// ── helpers ─────────────────────────────────────────────────────────

// canCallerReadAsset gates GET access. Caller workspace_id matches OR
// the asset is platform-owned + public. Private platform assets are
// admin-only — that gate lives in the handler chain (P1b).
func canCallerReadAsset(a *Asset, callerWorkspace string) bool {
	if a.WorkspaceID == nil {
		return a.Visibility == "public"
	}
	return *a.WorkspaceID == callerWorkspace
}

// assetBlobPath builds the on-disk path for a blob. workspace_id nil
// becomes the literal "_platform" so we can tell platform / tenant
// blobs apart at the filesystem layer.
func assetBlobPath(uploadDir string, workspaceID *string, sha string) string {
	wsSegment := "_platform"
	if workspaceID != nil && *workspaceID != "" {
		wsSegment = *workspaceID
	}
	if len(sha) < 2 {
		return filepath.Join(uploadDir, "assets", wsSegment, "_short", sha)
	}
	return filepath.Join(uploadDir, "assets", wsSegment, sha[:2], sha)
}

// hashFileSHA256 hashes a file in 64 KiB chunks. Returns hex digest
// and total size.
func hashFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
