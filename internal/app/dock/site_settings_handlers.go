package dock

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// applePushCertSHA256Prefix reads the saved cert file from disk and
// returns the first 16 hex chars of its sha256. Used by the audit log
// so future investigations can match a leaked cert to an upload event
// without ever storing the cert bytes themselves. Returns "" on any
// IO error — audit is best-effort.
func applePushCertSHA256Prefix(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	full := hex.EncodeToString(h.Sum(nil))
	if len(full) > 16 {
		return full[:16]
	}
	return full
}

func normalizeApplePushEnvironment(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "dev":
		return "dev", nil
	case "prod":
		return "prod", nil
	default:
		return "", errors.New("invalid apple push certificate environment")
	}
}

func isAllowedApplePushCertificate(filename string) bool {
	switch strings.ToLower(filepath.Ext(strings.TrimSpace(filename))) {
	case ".p8", ".p12", ".pem", ".cer", ".crt", ".key":
		return true
	default:
		return false
	}
}

func (s *Server) applePushCertificateDir() string {
	return filepath.Join(s.uploadDir, "apple_push")
}

func applePushCertificateFromSettings(settings *SiteSettings, environment string) *ApplePushCertificate {
	if settings == nil {
		return nil
	}
	if environment == "dev" {
		return settings.ApplePushDevCert
	}
	return settings.ApplePushProdCert
}

func (s *Server) handleSiteSettingsGet(c *gin.Context) {
	settings, err := s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"site": s.hydrateSiteSettings(settings),
	})
}

func (s *Server) handleSiteSettingsUpdate(c *gin.Context) {
	var req struct {
		Name                       string `json:"name" binding:"required"`
		Description                string `json:"description"`
		RegistrationRequiresInvite bool   `json:"registration_requires_invite"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "站点名称不能为空"})
		return
	}

	description := strings.TrimSpace(req.Description)
	if err := s.updateSiteSettings(name, description, req.RegistrationRequiresInvite); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}

	settings, err := s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "site_settings.update",
		TargetKind: "site_setting",
		TargetID:   "site",
		Payload: map[string]any{
			"after": map[string]any{
				"name":                         name,
				"description":                  description,
				"registration_requires_invite": req.RegistrationRequiresInvite,
			},
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "保存成功",
		"site":    s.hydrateSiteSettings(settings),
	})
}

func (s *Server) handleInviteCodeGenerate(c *gin.Context) {
	var req struct {
		Count int `json:"count"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Count = 1
	}
	if req.Count <= 0 {
		req.Count = 1
	}
	if req.Count > 50 {
		req.Count = 50
	}

	adminID, _ := c.Get("user_id")
	adminIDStr, _ := adminID.(string)
	codes, err := s.createInviteCodes(adminIDStr, req.Count, time.Now())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "邀请码生成失败"})
		return
	}

	// Audit: log per-code prefix/suffix ONLY — full invite codes are
	// secrets (single-use, redeemable by any anonymous client) so they
	// must never land in the audit log.
	prefixes := make([]string, 0, len(codes))
	for _, ic := range codes {
		code := ic.Code
		var head, tail string
		switch {
		case len(code) <= 4:
			head = code
		case len(code) <= 8:
			head = code[:2]
			tail = code[len(code)-2:]
		default:
			head = code[:4]
			tail = code[len(code)-2:]
		}
		prefixes = append(prefixes, head+"…"+tail)
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "invite_code.generate",
		TargetKind: "site_setting",
		TargetID:   "invite_codes",
		Payload: map[string]any{
			"count":             len(codes),
			"code_hint_prefixes": prefixes,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message": "邀请码已生成",
		"codes":   codes,
	})
}

func (s *Server) handleInviteCodeList(c *gin.Context) {
	limit := 30
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 limit 参数"})
			return
		}
		if parsed > 200 {
			parsed = 200
		}
		limit = parsed
	}

	codes, err := s.listInviteCodes(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "邀请码加载失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"codes": codes})
}

func (s *Server) handleSiteIconUpload(c *gin.Context) {
	if s.uploadDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传目录未配置"})
		return
	}

	file, err := c.FormFile("icon")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择图片文件"})
		return
	}

	if file.Size > maxIconSizeBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "图片过大，建议不超过 2MB"})
		return
	}

	contentType := file.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持图片格式"})
		return
	}

	if err := os.MkdirAll(s.uploadDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	filename := "site_icon_" + buildUploadFilename(file.Filename)
	dstPath := filepath.Join(s.uploadDir, filename)
	if err := c.SaveUploadedFile(file, dstPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}

	// Single-write to the central assets catalog + a permanent public
	// share link. The staged file is transient (kept only to hash +
	// register); icon_url becomes an ABSOLUTE share url that resolves from
	// any subdomain (the relative /uploads/ form 404'd cross-origin).
	iconURL, assetID, mErr := s.migrateSiteIconFileToAsset(dstPath, contentType, c.GetString("user_id"))
	_ = os.Remove(dstPath)
	if mErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "资产保存失败"})
		return
	}

	var oldIcon string
	if settings, err := s.getSiteSettings(); err == nil && settings != nil {
		oldIcon = settings.IconURL
	}
	if oldIcon != "" && strings.HasPrefix(oldIcon, "/uploads/") {
		_ = os.Remove(filepath.Join(s.uploadDir, filepath.Base(oldIcon)))
	}

	if err := s.updateSiteIcon(iconURL); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}
	_ = s.setSiteIconAsset(assetID)

	settings, err := s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "site_settings.icon_upload",
		TargetKind: "site_setting",
		TargetID:   "site_icon",
		Payload: map[string]any{
			"after": map[string]any{
				"filename":  file.Filename,
				"size":      file.Size,
				"icon_url":  iconURL,
			},
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message":  "更新成功",
		"icon_url": iconURL,
		"site":     s.hydrateSiteSettings(settings),
	})
}

func (s *Server) handleApplePushCertificateUpload(c *gin.Context) {
	if s.uploadDir == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "上传目录未配置"})
		return
	}

	environment, err := normalizeApplePushEnvironment(c.Query("env"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "环境参数无效，仅支持 dev 或 prod"})
		return
	}

	file, err := c.FormFile("certificate")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择 APNs Key / .p8 文件"})
		return
	}

	if !isAllowedApplePushCertificate(file.Filename) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "仅支持 .p8、.p12、.pem、.cer、.crt、.key 文件"})
		return
	}

	certDir := s.applePushCertificateDir()
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	filename := "apple_push_" + environment + "_" + buildUploadFilename(file.Filename)
	dstPath := filepath.Join(certDir, filename)
	if err := c.SaveUploadedFile(file, dstPath); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}

	fileURL := "/uploads/apple_push/" + filename
	now := time.Now()
	settings, _ := s.getSiteSettings()
	oldCert := applePushCertificateFromSettings(settings, environment)
	if err := s.updateApplePushCertificate(environment, fileURL, file.Filename, now); err != nil {
		_ = os.Remove(dstPath)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "更新失败"})
		return
	}

	if oldCert != nil && oldCert.FileURL != "" && strings.HasPrefix(oldCert.FileURL, "/uploads/apple_push/") {
		oldPath := filepath.Join(certDir, filepath.Base(oldCert.FileURL))
		if oldPath != dstPath {
			_ = os.Remove(oldPath)
		}
	}

	settings, err = s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	// Audit: compute a fingerprint of the saved cert bytes (NOT the file
	// contents themselves — never log raw cert material). Best-effort; if
	// the file can't be re-read we still log filename + env so the trail
	// shows an upload happened.
	fingerprint := applePushCertSHA256Prefix(dstPath)
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "site_settings.apns_cert_upload",
		TargetKind: "site_setting",
		TargetID:   "apns_cert_" + environment,
		Payload: map[string]any{
			"env":                  environment,
			"filename":             file.Filename,
			"size":                 file.Size,
			"sha256_prefix":        fingerprint,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message":     "APNs Key / .p8 已上传",
		"environment": environment,
		"site":        s.hydrateSiteSettings(settings),
	})
}

func (s *Server) handleApplePushCertificateDelete(c *gin.Context) {
	environment, err := normalizeApplePushEnvironment(c.Query("env"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "环境参数无效，仅支持 dev 或 prod"})
		return
	}

	settings, err := s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	cert := applePushCertificateFromSettings(settings, environment)
	if cert == nil || cert.FileURL == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "当前环境下没有已上传的证书"})
		return
	}

	if err := s.clearApplePushCertificate(environment); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}

	if s.uploadDir != "" && strings.HasPrefix(cert.FileURL, "/uploads/apple_push/") {
		_ = os.Remove(filepath.Join(s.applePushCertificateDir(), filepath.Base(cert.FileURL)))
	}

	settings, err = s.getSiteSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatConfig,
		Action:     "site_settings.apns_cert_delete",
		TargetKind: "site_setting",
		TargetID:   "apns_cert_" + environment,
		Payload: map[string]any{
			"env": environment,
		},
	})

	c.JSON(http.StatusOK, gin.H{
		"message":     "APNs Key / .p8 已删除",
		"environment": environment,
		"site":        s.hydrateSiteSettings(settings),
	})
}
