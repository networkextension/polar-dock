package dock

// post_media.go — post images/videos served from the assets module
// instead of the legacy /uploads/ static path. Cutover (no dual-write):
//
//   upload  → register each variant as a PUBLIC asset; the post_images /
//             post_videos URL columns hold a stable /api/media/<id> URL.
//   serve   → GET /api/media/:id (public, no auth) 302s to a fresh signed
//             provider URL, so bytes flow browser↔provider, bypassing dock.
//   migrate → POST /api/admin/assets/migrate-post-media backfills the
//             existing /uploads/-backed rows into assets + sets asset_id.
//
// Why a dedicated public route (not /api/assets/:id/blob): post media is
// public content shown in the feed, and the stored URL must be permanent
// while the signed provider redirect is short-lived. /api/media/:id is the
// permanent handle; it only ever serves assets with visibility='public'.

import (
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// mediaPublicURL is the permanent, frontend-facing URL for a post-media
// asset. Resolves (via nginx /api → dock) to handlePublicMedia.
func mediaPublicURL(assetID int64) string {
	return "/api/media/" + strconv.FormatInt(assetID, 10)
}

// mimeFromPath guesses a content-type from the file extension, defaulting
// to octet-stream. Used so the public route serves a correct Content-Type.
func mimeFromPath(path string) string {
	if m := mime.TypeByExtension(filepath.Ext(path)); m != "" {
		return m
	}
	return "application/octet-stream"
}

// registerPostMediaAsset registers the file at srcPath as a PUBLIC asset
// (kind = post-image|post-video), named posts/<postID>/<basename> so the
// catalog browses cleanly. Idempotent: a re-register of the same
// (workspace,kind,name) returns the existing asset rather than erroring,
// so the backfill can be safely re-run.
func (s *Server) registerPostMediaAsset(workspaceID, userID string, postID int64, srcPath, kind string) (*Asset, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("registerPostMediaAsset: empty workspace_id (post %d)", postID)
	}
	name := fmt.Sprintf("posts/%d/%s", postID, filepath.Base(srcPath))
	return s.registerPostMediaAssetNamed(workspaceID, userID, name, srcPath, kind, map[string]any{"post_id": postID})
}

// registerPostMediaAssetNamed is registerPostMediaAsset with an explicit
// asset name + metadata — the eager block-editor upload registers drafts
// under posts/drafts/<mediaID>/ before any post exists.
func (s *Server) registerPostMediaAssetNamed(workspaceID, userID, name, srcPath, kind string, metadata map[string]any) (*Asset, error) {
	if workspaceID == "" {
		return nil, fmt.Errorf("registerPostMediaAssetNamed: empty workspace_id (%s)", name)
	}
	a, err := s.insertAssetFromFile(AssetInsertInput{
		WorkspaceID: workspaceID,
		Kind:        kind,
		Name:        name,
		Version:     "v1",
		Visibility:  "public",
		Mime:        mimeFromPath(srcPath),
		SrcPath:     srcPath,
		UploadedBy:  userID,
		Metadata:    metadata,
	})
	if err == nil {
		return a, nil
	}
	if strings.Contains(err.Error(), "asset_exists") {
		ws := workspaceID
		return s.getAssetByName(&ws, kind, name, "v1")
	}
	return nil, err
}

// postMediaMaxImageSize caps eager inline-image uploads from the block
// editor. Post create historically has no per-image cap; new surface,
// new bound. Chat images cap at 10 MiB — posts allow originals.
const postMediaMaxImageSize = 20 << 20

// handlePostMediaUpload is the eager upload endpoint for the block
// editor (POST /api/posts/media, field "file"): the image is uploaded
// while composing, thumbnailed like a post image, registered as a
// draft asset (posts/drafts/<mediaID>/…), and the permanent
// /api/media/<id> URLs are returned for inline :::figure/:::gallery
// markdown. The post referencing the URLs is created later; unreferenced
// drafts are the orphan-GC target (C-P1, Plane POLARDOCK #15).
func (s *Server) handlePostMediaUpload(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	if s.uploadDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传目录未配置"})
		return
	}

	file, err := c.FormFile("file")
	if err != nil || file == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少文件"})
		return
	}
	if !isUploadType(file, "image/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "只支持图片文件"})
		return
	}
	if file.Size <= 0 || file.Size > postMediaMaxImageSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "图片超过大小限制 (20MB)"})
		return
	}

	filename := buildUploadFilename(file.Filename)
	dstPath := filepath.Join(s.uploadDir, filename)
	if err := c.SaveUploadedFile(file, dstPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存文件失败"})
		return
	}

	publicURL := "/uploads/" + filename
	imageItem, _, err := processUploadedPostImage(s.uploadDir, dstPath, publicURL, filename)
	if err != nil {
		log.Printf("process post media failed for %s: %v", dstPath, err)
	}
	if imageItem.OriginalURL == "" {
		imageItem = normalizePostImageItem(publicURL, "", "")
	}

	// Register each variant as a draft asset; on failure the variant
	// keeps its /uploads/ URL (same never-hard-fail posture as posts).
	mediaID := generateResourceID()
	cutover := func(u string) string {
		src, isUpload := s.localPathForUploadsURL(u)
		if !isUpload {
			return u
		}
		name := "posts/drafts/" + mediaID + "/" + filepath.Base(src)
		a, cerr := s.registerPostMediaAssetNamed(workspaceID, userID, name, src, "post-image",
			map[string]any{"draft": true, "media_id": mediaID})
		if cerr != nil || a == nil {
			log.Printf("post-media draft cutover %s: %v", u, cerr)
			return u
		}
		return mediaPublicURL(a.ID)
	}
	imageItem.OriginalURL = cutover(imageItem.OriginalURL)
	imageItem.MediumURL = cutover(imageItem.MediumURL)
	imageItem.SmallURL = cutover(imageItem.SmallURL)

	c.JSON(http.StatusCreated, gin.H{
		"media_id":   mediaID,
		"url":        imageItem.OriginalURL,
		"medium_url": imageItem.MediumURL,
		"small_url":  imageItem.SmallURL,
	})
}

// handlePublicMedia serves a public post-media asset: 302 to a fresh
// signed provider URL (provider mode) or stream from local FS (stub mode).
// Only serves assets with visibility='public' so it can never leak a
// private/workspace asset.
func (s *Server) handlePublicMedia(c *gin.Context) {
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
	if a == nil || a.Visibility != "public" {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if s.polarAssetsUseProvider {
		region := ""
		if a.WorkspaceID != nil {
			region = s.regionOfWorkspace(*a.WorkspaceID)
		}
		s.redirectToProviderBlob(c, a, region)
		return
	}
	// Local-FS stub fallback.
	path := assetBlobPath(s.uploadDir, a.WorkspaceID, a.SHA256)
	f, err := os.Open(path)
	if err != nil {
		c.JSON(http.StatusGone, gin.H{"error": "blob missing"})
		return
	}
	defer f.Close()
	c.Header("Content-Type", a.Mime)
	c.Header("Content-Length", strconv.FormatInt(a.SizeBytes, 10))
	c.Header("Cache-Control", "public, max-age=86400")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, f)
}

// localPathForUploadsURL maps a "/uploads/<file>" URL to its on-disk path
// under uploadDir, or returns ("", false) if the URL isn't a /uploads/ ref.
func (s *Server) localPathForUploadsURL(u string) (string, bool) {
	if !strings.HasPrefix(u, "/uploads/") {
		return "", false
	}
	base := filepath.Base(strings.TrimPrefix(u, "/uploads/"))
	if base == "" || base == "." || base == "/" {
		return "", false
	}
	return filepath.Join(s.uploadDir, base), true
}

// cutoverUploadsURL is the shared primitive for both the live upload path
// and the backfill: given a "/uploads/<file>" URL, it registers the
// on-disk file as a public asset and returns the permanent /api/media/<id>
// URL + the asset. Returns the URL unchanged (asset nil, err nil) when it
// isn't a /uploads ref (already migrated / external). Returns an error
// (URL unchanged) when the file is missing or registration fails, so the
// caller keeps a serviceable legacy URL rather than breaking the row.
func (s *Server) cutoverUploadsURL(workspaceID, userID string, postID int64, url, kind string) (string, *Asset, error) {
	src, ok := s.localPathForUploadsURL(url)
	if !ok {
		return url, nil, nil
	}
	if _, err := os.Stat(src); err != nil {
		return url, nil, fmt.Errorf("file missing: %s", url)
	}
	a, err := s.registerPostMediaAsset(workspaceID, userID, postID, src, kind)
	if err != nil || a == nil {
		return url, nil, fmt.Errorf("register %s: %v", url, err)
	}
	return mediaPublicURL(a.ID), a, nil
}

// handleMigratePostMedia is the one-time backfill (admin). For every
// post_images / post_videos row still pointing at /uploads/, it registers
// each on-disk variant as a public asset and rewrites the URL columns to
// /api/media/<id> (+ post_images.asset_id = original variant). Idempotent
// and safe to re-run; rows whose files are missing or whose post has no
// workspace are skipped + counted.
func (s *Server) handleMigratePostMedia(c *gin.Context) {
	var imgRows, imgFiles, imgSkipped, vidRows, vidFiles int
	var errs []string

	// migrate one /uploads/ URL → (newURL, asset, migrated?), recording
	// any error. On failure returns the URL unchanged (row stays usable).
	migrate := func(ws, userID string, postID int64, url, kind string) (string, *Asset, bool) {
		newURL, a, err := s.cutoverUploadsURL(ws, userID, postID, url, kind)
		if err != nil {
			errs = append(errs, fmt.Sprintf("post %d: %v", postID, err))
			return url, nil, false
		}
		return newURL, a, a != nil
	}

	// ── post_images ──
	type imgRow struct {
		id, postID                  int64
		file, small, medium, ws, uid string
	}
	rows, err := s.db.Query(`
		SELECT pi.id, pi.post_id, pi.file_url, pi.small_url, pi.medium_url,
		       COALESCE(p.workspace_id,''), p.user_id
		  FROM post_images pi JOIN posts p ON p.id = pi.post_id
		 WHERE pi.file_url LIKE '/uploads/%'`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query post_images: " + err.Error()})
		return
	}
	var imgs []imgRow
	for rows.Next() {
		var r imgRow
		if err := rows.Scan(&r.id, &r.postID, &r.file, &r.small, &r.medium, &r.ws, &r.uid); err != nil {
			rows.Close()
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan: " + err.Error()})
			return
		}
		imgs = append(imgs, r)
	}
	rows.Close()
	for _, r := range imgs {
		if r.ws == "" {
			imgSkipped++
			errs = append(errs, fmt.Sprintf("post_image %d: post has no workspace_id, skipped", r.id))
			continue
		}
		newFile, fa, okF := migrate(r.ws, r.uid, r.postID, r.file, "post-image")
		newSmall, _, okS := migrate(r.ws, r.uid, r.postID, r.small, "post-image")
		newMed, _, okM := migrate(r.ws, r.uid, r.postID, r.medium, "post-image")
		if !okF && !okS && !okM {
			continue
		}
		for _, ok := range []bool{okF, okS, okM} {
			if ok {
				imgFiles++
			}
		}
		var assetID *int64
		if fa != nil {
			assetID = &fa.ID
		}
		if _, err := s.db.Exec(
			`UPDATE post_images SET file_url=$1, small_url=$2, medium_url=$3, asset_id=$4 WHERE id=$5`,
			newFile, newSmall, newMed, assetID, r.id); err != nil {
			errs = append(errs, fmt.Sprintf("post_image %d: update: %v", r.id, err))
			continue
		}
		imgRows++
	}

	// ── post_videos (no asset_id column; just rewrite URLs) ──
	type vidRow struct {
		id, postID         int64
		file, poster, ws, uid string
	}
	vrows, err := s.db.Query(`
		SELECT pv.id, pv.post_id, pv.file_url, pv.poster_url,
		       COALESCE(p.workspace_id,''), p.user_id
		  FROM post_videos pv JOIN posts p ON p.id = pv.post_id
		 WHERE pv.file_url LIKE '/uploads/%' OR pv.poster_url LIKE '/uploads/%'`)
	if err == nil {
		var vids []vidRow
		for vrows.Next() {
			var r vidRow
			if err := vrows.Scan(&r.id, &r.postID, &r.file, &r.poster, &r.ws, &r.uid); err == nil {
				vids = append(vids, r)
			}
		}
		vrows.Close()
		for _, r := range vids {
			if r.ws == "" {
				continue
			}
			newFile, _, okF := migrate(r.ws, r.uid, r.postID, r.file, "post-video")
			newPoster, _, okP := migrate(r.ws, r.uid, r.postID, r.poster, "post-video")
			if !okF && !okP {
				continue
			}
			if okF {
				vidFiles++
			}
			if okP {
				vidFiles++
			}
			if _, err := s.db.Exec(`UPDATE post_videos SET file_url=$1, poster_url=$2 WHERE id=$3`,
				newFile, newPoster, r.id); err != nil {
				errs = append(errs, fmt.Sprintf("post_video %d: update: %v", r.id, err))
				continue
			}
			vidRows++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"post_images_migrated": imgRows,
		"post_image_files":     imgFiles,
		"post_images_skipped":  imgSkipped,
		"post_videos_migrated": vidRows,
		"post_video_files":     vidFiles,
		"errors":               errs,
	})
}
