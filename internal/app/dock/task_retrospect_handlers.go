package dock

// HTTP handlers for project task retrospect (system_agent capability C).
//
// GET  /api/projects/:id/tasks/:task_id/retrospect          — read cached
// POST /api/projects/:id/tasks/:task_id/retrospect          — sync force regenerate
//
// Auto-trigger on status→done is wired in handleProjectTaskUpdate
// (projects_handlers.go) which calls s.fireRetrospectAsync().

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleProjectTaskRetrospectGet(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	projectID := strings.TrimSpace(c.Param("id"))
	if _, err := s.getProject(projectID, workspaceID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	taskID := strings.TrimSpace(c.Param("task_id"))
	r, err := s.getProjectTaskRetrospect(taskID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Returning nil retrospect with 200 OK is intentional — UI shows
	// "尚无复盘 · 点击生成" rather than handling 404 specially.
	c.JSON(http.StatusOK, gin.H{"retrospect": r})
}

func (s *Server) handleProjectTaskRetrospectGenerate(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	userID, _ := c.Get("user_id")
	userIDStr, _ := userID.(string)
	projectID := strings.TrimSpace(c.Param("id"))
	taskID := strings.TrimSpace(c.Param("task_id"))

	r, err := s.generateTaskRetrospect(c.Request.Context(), projectID, taskID, workspaceID, userIDStr)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"retrospect": r})
}
