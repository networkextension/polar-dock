package dock

// Workspace-scoped, content-addressed file uploads. Backs posts +
// chat_messages today via junction tables (post_attachments,
// chat_message_attachments).
//
// Storage layout:
//   <UploadDir>/attachments/<workspace_id>/<sha256[:2]>/<sha256>
//
// Filename + extension are preserved on Attachment row (filename
// field) for display + Content-Disposition; on disk we use the
// hash so identical bytes uploaded twice in the same workspace
// don't double-store (UNIQUE INDEX on (workspace_id, sha256)).
//
// Migrated rows (from chat_messages.attachment JSON) keep
// storage_kind='local_fs' but storage_ref points to the original
// /uploads/<filename> path — no migration of bytes, just the
// metadata moves into the table.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	AttachmentStorageLocalFS = "local_fs"
	AttachmentStorageRemote  = "remote"

	// Kept in sync with chatAttachmentMaxFileSize (chat_handlers.go).
	// 200 MiB covers RE archives + textbook PDFs (e.g. OS Internals Vol 2)
	// without making the upload UX painful. nginx vhost must be at
	// least client_max_body_size 200m — see scripts/nginx/dock-vhost-snippet.conf.
	attachmentMaxSize = 200 * 1024 * 1024 // 200 MiB per file
)

// allowedAttachmentExtensions is the whitelist for the new
// /api/attachments upload endpoint. Doc-style files only — chat
// images use a separate path (postUploadImage / chatStorage)
// because they need thumbnail/resize processing.
var allowedAttachmentExtensions = map[string]string{
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".zip":  "application/zip",
}

type Attachment struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	UploaderUserID string     `json:"uploader_user_id"`
	Filename       string     `json:"filename"`
	MimeType       string     `json:"mime_type"`
	SizeBytes      int64      `json:"size_bytes"`
	StorageKind    string     `json:"storage_kind"`
	StorageRef     string     `json:"storage_ref"`
	Sha256         string     `json:"sha256,omitempty"`
	Width          int        `json:"width,omitempty"`
	Height         int        `json:"height,omitempty"`
	ThumbnailURL   string     `json:"thumbnail_url,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	// DownloadURL is computed at read time, not stored. For local
	// attachments it's /api/attachments/<id>/download (auth-gated);
	// for remote it's the absolute URL pass-through.
	DownloadURL string `json:"download_url,omitempty"`
}

func attachmentExtensionFromFilename(filename string) string {
	return strings.ToLower(filepath.Ext(filename))
}

func isAllowedAttachmentExtension(filename string) (string, bool) {
	ext := attachmentExtensionFromFilename(filename)
	mimeType, ok := allowedAttachmentExtensions[ext]
	return mimeType, ok
}

// hashAndStoreAttachmentFile reads the source file, computes its
// sha256, and moves it into the canonical content-addressed
// location under UploadDir. Returns the relative storage_ref and
// hex-encoded sha. Source file is removed on success.
func (s *Server) hashAndStoreAttachmentFile(workspaceID, sourcePath string) (string, string, error) {
	src, err := os.Open(sourcePath)
	if err != nil {
		return "", "", err
	}
	defer src.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, src); err != nil {
		return "", "", err
	}
	sum := hex.EncodeToString(hasher.Sum(nil))
	rel := filepath.Join("attachments", workspaceID, sum[:2], sum)
	abs := filepath.Join(s.uploadDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(abs); err == nil {
		// Already stored (dedup hit) — just remove the source.
		_ = os.Remove(sourcePath)
		return rel, sum, nil
	}
	if err := os.Rename(sourcePath, abs); err != nil {
		// Cross-device rename can fail on some setups (e.g. /tmp on
		// a different fs). Fall back to copy-then-remove.
		if copyErr := copyFileForAttachment(sourcePath, abs); copyErr != nil {
			return "", "", copyErr
		}
		_ = os.Remove(sourcePath)
	}
	return rel, sum, nil
}

func copyFileForAttachment(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// findAttachmentBySha returns the existing attachment with the
// given (workspace, sha) pair so callers can reuse it instead of
// inserting a duplicate. Returns nil/nil if not present.
func (s *Server) findAttachmentBySha(workspaceID, sha string) (*Attachment, error) {
	if workspaceID == "" || sha == "" {
		return nil, nil
	}
	row := s.db.QueryRow(
		`SELECT id, workspace_id, uploader_user_id, filename, mime_type, size_bytes, storage_kind, storage_ref, sha256, width, height, thumbnail_url, created_at, deleted_at
		   FROM attachments
		  WHERE workspace_id = $1 AND sha256 = $2 AND deleted_at IS NULL`,
		workspaceID, sha,
	)
	return scanAttachment(row.Scan)
}

func (s *Server) insertAttachment(a *Attachment) error {
	if a.ID == "" {
		a.ID = generateResourceID()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.StorageKind == "" {
		a.StorageKind = AttachmentStorageLocalFS
	}
	var sha *string
	if a.Sha256 != "" {
		v := a.Sha256
		sha = &v
	}
	var width, height *int
	if a.Width > 0 {
		v := a.Width
		width = &v
	}
	if a.Height > 0 {
		v := a.Height
		height = &v
	}
	var thumb *string
	if a.ThumbnailURL != "" {
		v := a.ThumbnailURL
		thumb = &v
	}
	_, err := s.db.Exec(
		`INSERT INTO attachments
		  (id, workspace_id, uploader_user_id, filename, mime_type, size_bytes,
		   storage_kind, storage_ref, sha256, width, height, thumbnail_url, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		a.ID, a.WorkspaceID, a.UploaderUserID, a.Filename, a.MimeType, a.SizeBytes,
		a.StorageKind, a.StorageRef, sha, width, height, thumb, a.CreatedAt,
	)
	return err
}

func (s *Server) loadAttachment(id string) (*Attachment, error) {
	row := s.db.QueryRow(
		`SELECT id, workspace_id, uploader_user_id, filename, mime_type, size_bytes, storage_kind, storage_ref, sha256, width, height, thumbnail_url, created_at, deleted_at
		   FROM attachments WHERE id = $1`,
		id,
	)
	return scanAttachment(row.Scan)
}

func (s *Server) softDeleteAttachment(id, requesterUserID string) error {
	res, err := s.db.Exec(
		`UPDATE attachments SET deleted_at = $1 WHERE id = $2 AND uploader_user_id = $3 AND deleted_at IS NULL`,
		time.Now().UTC(), id, requesterUserID,
	)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// listAttachmentsForPost returns the attachments linked to a post,
// ordered by ord. Soft-deleted attachments are filtered out.
func (s *Server) listAttachmentsForPost(postID int64) ([]Attachment, error) {
	return s.listAttachmentsByJoin(
		`SELECT a.id, a.workspace_id, a.uploader_user_id, a.filename, a.mime_type, a.size_bytes,
		        a.storage_kind, a.storage_ref, a.sha256, a.width, a.height, a.thumbnail_url, a.created_at, a.deleted_at
		   FROM post_attachments pa
		   JOIN attachments a ON a.id = pa.attachment_id
		  WHERE pa.post_id = $1 AND a.deleted_at IS NULL
		  ORDER BY pa.ord ASC, a.created_at ASC`,
		postID,
	)
}

func (s *Server) listAttachmentsForChatMessage(messageID int64) ([]Attachment, error) {
	return s.listAttachmentsByJoin(
		`SELECT a.id, a.workspace_id, a.uploader_user_id, a.filename, a.mime_type, a.size_bytes,
		        a.storage_kind, a.storage_ref, a.sha256, a.width, a.height, a.thumbnail_url, a.created_at, a.deleted_at
		   FROM chat_message_attachments cma
		   JOIN attachments a ON a.id = cma.attachment_id
		  WHERE cma.message_id = $1 AND a.deleted_at IS NULL
		  ORDER BY cma.ord ASC, a.created_at ASC`,
		messageID,
	)
}

func (s *Server) listAttachmentsByJoin(query string, arg any) ([]Attachment, error) {
	rows, err := s.db.Query(query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows.Scan)
		if err != nil {
			return nil, err
		}
		if a == nil {
			continue
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

func scanAttachment(scanFn func(...any) error) (*Attachment, error) {
	var a Attachment
	var sha, thumb sql.NullString
	var width, height sql.NullInt64
	var deletedAt sql.NullTime
	err := scanFn(
		&a.ID, &a.WorkspaceID, &a.UploaderUserID, &a.Filename, &a.MimeType, &a.SizeBytes,
		&a.StorageKind, &a.StorageRef, &sha, &width, &height, &thumb, &a.CreatedAt, &deletedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if sha.Valid {
		a.Sha256 = sha.String
	}
	if thumb.Valid {
		a.ThumbnailURL = thumb.String
	}
	if width.Valid {
		a.Width = int(width.Int64)
	}
	if height.Valid {
		a.Height = int(height.Int64)
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		a.DeletedAt = &t
	}
	return &a, nil
}

// linkAttachmentsToPost sets the full attachment list for a post,
// replacing any existing links. Verifies all attachments belong to
// the same workspace as the post (enforced at handler level too).
func (s *Server) linkAttachmentsToPost(postID int64, attachmentIDs []string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM post_attachments WHERE post_id = $1`, postID); err != nil {
		return err
	}
	for i, id := range attachmentIDs {
		if id == "" {
			continue
		}
		if _, err := tx.Exec(
			`INSERT INTO post_attachments (post_id, attachment_id, ord) VALUES ($1, $2, $3)
			 ON CONFLICT (post_id, attachment_id) DO UPDATE SET ord = EXCLUDED.ord`,
			postID, id, i,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// linkAttachmentToChatMessage links a single attachment to a chat
// message. Most chat messages have at most one attachment today.
func (s *Server) linkAttachmentToChatMessage(messageID int64, attachmentID string) error {
	if attachmentID == "" {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO chat_message_attachments (message_id, attachment_id, ord) VALUES ($1, $2, 0)
		 ON CONFLICT (message_id, attachment_id) DO NOTHING`,
		messageID, attachmentID,
	)
	return err
}

// attachmentDownloadURL returns the URL to use in API responses
// (for the FE to fetch the file). Local attachments go through the
// auth-gated /api/attachments/:id/download; remote ones are
// passthrough.
func (a Attachment) attachmentDownloadURL() string {
	if a.StorageKind == AttachmentStorageRemote {
		return a.StorageRef
	}
	return "/api/attachments/" + a.ID + "/download"
}

// resolveDownloadURLs is a list-flavored mutation helper: stamps
// DownloadURL onto each row before returning to the API caller.
func resolveDownloadURLs(items []Attachment) []Attachment {
	for i := range items {
		items[i].DownloadURL = items[i].attachmentDownloadURL()
	}
	return items
}

func mimeFromFilenameFallback(filename string) string {
	ext := attachmentExtensionFromFilename(filename)
	if m, ok := allowedAttachmentExtensions[ext]; ok {
		return m
	}
	if m := mime.TypeByExtension(ext); m != "" {
		return m
	}
	return "application/octet-stream"
}

// attachmentDiskPath returns the absolute filesystem path for a
// local_fs attachment. For remote attachments returns "" (caller
// should redirect rather than read from disk).
func (s *Server) attachmentDiskPath(a Attachment) string {
	if a.StorageKind != AttachmentStorageLocalFS {
		return ""
	}
	ref := a.StorageRef
	// Tolerate both "/uploads/foo" (migrated chat rows) and
	// "attachments/<ws>/<sha2>/<sha>" (new uploads).
	if strings.HasPrefix(ref, "/uploads/") {
		ref = strings.TrimPrefix(ref, "/uploads/")
	}
	return filepath.Join(s.uploadDir, ref)
}

// migrateChatMessageAttachments handles the legacy
// chat_messages.attachment JSONB column: for every row that still
// has a value, INSERT an attachments row and link it via
// chat_message_attachments, then DROP COLUMN. Idempotent — if the
// column doesn't exist (fresh DB or already-migrated DB) the
// function no-ops cleanly.
//
// Workspace ownership for migrated rows = sender's personal team.
// Storage stays at the original /uploads/<filename> path; we
// don't move bytes during migration.
func (s *Server) migrateChatMessageAttachments() error {
	var exists bool
	if err := s.db.QueryRow(
		`SELECT EXISTS (
		    SELECT 1 FROM information_schema.columns
		    WHERE table_name = 'chat_messages' AND column_name = 'attachment'
		 )`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("probe attachment column: %w", err)
	}
	if !exists {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT m.id, m.sender_id, m.attachment, m.created_at
		   FROM chat_messages m
		  WHERE m.attachment IS NOT NULL AND m.attachment <> ''
		    AND NOT EXISTS (
		      SELECT 1 FROM chat_message_attachments cma WHERE cma.message_id = m.id
		    )`,
	)
	if err != nil {
		return fmt.Errorf("scan legacy attachments: %w", err)
	}
	type legacyRow struct {
		MessageID int64
		SenderID  string
		JSON      string
		CreatedAt time.Time
	}
	var legacy []legacyRow
	for rows.Next() {
		var r legacyRow
		if err := rows.Scan(&r.MessageID, &r.SenderID, &r.JSON, &r.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	migrated := 0
	for _, r := range legacy {
		var att ChatMessageAttachment
		if err := jsonDecodeChatAttachment(r.JSON, &att); err != nil {
			continue
		}
		ws, err := s.personalWorkspaceForUser(r.SenderID)
		if err != nil {
			return fmt.Errorf("personal workspace for %s: %w", r.SenderID, err)
		}
		row := &Attachment{
			WorkspaceID:    ws,
			UploaderUserID: r.SenderID,
			Filename:       att.FileName,
			MimeType:       att.MimeType,
			SizeBytes:      att.Size,
			StorageKind:    AttachmentStorageLocalFS,
			StorageRef:     att.URL,
			Width:          att.Width,
			Height:         att.Height,
			ThumbnailURL:   att.ThumbnailURL,
			CreatedAt:      r.CreatedAt,
		}
		if strings.HasPrefix(att.URL, "http://") || strings.HasPrefix(att.URL, "https://") {
			row.StorageKind = AttachmentStorageRemote
		}
		if err := s.insertAttachment(row); err != nil {
			return fmt.Errorf("insert attachment for msg %d: %w", r.MessageID, err)
		}
		if err := s.linkAttachmentToChatMessage(r.MessageID, row.ID); err != nil {
			return fmt.Errorf("link attachment for msg %d: %w", r.MessageID, err)
		}
		migrated++
	}
	if migrated > 0 {
		fmt.Printf("[migrate] chat_messages.attachment → attachments: %d row(s)\n", migrated)
	}
	if _, err := s.db.Exec(`ALTER TABLE chat_messages DROP COLUMN IF EXISTS attachment`); err != nil {
		return fmt.Errorf("drop chat_messages.attachment: %w", err)
	}
	return nil
}

func jsonDecodeChatAttachment(raw string, out *ChatMessageAttachment) error {
	return json.Unmarshal([]byte(raw), out)
}
