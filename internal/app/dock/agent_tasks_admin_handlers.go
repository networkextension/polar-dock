package dock

// agent_tasks_admin_handlers.go — site-admin lifecycle control over ANY compute
// task (polar-task P2b). Unlike the module-scoped /internal/v1 surface, these are
// keyed by task id only (module=""), gated by AuthMiddleware+AdminMiddleware, and
// audit-logged. They also back the future admin monitor UI (P5) buttons.
//
//   POST /api/compute-tasks/:id/release | pause | resume | cancel | priority

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) adminTaskLifecycle(c *gin.Context, action string, fn func(id string) (bool, error)) {
	id := strings.TrimSpace(c.Param("id"))
	ok, err := fn(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found or not in a valid state"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:   AuditCatProvisioning,
		Action:     "compute_task." + action,
		TargetKind: "compute_task",
		TargetID:   id,
	})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleAdminComputeTaskRelease(c *gin.Context) {
	s.adminTaskLifecycle(c, "release", func(id string) (bool, error) { return s.releaseTask("", id) })
}
func (s *Server) handleAdminComputeTaskPause(c *gin.Context) {
	s.adminTaskLifecycle(c, "pause", func(id string) (bool, error) { return s.pauseTask("", id) })
}
func (s *Server) handleAdminComputeTaskResume(c *gin.Context) {
	s.adminTaskLifecycle(c, "resume", func(id string) (bool, error) { return s.resumeTask("", id) })
}
func (s *Server) handleAdminComputeTaskCancel(c *gin.Context) {
	s.adminTaskLifecycle(c, "cancel", func(id string) (bool, error) { return s.cancelTask("", id) })
}
func (s *Server) handleAdminComputeTaskSetPriority(c *gin.Context) {
	var req struct {
		Priority int `json:"priority"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority required"})
		return
	}
	s.adminTaskLifecycle(c, "priority", func(id string) (bool, error) { return s.setTaskPriority("", id, req.Priority) })
}
