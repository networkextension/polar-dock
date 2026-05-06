package dock

// HTTP handlers for the teams (collaboration workspaces) module.
// Routes are wired in app.go registerRoutes(). T5 will pull the role
// checks into a RequireTeamRole middleware; for now they live inline
// because this is the first module touching the new model.

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	teamsMaxNameLen        = 120
	teamsMaxDescriptionLen = 500
	teamsMaxAvatarURLLen   = 500
)

// teamRoleAllows reports whether actorRole is at least minRole. Order:
// owner > admin > member > viewer. Unknown roles are treated as below
// viewer.
func teamRoleAllows(actorRole, minRole string) bool {
	rank := func(r string) int {
		switch r {
		case TeamRoleOwner:
			return 4
		case TeamRoleAdmin:
			return 3
		case TeamRoleMember:
			return 2
		case TeamRoleViewer:
			return 1
		default:
			return 0
		}
	}
	return rank(actorRole) >= rank(minRole)
}

// requireWorkspaceID returns the active workspace ID from gin
// context (set by AuthMiddleware → resolveActiveWorkspace). If the
// middleware hasn't run or workspace lookup soft-failed, returns
// ("", false) and writes a 500 to c. Modules that have migrated to
// team_id ownership should call this instead of falling back to
// user_id.
func requireWorkspaceID(c *gin.Context) (string, bool) {
	v, _ := c.Get("workspace_id")
	id, ok := v.(string)
	if !ok || id == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	return id, true
}

// workspaceRole returns the caller's role in the active workspace, or
// the empty string if not set. Useful for handlers that need to gate
// by role on the active workspace specifically (the common case for
// migrated modules).
func workspaceRole(c *gin.Context) string {
	v, _ := c.Get("workspace_role")
	role, _ := v.(string)
	return role
}

// requireTeamMember resolves the caller's role on team teamID and
// enforces minRole. Writes the matching JSON error to c on failure.
// On success returns (role, true).
//
// Most team-scoped routes now use the RequireTeamRole middleware
// (see teams_middleware.go) which resolves and stores the role in
// ctx before the handler runs. This helper is kept for the rare
// case where the team id isn't on the path (e.g., resolved from a
// resource id at handler time).
func (s *Server) requireTeamMember(c *gin.Context, teamID, userID, minRole string) (string, bool) {
	member, err := s.getTeamMember(teamID, userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return "", false
	}
	if member == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在或无权访问"})
		return "", false
	}
	if !teamRoleAllows(member.Role, minRole) {
		c.JSON(http.StatusForbidden, gin.H{"error": "权限不足"})
		return "", false
	}
	return member.Role, true
}

// ---- list / detail -------------------------------------------------

func (s *Server) handleTeamList(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	teams, roles, err := s.listTeamsForUser(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Decorate each team with the caller's role so the UI can show
	// the action affordances (edit / delete) without a second call.
	type teamWithRole struct {
		Team
		MyRole string `json:"my_role"`
	}
	out := make([]teamWithRole, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamWithRole{Team: t, MyRole: roles[t.ID]})
	}
	c.JSON(http.StatusOK, gin.H{"teams": out})
}

func (s *Server) handleTeamDetail(c *gin.Context) {
	teamID, role, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	team, err := s.getTeam(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if team == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"team": team, "my_role": role})
}

// ---- create --------------------------------------------------------

type teamCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AvatarURL   string `json:"avatar_url"`
}

func (s *Server) handleTeamCreate(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req teamCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	description := strings.TrimSpace(req.Description)
	avatarURL := strings.TrimSpace(req.AvatarURL)
	if name == "" || len([]rune(name)) > teamsMaxNameLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "工作区名称不能为空且不超过 120 字"})
		return
	}
	if len([]rune(description)) > teamsMaxDescriptionLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "描述不超过 500 字"})
		return
	}
	if len(avatarURL) > teamsMaxAvatarURLLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "头像 URL 过长"})
		return
	}

	slug, err := s.generateUniqueTeamSlug()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	team := &Team{
		Slug:        slug,
		Name:        name,
		Description: description,
		AvatarURL:   avatarURL,
		Kind:        TeamKindTeam,
		OwnerUserID: userID,
	}
	if err := s.createTeam(team); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"team": team, "my_role": TeamRoleOwner})
}

// ---- update --------------------------------------------------------

type teamUpdateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AvatarURL   string `json:"avatar_url"`
}

func (s *Server) handleTeamUpdate(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	var req teamUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	name := strings.TrimSpace(req.Name)
	description := strings.TrimSpace(req.Description)
	avatarURL := strings.TrimSpace(req.AvatarURL)
	if name == "" || len([]rune(name)) > teamsMaxNameLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "工作区名称不能为空且不超过 120 字"})
		return
	}
	if len([]rune(description)) > teamsMaxDescriptionLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "描述不超过 500 字"})
		return
	}
	if len(avatarURL) > teamsMaxAvatarURLLen {
		c.JSON(http.StatusBadRequest, gin.H{"error": "头像 URL 过长"})
		return
	}
	if err := s.updateTeamMetadata(teamID, name, description, avatarURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	team, err := s.getTeam(teamID)
	if err != nil || team == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"team": team})
}

// ---- delete --------------------------------------------------------

func (s *Server) handleTeamDelete(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	team, err := s.getTeam(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if team == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在"})
		return
	}
	if team.Kind == TeamKindPersonal {
		c.JSON(http.StatusBadRequest, gin.H{"error": "个人工作区不能删除"})
		return
	}
	// T6 will add cross-module resource checks here. For now the
	// schema's ON DELETE CASCADE handles team_members and
	// team_invitations cleanup; module tables don't yet carry
	// team_id so there's nothing else to vet.
	if err := s.deleteTeam(teamID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- members -------------------------------------------------------

func (s *Server) handleTeamMembers(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	members, err := s.listTeamMembersWithUser(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}
