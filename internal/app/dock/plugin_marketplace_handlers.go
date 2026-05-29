package dock

// plugin_marketplace_handlers.go — R3-2: the module marketplace
// approval loop.
//
//   Admin (AuthMiddleware + AdminMiddleware):
//     PATCH /api/admin/plugin-modules/:name/publish
//     PATCH /api/admin/plugin-modules/:name/accepts-applications
//     GET   /api/admin/plugin-access-requests
//     POST  /api/admin/plugin-access-requests/:id/approve
//     POST  /api/admin/plugin-access-requests/:id/deny
//
//   Tenant (RequireTeamRole on :id):
//     POST  /api/teams/:id/plugin-requests   (team-admin)
//     GET   /api/teams/:id/plugin-requests   (member)
//
// Every admin mutation writes admin_audit_log (category provisioning).

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ── Admin: publish flags ─────────────────────────────────────────────

func (s *Server) handleAdminPluginModuleSetPublished(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	var req struct {
		Published bool `json:"published"`
	}
	if name == "" || c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name + published required"})
		return
	}
	ok, err := s.setPluginModulePublished(name, req.Published)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.publish_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.set_published",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload:    map[string]any{"after": map[string]any{"published": req.Published}},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "published": req.Published})
}

func (s *Server) handleAdminPluginModuleSetAcceptsApplications(c *gin.Context) {
	name := strings.TrimSpace(c.Param("name"))
	var req struct {
		Accepts bool `json:"accepts"`
	}
	if name == "" || c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name + accepts required"})
		return
	}
	ok, err := s.setPluginModuleAcceptsApplications(name, req.Accepts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_modules.accepts_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "plugin not found"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_module.set_accepts_applications",
		TargetKind: "plugin_module",
		TargetID:   name,
		Payload:    map[string]any{"after": map[string]any{"accepts_applications": req.Accepts}},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "accepts_applications": req.Accepts})
}

// ── Admin: request queue + review ────────────────────────────────────

func (s *Server) handleAdminPluginAccessRequestList(c *gin.Context) {
	reqs, err := s.listPendingPluginAccessRequests()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_requests.list_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"requests": reqs})
}

func (s *Server) handleAdminPluginAccessRequestApprove(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	pr, err := s.getPluginAccessRequest(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_requests.get_failed"})
		return
	}
	if pr == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "request not found"})
		return
	}
	reviewer := c.GetString("user_id")
	// Transition pending → approved. Returns false if it was already
	// reviewed (or vanished) — 409 so the admin re-reads the queue.
	ok, err := s.setPluginAccessRequestStatus(id, pluginAccessStatusApproved, reviewer)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_requests.update_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "request is not pending"})
		return
	}
	// Write the access grant. workspace_plugin_access is module-level;
	// api-scope requests grant the module too (the api_route is recorded
	// on the request for audit + future per-route enforcement).
	if _, err := s.upsertWorkspacePluginAccess(pr.WorkspaceID, pr.PluginName, true, reviewer); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "workspace_plugin_access.grant_failed"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:     AuditCatProvisioning,
		Action:       "plugin_access_request.approve",
		TargetTeamID: pr.WorkspaceID,
		TargetKind:   "plugin_access_request",
		TargetID:     strconv.FormatInt(id, 10),
		Payload: map[string]any{"after": map[string]any{
			"plugin_name": pr.PluginName, "scope": pr.Scope, "api_route": pr.APIRoute,
			"granted": true,
		}},
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": pluginAccessStatusApproved})
}

func (s *Server) handleAdminPluginAccessRequestDeny(c *gin.Context) {
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return
	}
	reviewer := c.GetString("user_id")
	ok, err := s.setPluginAccessRequestStatus(id, pluginAccessStatusDenied, reviewer)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_requests.update_failed"})
		return
	}
	if !ok {
		c.JSON(http.StatusConflict, gin.H{"error": "request not found or not pending"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "plugin_access_request.deny",
		TargetKind: "plugin_access_request",
		TargetID:   strconv.FormatInt(id, 10),
	})
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": pluginAccessStatusDenied})
}

// ── Tenant: request + list ───────────────────────────────────────────

func (s *Server) handleTeamPluginRequestCreate(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c) // route gates team-admin
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req struct {
		PluginName string `json:"plugin_name" binding:"required"`
		Scope      string `json:"scope"`
		APIRoute   string `json:"api_route"`
		Reason     string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin_name required"})
		return
	}
	// Only a published module that accepts applications can be requested.
	published, acceptsApps, found, err := s.pluginModulePublishState(req.PluginName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin lookup failed"})
		return
	}
	if !found || !published {
		c.JSON(http.StatusNotFound, gin.H{"error": "module not found in marketplace"})
		return
	}
	if !acceptsApps {
		c.JSON(http.StatusForbidden, gin.H{"error": "this module does not accept applications"})
		return
	}
	pr, err := s.createPluginAccessRequest(teamID, req.PluginName, req.Scope, req.APIRoute, userID, req.Reason)
	if err != nil {
		low := strings.ToLower(err.Error())
		if strings.Contains(low, "unique") || strings.Contains(low, "duplicate") {
			c.JSON(http.StatusConflict, gin.H{"error": "an open request for this module already exists"})
			return
		}
		// scope-validation errors are caller mistakes → 400.
		if strings.Contains(low, "scope") || strings.Contains(low, "api_route") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_request.create_failed"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"request": pr})
}

func (s *Server) handleTeamPluginRequestList(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	reqs, err := s.listPluginAccessRequestsForWorkspace(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin_access_requests.list_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"requests": reqs})
}
