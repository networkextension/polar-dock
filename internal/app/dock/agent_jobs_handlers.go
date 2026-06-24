package dock

// agent_jobs_handlers.go — the generic durable fleet-job queue (P3 of
// doc/arch/agent-proxy-and-task-dispatch.md, generalized beyond research runs).
//
//   producer → POST /internal/v1/agent-tasks/enqueue   (plugin HMAC; e.g. polar-film)
//   dock     → {"kind":"task.wake"} doorbell to capable online agents
//   agent    → POST /api/agent/tasks/claim             (agent token; FOR UPDATE SKIP LOCKED)
//            → run the job, then
//   agent    → POST /api/agent/tasks/:id/complete|/fail (agent token)
//
// Capability routing is the claim WHERE clause: a job's required_arch (NULL = any)
// is matched against the claiming agent's host arch. filmscan_extract jobs are
// required_arch NULL so any online agent (x86 or arm64) drains them — they never
// stall; filmscan_analyze jobs are required_arch 'arm64' so only Apple-silicon
// (Neural Engine) hosts run them. On filmscan_extract completion dock chains a
// filmscan_analyze job from the extract result.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	jobKindFilmscanExtract = "filmscan_extract"
	jobKindFilmscanAnalyze = "filmscan_analyze"
)

func validAgentJobKind(k string) bool {
	switch k {
	case jobKindFilmscanExtract, jobKindFilmscanAnalyze:
		return true
	}
	return false
}

// resolveAgentCapability maps an agent token id to its v4 identity: the agent id,
// its workspace, and its host arch (from the agents row, falling back to the
// host_arch the token recorded at hello). workspaceID == "" means the token has no
// registered agent — it can't claim workspace-scoped jobs.
func (s *Server) resolveAgentCapability(tokenID string) (agentID, workspaceID, arch string, err error) {
	err = s.db.QueryRow(`
		SELECT COALESCE(a.id,''), COALESCE(a.workspace_id,''),
		       COALESCE(NULLIF(a.arch,''), t.host_arch, '')
		  FROM agent_tokens t
		  LEFT JOIN agents a ON a.agent_token_id = t.id
		 WHERE t.id = $1`, tokenID).Scan(&agentID, &workspaceID, &arch)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	return agentID, workspaceID, arch, err
}

// enqueueAgentJob inserts a queued job and returns its id. requiredArch == ""
// stores NULL (any agent). payload must be valid JSON (defaults to {}).
func (s *Server) enqueueAgentJob(workspaceID, kind, requiredArch, assignedAgentID string, payload []byte) (int64, error) {
	if len(payload) == 0 {
		payload = []byte("{}")
	}
	var reqArch, assigned any
	if requiredArch != "" {
		reqArch = requiredArch
	}
	if assignedAgentID != "" {
		assigned = assignedAgentID
	}
	var id int64
	err := s.db.QueryRow(`
		INSERT INTO agent_jobs (workspace_id, kind, required_arch, assigned_agent_id, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb)
		RETURNING id`, workspaceID, kind, reqArch, assigned, string(payload)).Scan(&id)
	return id, err
}

// ── POST /internal/v1/agent-tasks/enqueue (producer; plugin HMAC) ───

type enqueueAgentJobReq struct {
	WorkspaceID     string          `json:"workspace_id"`
	Kind            string          `json:"kind"`
	RequiredArch    string          `json:"required_arch"`
	AssignedAgentID string          `json:"assigned_agent_id"`
	Payload         json.RawMessage `json:"payload"`
}

func (s *Server) handleAgentJobEnqueue(c *gin.Context) {
	var req enqueueAgentJobReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad json: " + err.Error()})
		return
	}
	req.WorkspaceID = strings.TrimSpace(req.WorkspaceID)
	req.Kind = strings.TrimSpace(req.Kind)
	req.RequiredArch = strings.TrimSpace(req.RequiredArch)
	if req.WorkspaceID == "" || !validAgentJobKind(req.Kind) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace_id and a valid kind required"})
		return
	}
	id, err := s.enqueueAgentJob(req.WorkspaceID, req.Kind, req.RequiredArch, req.AssignedAgentID, req.Payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "enqueue failed: " + err.Error()})
		return
	}
	// Doorbell: nudge capable online agents to claim now (best-effort).
	s.wakeCapableAgents(req.WorkspaceID, req.RequiredArch)
	c.JSON(http.StatusOK, gin.H{"job_id": id})
}

// ── POST /api/agent/tasks/claim (agent token) ───────────────────────

func (s *Server) handleAgentTaskClaim(c *gin.Context) {
	tokenID := c.GetString("agent_token_id")
	agentID, workspaceID, arch, err := s.resolveAgentCapability(tokenID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "capability lookup failed"})
		return
	}
	if workspaceID == "" {
		c.Status(http.StatusNoContent) // not a registered agent → nothing to claim
		return
	}

	var (
		id      int64
		kind    string
		payload []byte
	)
	// Oldest queued job in this workspace whose capability + assignment match.
	err = s.db.QueryRow(`
		UPDATE agent_jobs SET status='running', claimed_by_agent_id=$1,
		       claimed_at=now(), updated_at=now()
		 WHERE id = (
		   SELECT id FROM agent_jobs
		    WHERE status='queued' AND workspace_id=$2
		      AND (required_arch IS NULL OR required_arch=$3)
		      AND (assigned_agent_id IS NULL OR assigned_agent_id=$1)
		    ORDER BY created_at
		    LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, kind, payload`, agentID, workspaceID, arch).Scan(&id, &kind, &payload)
	if err == sql.ErrNoRows {
		c.Status(http.StatusNoContent)
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "claim failed: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"kind":    kind,
		"job_id":  id,
		"payload": json.RawMessage(payload),
	})
}

// ── POST /api/agent/tasks/:id/complete | /fail (agent token) ────────

type completeAgentJobReq struct {
	Result json.RawMessage `json:"result"`
}

func (s *Server) handleAgentJobComplete(c *gin.Context) {
	tokenID := c.GetString("agent_token_id")
	agentID, workspaceID, _, err := s.resolveAgentCapability(tokenID)
	if err != nil || workspaceID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unknown agent"})
		return
	}
	jobID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad job id"})
		return
	}
	var req completeAgentJobReq
	_ = c.ShouldBindJSON(&req)
	result := req.Result
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}

	var kind string
	err = s.db.QueryRow(`
		UPDATE agent_jobs SET status='done', result=$1::jsonb,
		       completed_at=now(), updated_at=now()
		 WHERE id=$2 AND claimed_by_agent_id=$3 AND status='running'
		RETURNING kind`, string(result), jobID, agentID).Scan(&kind)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not running under this agent"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "complete failed: " + err.Error()})
		return
	}

	// Chain: a finished extract auto-enqueues the ANE analyze stage, carrying the
	// extract result (manifest + audio_track_id) so the analyze box — which never
	// saw the source video — can pull the audio and resolve the manifest.
	if kind == jobKindFilmscanExtract {
		if analyzeID, aerr := s.chainAnalyzeJob(workspaceID, result); aerr == nil && analyzeID > 0 {
			s.wakeCapableAgents(workspaceID, "arm64")
			c.JSON(http.StatusOK, gin.H{"ok": true, "chained_analyze_job_id": analyzeID})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// chainAnalyzeJob enqueues a filmscan_analyze job from a filmscan_extract result.
// The analyze payload IS the extract manifest (+ any media_id) so the analyze
// runner can `filmscan analyze --from-manifest`.
func (s *Server) chainAnalyzeJob(workspaceID string, extractResult json.RawMessage) (int64, error) {
	var r struct {
		Manifest json.RawMessage `json:"manifest"`
		MediaID  string          `json:"media_id"`
	}
	_ = json.Unmarshal(extractResult, &r)
	if len(r.Manifest) == 0 {
		return 0, nil // nothing analyzable
	}
	payload, _ := json.Marshal(map[string]any{
		"manifest":     r.Manifest,
		"media_id":     r.MediaID,
		"workspace_id": workspaceID,
	})
	return s.enqueueAgentJob(workspaceID, jobKindFilmscanAnalyze, "arm64", "", payload)
}

type failAgentJobReq struct {
	Error string `json:"error"`
}

func (s *Server) handleAgentJobFail(c *gin.Context) {
	tokenID := c.GetString("agent_token_id")
	agentID, workspaceID, _, err := s.resolveAgentCapability(tokenID)
	if err != nil || workspaceID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unknown agent"})
		return
	}
	jobID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad job id"})
		return
	}
	var req failAgentJobReq
	_ = c.ShouldBindJSON(&req)
	res, err := s.db.Exec(`
		UPDATE agent_jobs SET status='error', error_message=$1,
		       completed_at=now(), updated_at=now()
		 WHERE id=$2 AND claimed_by_agent_id=$3 AND status='running'`,
		strings.TrimSpace(req.Error), jobID, agentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "fail update failed"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not running under this agent"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// wakeCapableAgents rings a {"kind":"task.wake"} doorbell to every online agent in
// the workspace whose arch can run a job with requiredArch ("" = any). Best-effort.
func (s *Server) wakeCapableAgents(workspaceID, requiredArch string) {
	rows, err := s.db.Query(`
		SELECT DISTINCT bot_user_id FROM agents
		 WHERE workspace_id=$1 AND bot_user_id IS NOT NULL
		   AND ($2='' OR arch=$2)`, workspaceID, requiredArch)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var bot string
		if rows.Scan(&bot) == nil && bot != "" {
			s.agentHub.dispatchTaskWake(bot)
		}
	}
}
