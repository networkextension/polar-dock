package dock

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Durable pull-based task dispatch (P3 of
// doc/arch/agent-proxy-and-task-dispatch.md). The push path
// (agentHub.dispatchResearchTask) is fire-and-forget and REQUIRES the
// agent online at dispatch time — an offline agent loses the task. The
// pull path instead enqueues the run (project_research_runs already is a
// queue) + sends a tiny task.wake doorbell; the agent claims the payload
// via POST /api/agent/tasks/claim and drains the backlog on reconnect.
// Durable, backpressured, survives offline agents.

// agentTaskPullEnabled gates the pull path. Default OFF (keep push).
// Flip POLAR_AGENT_TASK_PULL=1 only once the fleet's polar-agent
// understands task.wake + the claim loop.
func agentTaskPullEnabled() bool {
	return os.Getenv("POLAR_AGENT_TASK_PULL") == "1"
}

// storeResearchDispatchPayload persists the built research_task envelope
// (LLM api_key already stripped) so claim can return it after an offline
// gap without rebuilding it.
func (s *Server) storeResearchDispatchPayload(runID int64, payload []byte) error {
	_, err := s.db.Exec(
		`UPDATE project_research_runs SET dispatch_payload = $2 WHERE id = $1`,
		runID, string(payload),
	)
	return err
}

// botUserIDForAgentToken maps an authenticated agent token to the bot it
// registered as (agents.agent_token_id -> bot_user_id). Empty when the
// token isn't bound to a v4 agent row (e.g. a legacy attach token).
func (s *Server) botUserIDForAgentToken(agentTokenID string) (string, error) {
	var bot sql.NullString
	err := s.db.QueryRow(
		`SELECT bot_user_id FROM agents WHERE agent_token_id = $1 LIMIT 1`,
		strings.TrimSpace(agentTokenID),
	).Scan(&bot)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(bot.String), nil
}

// claimNextResearchRun atomically claims the oldest queued run for a bot
// (FOR UPDATE SKIP LOCKED so concurrent claims are race-safe), flipping
// it to running and returning its id + stored payload. Returns
// (0, "", nil) when the queue is empty.
func (s *Server) claimNextResearchRun(botUserID string, now time.Time) (int64, string, error) {
	var runID int64
	var payload sql.NullString
	err := s.db.QueryRow(
		`UPDATE project_research_runs
		    SET status = 'running',
		        claimed_at = $2,
		        started_at = COALESCE(started_at, $2),
		        updated_at = $2
		  WHERE id = (
		        SELECT id FROM project_research_runs
		         WHERE status = 'queued' AND bot_user_id = $1
		         ORDER BY created_at
		         LIMIT 1
		         FOR UPDATE SKIP LOCKED
		  )
		RETURNING id, dispatch_payload`,
		strings.TrimSpace(botUserID), now,
	).Scan(&runID, &payload)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", nil
		}
		return 0, "", err
	}
	return runID, payload.String, nil
}

// handleAgentTaskClaim is the agent's pull endpoint. Authenticated by
// the agent token, it claims the next queued research run for the
// agent's bot, re-injects the per-agent proxy token (kept out of the
// queue at rest), and returns the research_task envelope. 204 when the
// queue is empty — the agent's reconnect-drain loop stops on 204.
func (s *Server) handleAgentTaskClaim(c *gin.Context) {
	tokenIDAny, _ := c.Get("agent_token_id")
	agentTokenID, _ := tokenIDAny.(string)
	if strings.TrimSpace(agentTokenID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "agent token required"})
		return
	}
	botUserID, err := s.botUserIDForAgentToken(agentTokenID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "agent lookup failed"})
		return
	}
	if botUserID == "" {
		c.Status(http.StatusNoContent)
		return
	}

	now := time.Now()
	runID, payloadJSON, err := s.claimNextResearchRun(botUserID, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "claim failed"})
		return
	}
	if runID == 0 {
		c.Status(http.StatusNoContent) // queue empty
		return
	}

	var envelope agentResearchTask
	if strings.TrimSpace(payloadJSON) != "" {
		if uerr := json.Unmarshal([]byte(payloadJSON), &envelope); uerr != nil {
			log.Printf("agent task claim: run=%d bad stored payload: %v", runID, uerr)
		}
	}
	envelope.Kind = "research_task"
	envelope.ResearchRunID = runID
	// Re-inject the per-agent proxy token (kept out of dispatch_payload).
	if strings.TrimSpace(envelope.LLM.APIKey) == "" {
		if ag, perr := s.getAgentProxyByBotUserID(botUserID); perr == nil && ag != nil {
			if raw, derr := s.decryptStoredAgentProxyToken(ag.Enc, ag.Encrypted); derr == nil && strings.TrimSpace(raw) != "" {
				envelope.LLM.APIKey = raw
			}
		}
	}
	// The agent still calls /research/runs/:id/start (posts the
	// transparency message) and /result, exactly as in the push path —
	// claim only replaces the WS delivery of the envelope.
	c.JSON(http.StatusOK, envelope)
}
