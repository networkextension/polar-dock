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
	"strconv"
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
	arts, _ := s.listTaskArtifacts(task.ID)
	for i := range arts {
		arts[i].DownloadURL = s.assetDownloadURL(arts[i].AssetID, workspaceID)
	}
	c.JSON(http.StatusOK, gin.H{"task": task, "artifacts": arts})
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
	s.injectInputAssetURL(task, workspaceID)
	c.JSON(http.StatusOK, task)
}

// injectInputAssetURL: if a claimed task's input carries an asset_id, mint a
// short-lived signed download URL and add it as input.url so the skill can fetch
// the input straight from polar-assets (vision.* read input.url via ImageSource;
// speech.stt via localFile). The DB row keeps asset_id canonical — the url is
// ephemeral (per-claim, short TTL) and never persisted. See polar-task doc/07 P1.
func (s *Server) injectInputAssetURL(task *AgentTask, workspaceID string) {
	if task == nil || len(task.Input) == 0 {
		return
	}
	var m map[string]any
	if json.Unmarshal(task.Input, &m) != nil || m == nil {
		return
	}
	if _, has := m["url"]; has { // an explicit url wins; leave it
		return
	}
	aid := jsonInt64(m["asset_id"])
	if aid <= 0 {
		return
	}
	a, err := s.getAssetByID(aid) // ownership guard: never hand out another ws's asset
	if err != nil || a == nil {
		return
	}
	if a.WorkspaceID != nil && *a.WorkspaceID != workspaceID {
		return
	}
	if u := s.assetDownloadURL(aid, workspaceID); u != "" {
		m["url"] = u
		if b, err := json.Marshal(m); err == nil {
			task.Input = b
		}
	}
}

// jsonInt64 coerces a value decoded from JSON (float64 / json.Number / string)
// into an int64; 0 if not a positive integer.
func jsonInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case int64:
		return n
	case int:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	}
	return 0
}

type taskResultRequest struct {
	OK        bool            `json:"ok"`
	Output    json.RawMessage `json:"output"`
	Error     string          `json:"error"`
	Artifacts []struct {
		Kind    string `json:"kind"`
		AssetID int64  `json:"asset_id"`
		Mime    string `json:"mime"`
		Bytes   int64  `json:"bytes"`
	} `json:"artifacts"`
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
	taskID := strings.TrimSpace(c.Param("id"))
	for _, a := range req.Artifacts {
		if a.AssetID <= 0 {
			continue
		}
		if err := s.insertTaskArtifact(taskID, a.Kind, a.AssetID, a.Mime, a.Bytes); err != nil {
			// non-fatal: the task is done; artifact row is recoverable
			c.Header("X-Artifact-Insert", "failed")
		}
	}
	// P3: if a module originated this task with a callback, push the result back
	// (signed, async, best-effort). Read the finalized row for requester info.
	if t, terr := s.getTask(workspaceID, taskID); terr == nil && t != nil && t.RequesterModule != "" && t.CallbackPath != "" {
		arts, _ := s.listTaskArtifacts(taskID)
		go s.fireTaskCallback(t, arts)
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
