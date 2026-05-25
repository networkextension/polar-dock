package dock

// HTTP surface for the workspace-scoped attachments service.
//
//   POST   /api/attachments              multipart upload (field: file)
//   GET    /api/attachments/:id          metadata (rare; download is the
//                                        common path)
//   GET    /api/attachments/:id/download streams bytes (or 302 to remote)
//   DELETE /api/attachments/:id          uploader-only soft delete
//
// All routes go through AuthMiddleware (user_id + workspace_id in
// gin context). Workspace mismatch on download = 404 (don't leak
// existence of attachments belonging to other workspaces).

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAttachmentUpload(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
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
	if file.Size > attachmentMaxSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": fmt.Sprintf("文件超过 %d MiB 上限", attachmentMaxSize/(1024*1024)),
		})
		return
	}
	mimeType, allowed := isAllowedAttachmentExtension(file.Filename)
	if !allowed {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "仅支持 zip / pdf / doc / docx / ppt / pptx",
		})
		return
	}

	// Save to a staging path first so we can hash + dedup before
	// committing to the canonical content-addressed location.
	stagePath := filepath.Join(s.uploadDir, "attachments-staging-"+generateResourceID()+filepath.Ext(file.Filename))
	if err := os.MkdirAll(filepath.Dir(stagePath), 0o755); err != nil {
		log.Printf("attachment-upload: mkdir uploadDir=%s failed: %v", s.uploadDir, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误: " + err.Error()})
		return
	}
	if err := c.SaveUploadedFile(file, stagePath); err != nil {
		log.Printf("attachment-upload: SaveUploadedFile name=%q size=%d dst=%s failed: %v", file.Filename, file.Size, stagePath, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件保存失败: " + err.Error()})
		return
	}
	storageRef, sha, err := s.hashAndStoreAttachmentFile(workspaceID, stagePath)
	if err != nil {
		log.Printf("attachment-upload: hashAndStore workspace=%s stage=%s failed: %v", workspaceID, stagePath, err)
		_ = os.Remove(stagePath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "文件写入失败: " + err.Error()})
		return
	}

	// Dedup: same workspace + same sha → reuse existing row (the
	// uploader_user_id stays as the original uploader, but we
	// record this user added it through the link side).
	if existing, err := s.findAttachmentBySha(workspaceID, sha); err == nil && existing != nil {
		existing.DownloadURL = existing.attachmentDownloadURL()
		c.JSON(http.StatusOK, gin.H{"attachment": existing, "deduped": true})
		return
	}

	att := &Attachment{
		WorkspaceID:    workspaceID,
		UploaderUserID: userID,
		Filename:       file.Filename,
		MimeType:       mimeType,
		SizeBytes:      file.Size,
		StorageKind:    AttachmentStorageLocalFS,
		StorageRef:     storageRef,
		Sha256:         sha,
	}
	if err := s.insertAttachment(att); err != nil {
		log.Printf("attachment-upload: insertAttachment workspace=%s sha=%s name=%q failed: %v", workspaceID, sha, file.Filename, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误: " + err.Error()})
		return
	}
	att.DownloadURL = att.attachmentDownloadURL()
	c.JSON(http.StatusCreated, gin.H{"attachment": att})
}

func (s *Server) handleAttachmentGet(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	att, err := s.loadAttachment(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if att == nil || att.DeletedAt != nil || att.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "附件不存在"})
		return
	}
	att.DownloadURL = att.attachmentDownloadURL()
	c.JSON(http.StatusOK, gin.H{"attachment": att})
}

func (s *Server) handleAttachmentDownload(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	att, err := s.loadAttachment(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if att == nil || att.DeletedAt != nil || att.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "附件不存在"})
		return
	}
	if att.StorageKind == AttachmentStorageRemote {
		c.Redirect(http.StatusFound, att.StorageRef)
		return
	}
	abs := s.attachmentDiskPath(*att)
	if abs == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "存储路径无效"})
		return
	}
	if _, err := os.Stat(abs); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "文件已不存在"})
		return
	}
	// RFC 5987 filename* for non-ASCII (Chinese) names.
	disposition := fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(att.Filename))
	c.Header("Content-Disposition", disposition)
	if att.MimeType != "" {
		c.Header("Content-Type", att.MimeType)
	}
	c.File(abs)
}

func (s *Server) handleAttachmentDelete(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if err := s.softDeleteAttachment(id, userID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "附件不存在或不属于你"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- helpers used by post / chat handlers --------------------------

// validateAttachmentIDsForWorkspace ensures every requested
// attachment exists, isn't deleted, and belongs to the active
// workspace. Returns the resolved Attachment slice for further use
// (like building a response).
func (s *Server) validateAttachmentIDsForWorkspace(ids []string, workspaceID string) ([]Attachment, error) {
	out := make([]Attachment, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		att, err := s.loadAttachment(id)
		if err != nil {
			return nil, err
		}
		if att == nil || att.DeletedAt != nil {
			return nil, fmt.Errorf("attachment %s not found", id)
		}
		if att.WorkspaceID != workspaceID {
			return nil, fmt.Errorf("attachment %s belongs to another workspace", id)
		}
		out = append(out, *att)
	}
	return out, nil
}

// touchAttachmentTime is reserved for future use (last-accessed
// tracking). Kept so the imports settle.
var _ = time.Now
