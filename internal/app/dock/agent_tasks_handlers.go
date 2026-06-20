package dock

// agent_tasks_handlers.go — REST for the general on-device task queue (T0).
//
//   POST /api/tasks                     submit (session / api-token)
//   GET  /api/tasks/:id                 fetch
//   GET  /api/tasks                     list (newest first)
//   POST /api/agent/tasks/claim         agent pulls next (agent token)
//   POST /api/agent/tasks/:id/result    agent reports result (agent token)
//
// Workspace rule (v0): both submit and claim resolve the actor's PERSONAL
// workspace, so "your tasks → your agents". See polar-task doc/02.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type submitTaskRequest struct {
	Skill       string          `json:"skill"`
	Input       json.RawMessage `json:"input"`
	Constraints json.RawMessage `json:"constraints"`
	Priority    int             `json:"priority"`
}

func (s *Server) handleComputeTaskSubmit(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req submitTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}
	req.Skill = strings.TrimSpace(req.Skill)
	if req.Skill == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skill required"})
		return
	}
	workspaceID, err := s.personalWorkspaceForUser(userID)
	if err != nil || workspaceID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "personal workspace lookup failed"})
		return
	}
	task, err := s.enqueueTask(workspaceID, userID, req.Skill, req.Input, req.Constraints, req.Priority)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, task)
}

func (s *Server) handleComputeTaskGet(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	workspaceID, _ := s.personalWorkspaceForUser(userID)
	task, err := s.getTask(workspaceID, strings.TrimSpace(c.Param("id")))
	if errors.Is(err, sql.ErrNoRows) {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found"})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, task)
}

func (s *Server) handleComputeTaskList(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	workspaceID, _ := s.personalWorkspaceForUser(userID)
	tasks, err := s.listTasks(workspaceID, 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"tasks": tasks})
}

type claimTaskRequest struct {
	Skills []string `json:"skills"`
}

func (s *Server) handleComputeTaskClaim(c *gin.Context) {
	userIDVal, _ := c.Get("agent_user_id")
	userID, _ := userIDVal.(string)
	if userID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent context missing"})
		return
	}
	var req claimTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Skills) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "skills[] required"})
		return
	}
	workspaceID, err := s.personalWorkspaceForUser(userID)
	if err != nil || workspaceID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "personal workspace lookup failed"})
		return
	}
	// agent_id is informational here (the claim is workspace-scoped); pass the
	// token id so finalize can verify the same agent reports back.
	tokenIDVal, _ := c.Get("agent_token_id")
	agentID, _ := tokenIDVal.(string)
	task, err := s.claimTask(workspaceID, agentID, req.Skills)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if task == nil {
		c.Status(http.StatusNoContent)
		return
	}
	c.JSON(http.StatusOK, task)
}

type taskResultRequest struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output"`
	Error  string          `json:"error"`
}

func (s *Server) handleComputeTaskResult(c *gin.Context) {
	userIDVal, _ := c.Get("agent_user_id")
	userID, _ := userIDVal.(string)
	tokenIDVal, _ := c.Get("agent_token_id")
	agentID, _ := tokenIDVal.(string)
	if userID == "" || agentID == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent context missing"})
		return
	}
	var req taskResultRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid input"})
		return
	}
	workspaceID, _ := s.personalWorkspaceForUser(userID)
	ok, err := s.finalizeTask(workspaceID, strings.TrimSpace(c.Param("id")), agentID, req.OK, req.Output, req.Error)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "task not found, not running, or not yours"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
