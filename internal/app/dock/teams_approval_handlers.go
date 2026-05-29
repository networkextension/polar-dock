package dock

// teams_approval_handlers.go — R2-1: site-admin team-creation approval.
// Explicitly-created collaboration teams land status='pending'
// (handleTeamCreate); a site admin approves → 'active' or rejects →
// 'rejected'. All audited as provisioning. Routes are
// AuthMiddleware + AdminMiddleware gated in app.go.
//
// (R2-2 will hook default AI team-bot provisioning onto approve.)

import (
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// handleAdminTeamPendingList — GET /api/admin/teams/pending.
func (s *Server) handleAdminTeamPendingList(c *gin.Context) {
	teams, err := s.listTeamsByStatus(TeamStatusPending)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "teams.pending_list_failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"teams": teams})
}

// handleAdminTeamApprove — POST /api/admin/teams/:id/approve.
func (s *Server) handleAdminTeamApprove(c *gin.Context) {
	s.reviewTeam(c, TeamStatusActive, "team.approve")
}

// handleAdminTeamReject — POST /api/admin/teams/:id/reject.
func (s *Server) handleAdminTeamReject(c *gin.Context) {
	s.reviewTeam(c, TeamStatusRejected, "team.reject")
}

// reviewTeam transitions a pending team to active/rejected, idempotently
// guarded (only acts on a currently-pending team) and audited.
func (s *Server) reviewTeam(c *gin.Context, newStatus, action string) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "team id required"})
		return
	}
	team, err := s.getTeam(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "teams.get_failed"})
		return
	}
	if team == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "team not found"})
		return
	}
	if team.Status != TeamStatusPending {
		c.JSON(http.StatusConflict, gin.H{"error": "team is not pending", "status": team.Status})
		return
	}
	if _, err := s.setTeamStatus(id, newStatus); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "teams.set_status_failed"})
		return
	}
	s.LogAdminAction(c, AdminAuditIn{
		Category:     AuditCatProvisioning,
		Action:       action,
		TargetTeamID: id,
		TargetKind:   "team",
		TargetID:     id,
		Payload: map[string]any{
			"before": map[string]any{"status": TeamStatusPending},
			"after":  map[string]any{"status": newStatus},
			"name":   team.Name,
		},
	})
	// R2-2: on approval, provision the team's default AI team bot
	// (minimum config). Idempotent; non-fatal — a provisioning hiccup
	// must not fail the approval, and the startup backfill retries.
	if newStatus == TeamStatusActive {
		if _, err := s.provisionTeamBot(id); err != nil {
			log.Printf("teams: team-bot provisioning for approved team %s failed: %v", id, err)
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "status": newStatus})
}
