package dock

// agent_tasks_internal_handlers.go — module-facing compute-task origination
// (polar-task P2). Plugin services (film/lawyer) submit tasks over the HMAC
// /internal/v1 surface; tasks default to `hold` until released. A module can
// only see/act on its own tasks (scoped to the calling plugin_name). See doc/07.
//
//   POST /internal/v1/compute-tasks            submit (default hold)
//   GET  /internal/v1/compute-tasks            list own tasks (?status=&requester_ref=&limit=)
//   GET  /internal/v1/compute-tasks/:id        get own task + artifacts
//   POST /internal/v1/compute-tasks/:id/release  hold → queued

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func pluginNameFromCtx(c *gin.Context) string {
	v, _ := c.Get("plugin_name")
	name, _ := v.(string)
	return strings.TrimSpace(name)
}

type internalSubmitTaskRequest struct {
	WorkspaceID  string          `json:"workspace_id"`
	Skill        string          `json:"skill"`
	Input        json.RawMessage `json:"input"`
	Constraints  json.RawMessage `json:"constraints"`
	Priority     int             `json:"priority"`
	CallbackPath string          `json:"callback_path"`
	RequesterRef string          `json:"requester_ref"`
	ActingUserID string          `json:"acting_user_id"`
	AutoStart    bool            `json:"auto_start"`
}

func (s *Server) handleInternalComputeTaskSubmit(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	var req internalSubmitTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}
	req.Skill = strings.TrimSpace(req.Skill)
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	if req.Skill == "" || req.WorkspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill and workspace_id required"})
		return
	}
	status := "hold"
	if req.AutoStart {
		status = "queued"
	}
	createdBy := strings.TrimSpace(req.ActingUserID)
	if createdBy == "" {
		createdBy = "module:" + module
	}
	task, err := s.enqueueModuleTask(req.WorkspaceID, createdBy, req.Skill, req.Input, req.Constraints,
		req.Priority, module, strings.TrimSpace(req.RequesterRef), strings.TrimSpace(req.CallbackPath), status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, task)
}

func (s *Server) handleInternalComputeTaskList(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	tasks, err := s.listTasksByRequester(module, strings.TrimSpace(c.Query("status")), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if ref := strings.TrimSpace(c.Query("requester_ref")); ref != "" {
		filtered := tasks[:0]
		for _, t := range tasks {
			if t.RequesterRef == ref {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

func (s *Server) handleInternalComputeTaskGet(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	task, err := s.getTaskByRequester(module, strings.TrimSpace(c.Param("id")))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	arts, _ := s.listTaskArtifacts(task.ID)
	for i := range arts {
		arts[i].DownloadURL = s.assetDownloadURL(arts[i].AssetID, task.WorkspaceID)
	}
	c.JSON(http.StatusOK, gin.H{"task": task, "artifacts": arts})
}

func (s *Server) handleInternalComputeTaskRelease(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	ok, err := s.releaseTask(module, strings.TrimSpace(c.Param("id")))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found, not yours, or not held"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// lifecycleResult writes the standard ok/404 response for a transition.
func lifecycleResult(c *gin.Context, ok bool, err error) {
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found, not yours, or not in a valid state"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleInternalComputeTaskPause(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	ok, err := s.pauseTask(module, strings.TrimSpace(c.Param("id")))
	lifecycleResult(c, ok, err)
}

func (s *Server) handleInternalComputeTaskResume(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	ok, err := s.resumeTask(module, strings.TrimSpace(c.Param("id")))
	lifecycleResult(c, ok, err)
}

func (s *Server) handleInternalComputeTaskCancel(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	ok, err := s.cancelTask(module, strings.TrimSpace(c.Param("id")))
	lifecycleResult(c, ok, err)
}

func (s *Server) handleInternalComputeTaskPriority(c *gin.Context) {
	module := pluginNameFromCtx(c)
	if module == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin context missing"})
		return
	}
	var req struct {
		Priority int `json:"priority"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority required"})
		return
	}
	ok, err := s.setTaskPriority(module, strings.TrimSpace(c.Param("id")), req.Priority)
	lifecycleResult(c, ok, err)
}
