package dock

// Agent Identity v4 — dock-side read endpoints + admin kick.
// See doc/arch/agent-identity-v4.md.
//
//   GET  /api/agents               (workspace-member) — list across the
//                                                       caller's workspace
//   GET  /api/agents/:id           (workspace-member) — single agent + host
//   GET  /api/hosts/:id/agents     (workspace-member) — agents on one host
//   POST /api/admin/agents/:id/kick (site-admin)      — close the agent's
//                                                       current WS conn
//
// Mirrors the polar-hosts plugin's read endpoints (which back the same
// UI for tenant-scoped views). Dock owns the kick because the
// in-memory agent_hub registry is here, not in the plugin.
//
// Wire-shape contract is held identical to polar-hosts' AgentListItem
// (agent_token_id_suffix = last 8 chars of agent_token_id, NEVER the
// raw bearer — see hosts_store.go AgentListItem doc).

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// AgentListItem is the dock wire shape for /api/agents listings. Kept
// in sync with polar-hosts' AgentListItem (the Phase F UI parses one
// or the other depending on which path the nginx layer routes —
// polar-hosts for tenant views, dock for cross-workspace admin views).
type AgentListItem struct {
	ID                 string  `json:"id"`
	WorkspaceID        string  `json:"workspace_id"`
	HostID             string  `json:"host_id"`
	HostName           string  `json:"host_name,omitempty"`
	HostHWModel        string  `json:"host_hw_model,omitempty"`
	Name               string  `json:"name"`
	BotUserID          string  `json:"bot_user_id,omitempty"`
	AgentTokenIDSuffix string  `json:"agent_token_id_suffix"`
	OS                 string  `json:"os,omitempty"`
	Arch               string  `json:"arch,omitempty"`
	CreatedAt          string  `json:"created_at"`
	LastHelloAt        *string `json:"last_hello_at,omitempty"`
}

// agentSelectColumns is the canonical SELECT shape. The
// agent_token_id_suffix uses RIGHT(a.agent_token_id, 8) so the raw
// bearer (which dock never persists in plaintext — only the
// agent_tokens.token_hash) is unreachable from this path even if a
// future Scan changed.
const agentSelectColumns = `
	SELECT a.id, a.workspace_id, a.host_id,
	       COALESCE(h.name, ''),
	       COALESCE(h.host_info_json->>'hw_model', ''),
	       a.name,
	       COALESCE(a.bot_user_id, ''),
	       RIGHT(a.agent_token_id, 8),
	       COALESCE(a.os, ''),
	       COALESCE(a.arch, ''),
	       a.created_at,
	       a.last_hello_at
	  FROM agents a
	  LEFT JOIN hosts h ON h.id = a.host_id`

func scanAgentListRow(row rowScanner) (*AgentListItem, error) {
	var it AgentListItem
	var lastHello sql.NullTime
	var createdAt sql.NullTime
	if err := row.Scan(
		&it.ID, &it.WorkspaceID, &it.HostID,
		&it.HostName, &it.HostHWModel,
		&it.Name, &it.BotUserID, &it.AgentTokenIDSuffix,
		&it.OS, &it.Arch,
		&createdAt, &lastHello,
	); err != nil {
		return nil, err
	}
	if createdAt.Valid {
		it.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05Z")
	}
	if lastHello.Valid {
		v := lastHello.Time.UTC().Format("2006-01-02T15:04:05Z")
		it.LastHelloAt = &v
	}
	return &it, nil
}

func (s *Server) scanAgentList(rows *sql.Rows) ([]AgentListItem, error) {
	defer rows.Close()
	out := []AgentListItem{}
	for rows.Next() {
		it, err := scanAgentListRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

func (s *Server) listAgentsForWorkspace(workspaceID string) ([]AgentListItem, error) {
	rows, err := s.db.Query(
		agentSelectColumns+` WHERE a.workspace_id = $1
		 ORDER BY a.last_hello_at DESC NULLS LAST, a.created_at DESC`,
		workspaceID,
	)
	if err != nil {
		return nil, err
	}
	return s.scanAgentList(rows)
}

func (s *Server) getAgentByID(agentID string) (*AgentListItem, error) {
	row := s.db.QueryRow(agentSelectColumns+` WHERE a.id = $1`, agentID)
	it, err := scanAgentListRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return it, err
}

func (s *Server) listAgentsByHost(hostID string) ([]AgentListItem, error) {
	rows, err := s.db.Query(
		agentSelectColumns+` WHERE a.host_id = $1
		 ORDER BY a.last_hello_at DESC NULLS LAST, a.created_at DESC`,
		hostID,
	)
	if err != nil {
		return nil, err
	}
	return s.scanAgentList(rows)
}

// hostBelongsToWorkspace is a fast existence probe used by the
// /api/hosts/:id/agents path to avoid leaking cross-tenant existence
// (we don't return the host row, just check that the caller has access).
func (s *Server) hostBelongsToWorkspace(hostID, workspaceID string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM hosts WHERE id = $1 AND workspace_id = $2)`,
		hostID, workspaceID,
	).Scan(&exists)
	return exists, err
}

// ---- HTTP handlers ---------------------------------------------------------

func (s *Server) handleAgentList(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	agents, err := s.listAgentsForWorkspace(workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

func (s *Server) handleAgentDetail(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 agent id"})
		return
	}
	agent, err := s.getAgentByID(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if agent == nil || agent.WorkspaceID != workspaceID {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent 不存在或不在当前工作区"})
		return
	}
	host, _ := s.getHostByID(agent.HostID)
	c.JSON(http.StatusOK, gin.H{
		"agent": agent,
		"host":  host,
	})
}

func (s *Server) handleAgentsByHost(c *gin.Context) {
	workspaceID, ok := requireWorkspaceID(c)
	if !ok {
		return
	}
	hostID := strings.TrimSpace(c.Param("id"))
	if hostID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的 host id"})
		return
	}
	ok, err := s.hostBelongsToWorkspace(hostID, workspaceID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "host 不存在或不在当前工作区"})
		return
	}
	agents, err := s.listAgentsByHost(hostID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"agents": agents})
}

// handleAdminAgentKick is the site-admin escape hatch for force-closing
// a misbehaving polar-agent's WS conn. We:
//   1. Resolve the agent's bot_user_id from the agents table.
//   2. Hand the agent_id to agent_hub.lookup; if a live conn is found,
//      close its `close` channel — the write/read loops both select on
//      this channel and unwind cleanly, which triggers the deferred
//      agentHub.unregister in the WS handler so the registry stays
//      consistent.
//   3. LogAdminAction (audit even on the no-op path so a "kick to be
//      sure" trail exists).
//
// The kick does NOT delete the agents row — the agent can reconnect
// with the same agent_id. To revoke for good, the admin uses
// DELETE /api/admin/agent-tokens/:id (separate endpoint, separate
// audit category).
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
	conn := s.agentHub.lookupByAgentID(id)
	if conn == nil && botUserID.Valid && botUserID.String != "" {
		conn = s.agentHub.lookup(botUserID.String)
	}
	kicked := false
	if conn != nil {
		// Close the close channel — both pumps select on it and unwind.
		// The defer in the WS handler calls agentHub.unregister so the
		// registry's byAgent + byBot maps both drop their entry; a
		// subsequent reconnect-as-new will register fresh without
		// double-registration.
		safeCloseAgentConn(conn)
		kicked = true
	}

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
		"agent_id":    id,
		"was_online":  kicked,
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
