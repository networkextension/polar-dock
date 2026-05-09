package dock

// HTTP handlers for the agent-driven build pipeline.
//
//   POST /api/iosdist/agent/builds/init           multipart: ipa + project_id
//   POST /api/iosdist/agent/builds/:id/start      returns creds + bundle id
//   POST /api/iosdist/agent/builds/:id/result     multipart: signed_ipa + meta
//
// All three require the polar-agent's bearer token (PolarAgentAuthMiddleware).
// Workspace scope is derived from the bound project — agent token alone
// doesn't carry a workspace, but the project id does.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const iosAgentBuildIPAMaxBytes = 500 << 20 // 500 MB

// ---- POST /api/iosdist/agent/builds/init ----

func (s *Server) handleIOSAgentBuildInit(c *gin.Context) {
	agentUserID, _ := c.Get("agent_user_id")
	uid, _ := agentUserID.(string)
	if uid == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent_user_id missing"})
		return
	}
	projectID := strings.TrimSpace(c.PostForm("project_id"))
	if projectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project_id is required"})
		return
	}
	// Find project — workspace check requires looking it up without
	// caller's X-Workspace-Id header (agent doesn't send one).
	project, err := s.getProjectByID(projectID)
	if err != nil || project == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found"})
		return
	}
	if project.IOSDistAppID == nil || *project.IOSDistAppID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "project has no iosdist_app binding; bind one in /projects.html first"})
		return
	}

	fh, err := c.FormFile("ipa")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing 'ipa' file part"})
		return
	}
	if fh.Size <= 0 || fh.Size > iosAgentBuildIPAMaxBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("ipa size %d invalid (max %d)", fh.Size, iosAgentBuildIPAMaxBytes)})
		return
	}

	build := &IOSAgentBuild{
		WorkspaceID:    project.WorkspaceID,
		ProjectID:      &project.ID,
		IOSDistAppID:   project.IOSDistAppID,
		AgentUserID:    uid,
		Status:         IOSAgentBuildStatusInit,
		SourceFilename: fh.Filename,
		SourceSize:     fh.Size,
	}
	taskID, err := s.createIOSAgentBuild(build)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create build: " + err.Error()})
		return
	}

	// Stage the upload to disk under uploadDir, then persist to chatStorage
	// (mirrors how iosdist_sign_worker uploads versions; reuse the
	// same staging pattern).
	stagedFilename := fmt.Sprintf("agent-build-%d-source-%s", taskID, sanitizeFilename(fh.Filename))
	stagedPath := filepath.Join(s.uploadDir, stagedFilename)
	if err := c.SaveUploadedFile(fh, stagedPath); err != nil {
		_ = s.markIOSAgentBuildFailed(taskID, "stage upload: "+err.Error(), "", "")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "stage upload failed"})
		return
	}
	url, err := s.chatStorage.Store(context.Background(), stagedPath, stagedFilename, "application/octet-stream")
	if err != nil {
		_ = s.markIOSAgentBuildFailed(taskID, "storage: "+err.Error(), "", "")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage failed"})
		return
	}
	if s.chatStorage.IsRemote() {
		_ = os.Remove(stagedPath)
	}
	if err := s.updateIOSAgentBuildStatus(taskID, IOSAgentBuildStatusUploaded, "", url); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update status: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"task_id":        taskID,
		"source_ipa_url": url,
	})
}

// ---- POST /api/iosdist/agent/builds/:id/start ----

func (s *Server) handleIOSAgentBuildStart(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	build, err := s.getIOSAgentBuild(taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load build"})
		return
	}
	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}
	if build.Status != IOSAgentBuildStatusUploaded {
		c.JSON(http.StatusConflict, gin.H{"error": "build is in state " + build.Status})
		return
	}
	if build.IOSDistAppID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "build is not bound to an iosdist app"})
		return
	}
	app, err := s.getIOSAppByID(*build.IOSDistAppID)
	if err != nil || app == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "iosdist app not found"})
		return
	}

	// Pick a distribution cert + app_store profile from the same workspace.
	// v1 picks the most-recently-updated. Multi-cert workspaces need an
	// explicit preference column — tracked as Phase 2 / FUTURE_WORK.
	cert, err := s.pickDefaultDistributionCert(app.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "pick cert: " + err.Error()})
		return
	}
	if cert == nil {
		c.JSON(http.StatusFailedDependency, gin.H{"error": "no distribution cert in workspace; upload one in /iosdist.html"})
		return
	}
	profile, err := s.pickDefaultAppStoreProfile(app.WorkspaceID, app.BundleID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "pick profile: " + err.Error()})
		return
	}
	if profile == nil {
		c.JSON(http.StatusFailedDependency, gin.H{"error": "no app_store profile matching bundle id; upload one in /iosdist.html"})
		return
	}
	asc, err := s.getIOSASCConfig(app.WorkspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load asc: " + err.Error()})
		return
	}
	if asc == nil {
		c.JSON(http.StatusFailedDependency, gin.H{"error": "no ASC config in workspace; upload .p8 in /iosdist.html"})
		return
	}

	// Decrypt cert password (one-shot — agent shouldn't persist it).
	password := cert.passwordCipher
	if cert.PasswordEncrypted {
		decrypted, derr := s.decryptIOSDistSecret(cert.passwordCipher)
		if derr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt cert pw: " + derr.Error()})
			return
		}
		password = decrypted
	}
	// Read the p8 bytes — already decrypted at load time via
	// loadASCConfigPrivateKey but we need the raw PEM for altool.
	_, p8PEM, err := s.loadASCConfigPrivateKey(asc)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load p8: " + err.Error()})
		return
	}

	if err := s.updateIOSAgentBuildStatus(taskID, IOSAgentBuildStatusSigning, "", ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update status"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"task_id":          taskID,
		"target_bundle_id": app.BundleID,
		"asc_app_id":       app.ASCAppID,
		"cert_url":         cert.FileURL,
		"cert_filename":    cert.FileFilename,
		"cert_password":    password,
		"cert_common_name": cert.CommonName,
		"profile_url":      profile.FileURL,
		"profile_filename": profile.FileFilename,
		"asc_issuer_id":    asc.IssuerID,
		"asc_key_id":       asc.KeyID,
		"asc_p8_pem":       string(p8PEM),
	})
}

// ---- POST /api/iosdist/agent/builds/:id/result ----

func (s *Server) handleIOSAgentBuildResult(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid task id"})
		return
	}
	build, err := s.getIOSAgentBuild(taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "load build"})
		return
	}
	if build == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "build not found"})
		return
	}

	okStr := strings.TrimSpace(c.PostForm("ok"))
	signMethod := strings.TrimSpace(c.PostForm("sign_method"))
	logTail := c.PostForm("log_tail")
	if okStr != "true" {
		errMsg := strings.TrimSpace(c.PostForm("error"))
		if errMsg == "" {
			errMsg = "unspecified failure"
		}
		_ = s.markIOSAgentBuildFailed(taskID, errMsg, signMethod, logTail)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	ascBuildID := strings.TrimSpace(c.PostForm("asc_build_id"))
	if ascBuildID == "" {
		// altool sometimes returns success without a build id (UUID
		// returned async). Allow empty but log it.
		ascBuildID = ""
	}

	signedURL := ""
	if fh, ferr := c.FormFile("signed_ipa"); ferr == nil && fh != nil && fh.Size > 0 {
		stagedFilename := fmt.Sprintf("agent-build-%d-signed-%s", taskID, sanitizeFilename(fh.Filename))
		stagedPath := filepath.Join(s.uploadDir, stagedFilename)
		if err := c.SaveUploadedFile(fh, stagedPath); err != nil {
			_ = s.markIOSAgentBuildFailed(taskID, "stage signed ipa: "+err.Error(), signMethod, logTail)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "stage signed ipa failed"})
			return
		}
		url, serr := s.chatStorage.Store(context.Background(), stagedPath, stagedFilename, "application/octet-stream")
		if serr != nil {
			_ = s.markIOSAgentBuildFailed(taskID, "storage signed ipa: "+serr.Error(), signMethod, logTail)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "storage failed"})
			return
		}
		if s.chatStorage.IsRemote() {
			_ = os.Remove(stagedPath)
		}
		signedURL = url
	}

	if err := s.markIOSAgentBuildSucceeded(taskID, signedURL, ascBuildID, signMethod, logTail); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "mark succeeded: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- helpers ----

// getProjectByID is a workspace-agnostic project lookup used by
// the agent middleware path (the agent's bearer token doesn't
// carry a workspace; we derive it from the project row).
func (s *Server) getProjectByID(projectID string) (*Project, error) {
	row := s.db.QueryRow(
		`SELECT `+projectSelectColumns+`
		 FROM projects WHERE id = $1`,
		projectID,
	)
	p, err := scanProject(row.Scan)
	if err != nil {
		return nil, err
	}
	return p, nil
}
