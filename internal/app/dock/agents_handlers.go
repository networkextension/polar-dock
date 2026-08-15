package dock

// Agent admin kick — the only host/agent surface left in dock.
//
//   POST /api/admin/agents/:id/kick (site-admin) — close the agent's
//                                                  current WS conn
//
// The host/agent READ endpoints (GET /api/agents, /api/agents/:id,
// /api/hosts/:id/agents) moved to the polar-hosts module, which owns the
// data (nginx routes /api/{hosts,agents,...} → hosts-svc). Dock keeps kick
// only because the in-memory agent_hub registry is here; it moves to
// polar-hosts in Phase 4 (the /ws/agent connection migration), after which
// dock reads zero host/agent tables.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleAdminAgentKick(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 agent id"})
		return
	}
	// Resolve identity first so audit captures workspace + bot.
	var (
		botUserID   sql.NullString
		workspaceID string
		agentName   string
	)
	err := s.db.QueryRow(
		`SELECT workspace_id, name, bot_user_id FROM agents WHERE id = $1`,
		id,
	).Scan(&workspaceID, &agentName, &botUserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent 不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	// Try agent_id first (v4 primary key in the hub), then fall back to
	// bot_user_id for legacy attaches that pre-date the agents table.
	// Local hub first, then polar-hosts (Phase 4b: agents may attach there).
	kicked := s.routeAgentKick(id, botUserID.String)

	s.LogAdminAction(c, AdminAuditIn{
		Category:     AuditCatProvisioning,
		Action:       "agent_kick",
		TargetTeamID: workspaceID,
		TargetKind:   "agent",
		TargetID:     id,
		Payload: map[string]any{
			"agent_name":  agentName,
			"bot_user_id": botUserID.String,
			"was_online":  kicked,
		},
		Note: "site-admin kicked agent's WS conn",
	})

	c.JSON(http.StatusOK, gin.H{
		"agent_id":   id,
		"was_online": kicked,
	})
}

// safeCloseAgentConn closes the agent's close channel exactly once;
// concurrent kicks (and the normal disconnect path) both call
// close(conn.close) and a double-close would panic. The recover
// flattens that into a no-op so kick is idempotent.
func safeCloseAgentConn(conn *agentConn) {
	defer func() { _ = recover() }()
	close(conn.close)
}
