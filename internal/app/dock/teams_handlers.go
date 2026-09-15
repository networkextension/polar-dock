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
		// R2: explicitly-created collaboration teams require site-admin
		// approval before they're active. (Home teams, created via
		// ensurePersonalTeam, default to active.)
		Status:      TeamStatusPending,
		OwnerUserID: userID,
	}
	if err := s.createTeam(team); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.logTeamAudit(team.ID, userID, "team.create", "team", team.ID,
		"创建工作区 "+team.Name+"（待审批）", map[string]any{"slug": team.Slug, "kind": team.Kind, "status": team.Status})
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
	if uid, ok := c.Get("user_id"); ok {
		s.logTeamAudit(teamID, uid.(string), "team.update", "team", teamID,
			"更新工作区元信息", map[string]any{"name": name})
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
	if isHomeTeam(team) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "主工作区不能删除"})
		return
	}
	// T7: pre-delete check across the T6-migrated tables. Refuse
	// rather than orphan apps / certs / profiles / ASC configs /
	// LLM configs that still belong to the workspace. Surface the
	// counts so the UI can prompt the user to clean up.
	counts, err := s.countTeamResources(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if counts.Total() > 0 {
		c.JSON(http.StatusConflict, gin.H{
			"error":           "工作区内仍有资源未清空，无法删除",
			"resource_counts": counts,
		})
		return
	}
	// team_members and team_invitations cascade via FK ON DELETE.
	if err := s.deleteTeam(teamID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if uid, ok := c.Get("user_id"); ok {
		// Workspace gone — record this site-wide so the audit
		// trail isn't lost when the workspace_id FK cascades.
		_ = s.appendLog(LogEntry{
			Category:    LogCategoryTeamAudit,
			Level:       LogLevelWarn,
			ActorUserID: uid.(string),
			ActorKind:   LogActorUser,
			Action:      "team.delete",
			SubjectKind: "team",
			SubjectID:   teamID,
			Message:     "删除工作区 " + team.Name,
		})
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- transfer ownership -------------------------------------------

type teamTransferRequest struct {
	NewOwnerUserID string `json:"new_owner_user_id"`
}

func (s *Server) handleTeamTransfer(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	var req teamTransferRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	newOwnerUserID := strings.TrimSpace(req.NewOwnerUserID)
	if newOwnerUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择新的所有者"})
		return
	}
	if err := s.transferTeamOwnership(teamID, newOwnerUserID); err != nil {
		switch err.Error() {
		case "personal team ownership cannot be transferred":
			c.JSON(http.StatusBadRequest, gin.H{"error": "个人工作区不能转让"})
			return
		case "new owner is not a member of this team":
			c.JSON(http.StatusBadRequest, gin.H{"error": "新所有者必须先成为工作区成员"})
			return
		case "team not found":
			c.JSON(http.StatusNotFound, gin.H{"error": "工作区不存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if uid, ok := c.Get("user_id"); ok {
		s.logTeamAudit(teamID, uid.(string), "team.transfer", "team", teamID,
			"转让工作区所有者", map[string]any{"new_owner_user_id": newOwnerUserID})
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- members: change role + kick / leave --------------------------

type teamMemberRoleUpdateRequest struct {
	Role string `json:"role"`
}

func (s *Server) handleTeamMemberRoleUpdate(c *gin.Context) {
	teamID, actorRole, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	if !teamRoleAllows(actorRole, TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "权限不足"})
		return
	}
	actorUserID, ok := requireUserID(c)
	if !ok {
		return
	}
	targetUserID := strings.TrimSpace(c.Param("user_id"))
	if targetUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if targetUserID == actorUserID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能修改自己的角色"})
		return
	}
	var req teamMemberRoleUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	role := strings.TrimSpace(req.Role)
	switch role {
	case TeamRoleAdmin, TeamRoleMember, TeamRoleViewer:
		// allowed
	default:
		// 'owner' is reserved for transfer flow.
		c.JSON(http.StatusBadRequest, gin.H{"error": "角色不可分配（owner 通过转让流程切换）"})
		return
	}
	target, err := s.getTeamMember(teamID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "成员不存在"})
		return
	}
	if target.Role == TeamRoleOwner {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能修改所有者角色，请先转让所有权"})
		return
	}
	// Admins can demote/promote members and viewers but not other
	// admins (only owner can touch admins). Tightens the bus-factor
	// failure mode where an admin could lock another admin out.
	if !teamRoleAllows(actorRole, TeamRoleOwner) && target.Role == TeamRoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "仅所有者可修改管理员角色"})
		return
	}
	if err := s.updateTeamMemberRole(teamID, targetUserID, role); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	s.logTeamAudit(teamID, actorUserID, "member.role_change", "user", targetUserID,
		"修改成员角色", map[string]any{"role": role, "previous_role": target.Role})
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleTeamMemberRemove(c *gin.Context) {
	teamID, actorRole, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	actorUserID, ok := requireUserID(c)
	if !ok {
		return
	}
	targetUserID := strings.TrimSpace(c.Param("user_id"))
	if targetUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
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
	if isHomeTeam(team) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "主工作区不能踢人 / 退出"})
		return
	}
	target, err := s.getTeamMember(teamID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "成员不存在"})
		return
	}
	// Owner can never be removed by API — must transfer first.
	if target.Role == TeamRoleOwner {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不能移除所有者，请先转让所有权"})
		return
	}
	isSelfLeave := targetUserID == actorUserID
	if !isSelfLeave {
		// Kicking someone else requires admin+.
		if !teamRoleAllows(actorRole, TeamRoleAdmin) {
			c.JSON(http.StatusForbidden, gin.H{"error": "权限不足"})
			return
		}
		// Admins can't kick other admins; only owner can.
		if !teamRoleAllows(actorRole, TeamRoleOwner) && target.Role == TeamRoleAdmin {
			c.JSON(http.StatusForbidden, gin.H{"error": "仅所有者可踢出管理员"})
			return
		}
	}
	if err := s.removeTeamMember(teamID, targetUserID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	action := "member.kick"
	message := "踢出成员"
	if isSelfLeave {
		action = "member.leave"
		message = "成员主动退出"
	}
	s.logTeamAudit(teamID, actorUserID, action, "user", targetUserID, message, map[string]any{"former_role": target.Role})
	c.JSON(http.StatusOK, gin.H{"ok": true, "self_left": isSelfLeave})
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

// POST /api/teams/:id/members — add an existing account to a team directly.
//
// Every other way in needs the invitee to act: an emailed invitation they
// must accept, or a registration code they must redeem. That leaves no path
// for "this account already exists and belongs in this team" — the shape you
// hit when a module grants someone a role but their team membership was
// never wired up, and the shape a backfill needs. Site admins were reduced
// to writing INSERTs by hand (infra/staging/mint-team.sh does exactly that).
//
// Role is capped at member/viewer by addTeamMember; promoting someone stays
// with handleTeamMemberRoleUpdate, which is where that decision is reviewed.
func (s *Server) handleTeamMemberAdd(c *gin.Context) {
	teamID, actorRole, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	if !teamRoleAllows(actorRole, TeamRoleAdmin) {
		c.JSON(http.StatusForbidden, gin.H{"error": "权限不足"})
		return
	}
	var req struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	targetUserID := strings.TrimSpace(req.UserID)
	if targetUserID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id 不能为空"})
		return
	}
	user, err := s.getUserByID(targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}
	// The home team is one person's own workspace; adding others to it would
	// quietly turn a private area into a shared one (same reason the
	// invitation endpoint refuses it).
	team, err := s.getTeam(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if team == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "团队不存在"})
		return
	}
	if isHomeTeam(team) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "主工作区不能添加成员"})
		return
	}
	if err := s.addTeamMember(teamID, targetUserID, req.Role); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	member, err := s.getTeamMember(teamID, targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "已加入团队", "member": member})
}
