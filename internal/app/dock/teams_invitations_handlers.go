package dock

// HTTP handlers for team invitations. Three audiences:
//   - team admins:  create / list / revoke (gated by RequireTeamRole admin+)
//   - invitees:     public preview by token, accept (auth, email match), decline
//   - logged-in users: inbox of invitations addressed to my email
//
// Email: dispatched via the existing s.mailer infrastructure. If
// disabled, invitation row is still created and the link is returned
// in-band so the inviter can hand-deliver it.

import (
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	teamsInvitationMaxBatchPerCall = 50
)

// publicInvitationView is what we expose to anyone holding the token,
// including unauthenticated visitors. Kept narrow on purpose: don't
// leak the inviter's email or the team's whole metadata.
type publicInvitationView struct {
	Status      string `json:"status"`
	Email       string `json:"email"`         // recipient
	Role        string `json:"role"`
	TeamID      string `json:"team_id"`
	TeamName    string `json:"team_name"`
	TeamSlug    string `json:"team_slug"`
	TeamAvatar  string `json:"team_avatar_url"`
	InviterName string `json:"inviter_name"`
	ExpiresAt   string `json:"expires_at"`
}

// adminInvitationView is what admins see in the invitations tab. More
// fields than the public view but no token (admins can re-fetch the
// canonical invite link via the create response — listing existing
// invites is a status-only operation).
type adminInvitationView struct {
	ID            string  `json:"id"`
	TeamID        string  `json:"team_id"`
	Email         string  `json:"email"`
	Role          string  `json:"role"`
	InviterUserID string  `json:"inviter_user_id"`
	Status        string  `json:"status"`
	ExpiresAt     string  `json:"expires_at"`
	CreatedAt     string  `json:"created_at"`
	InviteURL     string  `json:"invite_url"` // re-derived for admin's "copy link" UI
	InviterName   string  `json:"inviter_name"`
	InviterEmail  string  `json:"inviter_email"`
	AcceptedAt    *string `json:"accepted_at,omitempty"`
}

// inboxInvitationView is what the recipient sees in their own inbox.
// Contains the token because the recipient needs it to accept.
type inboxInvitationView struct {
	ID          string `json:"id"`
	Token       string `json:"token"`
	TeamID      string `json:"team_id"`
	TeamName    string `json:"team_name"`
	TeamSlug    string `json:"team_slug"`
	TeamAvatar  string `json:"team_avatar_url"`
	Role        string `json:"role"`
	InviterName string `json:"inviter_name"`
	ExpiresAt   string `json:"expires_at"`
	CreatedAt   string `json:"created_at"`
}

func looksLikeEmail(s string) bool {
	addr, err := mail.ParseAddress(strings.TrimSpace(s))
	return err == nil && addr.Address != ""
}

func roleIsAssignable(role string) bool {
	switch role {
	case TeamRoleAdmin, TeamRoleMember, TeamRoleViewer:
		return true
	default:
		// 'owner' is reserved for transfer flow (T7), not invitable.
		return false
	}
}

// ---- admin: create / list / revoke -------------------------------

type teamInvitationCreateRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

func (s *Server) handleTeamInvitationCreate(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	userID, ok := requireUserID(c)
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "个人工作区不能邀请他人"})
		return
	}
	var req teamInvitationCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	email := strings.TrimSpace(req.Email)
	role := strings.TrimSpace(req.Role)
	if !looksLikeEmail(email) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "邮箱格式不正确"})
		return
	}
	if !roleIsAssignable(role) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "角色不可分配（owner 不能直接邀请）"})
		return
	}

	// Block re-inviting an existing member.
	if existing, err := s.getUserByEmail(email); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	} else if existing != nil {
		if member, err := s.getTeamMember(teamID, existing.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
			return
		} else if member != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "该用户已是工作区成员"})
			return
		}
	}

	inv, err := s.createOrRefreshInvitation(teamID, email, role, userID, teamInvitationDefaultTTL)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}

	inviteURL := s.buildTeamInvitationLink(inv.Token)
	inviter, err := s.getUserByID(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	inviterName := ""
	inviterEmail := ""
	if inviter != nil {
		inviterName = firstNonEmpty(inviter.Username, inviter.Email)
		inviterEmail = inviter.Email
	}
	emailSent, sendErr := s.sendTeamInvitation(c.Request.Context(), requestLang(c), team.Name, inviterName, inviterEmail, inv, inviteURL)
	if sendErr != nil {
		log.Printf("[teams invite] send email failed: %v (team=%s email=%s)", sendErr, teamID, email)
	}

	c.JSON(http.StatusOK, gin.H{
		"invitation": adminInvitationView{
			ID:            inv.ID,
			TeamID:        inv.TeamID,
			Email:         inv.Email,
			Role:          inv.Role,
			InviterUserID: inv.InviterUserID,
			Status:        inv.Status,
			ExpiresAt:     inv.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
			CreatedAt:     inv.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			InviteURL:     inviteURL,
			InviterName:   inviterName,
			InviterEmail:  inviterEmail,
		},
		"email_sent":          emailSent,
		"email_send_disabled": s.mailer == nil || !s.mailer.Enabled(),
	})
}

func (s *Server) handleTeamInvitationList(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	invs, err := s.listPendingInvitationsForTeam(teamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	// Hydrate inviter info per row. Inviters are typically a tiny set
	// (1-3 admins) but we don't bother caching across rows.
	out := make([]adminInvitationView, 0, len(invs))
	for _, inv := range invs {
		inviterName := ""
		inviterEmail := ""
		if user, err := s.getUserByID(inv.InviterUserID); err == nil && user != nil {
			inviterName = firstNonEmpty(user.Username, user.Email)
			inviterEmail = user.Email
		}
		out = append(out, adminInvitationView{
			ID:            inv.ID,
			TeamID:        inv.TeamID,
			Email:         inv.Email,
			Role:          inv.Role,
			InviterUserID: inv.InviterUserID,
			Status:        inv.Status,
			ExpiresAt:     inv.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
			CreatedAt:     inv.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			InviteURL:     s.buildTeamInvitationLink(inv.Token),
			InviterName:   inviterName,
			InviterEmail:  inviterEmail,
		})
	}
	c.JSON(http.StatusOK, gin.H{"invitations": out})
}

func (s *Server) handleTeamInvitationRevoke(c *gin.Context) {
	teamID, _, ok := requireTeamRoleFromCtx(c)
	if !ok {
		return
	}
	invID := strings.TrimSpace(c.Param("inv_id"))
	if invID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	// Light cross-check: ensure the invitation belongs to this team.
	// Revoke is harmless if it doesn't but we'd rather refuse than
	// silently no-op.
	rows, err := s.db.Query(
		`SELECT id FROM team_invitations WHERE id = $1 AND team_id = $2`,
		invID, teamID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	found := rows.Next()
	rows.Close()
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "邀请不存在"})
		return
	}
	if err := s.revokeInvitation(invID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- public: preview / accept / decline --------------------------

// invitationStatusForView normalizes the status field with an
// "expired"-on-the-fly check. We don't trust the column alone since
// there's no scheduled job; rows can sit pending past their TTL.
func invitationStatusForView(inv *TeamInvitation) string {
	if inv.Status == TeamInvitationStatusPending && inv.ExpiresAt.Before(time.Now().UTC()) {
		return TeamInvitationStatusExpired
	}
	return inv.Status
}

func (s *Server) handleInvitationPublicPreview(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	inv, err := s.getInvitationByToken(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if inv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邀请不存在或已失效"})
		return
	}
	team, err := s.getTeam(inv.TeamID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if team == nil {
		c.JSON(http.StatusGone, gin.H{"error": "对应的工作区已不存在"})
		return
	}
	inviterName := ""
	if inviter, err := s.getUserByID(inv.InviterUserID); err == nil && inviter != nil {
		inviterName = firstNonEmpty(inviter.Username, inviter.Email)
	}
	c.JSON(http.StatusOK, gin.H{"invitation": publicInvitationView{
		Status:      invitationStatusForView(inv),
		Email:       inv.Email,
		Role:        inv.Role,
		TeamID:      team.ID,
		TeamName:    team.Name,
		TeamSlug:    team.Slug,
		TeamAvatar:  team.AvatarURL,
		InviterName: inviterName,
		ExpiresAt:   inv.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	}})
}

func (s *Server) handleInvitationAccept(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	user, err := s.getUserByID(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	inv, err := s.getInvitationByToken(token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if inv == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邀请不存在或已失效"})
		return
	}
	// Strict email match — case-insensitive but otherwise exact.
	if !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(inv.Email)) {
		c.JSON(http.StatusForbidden, gin.H{"error": "你的登录邮箱与受邀邮箱不一致"})
		return
	}
	accepted, err := s.acceptInvitation(token, userID)
	if err != nil {
		// User-friendly mapping for known states.
		msg := err.Error()
		switch msg {
		case "invitation has expired":
			c.JSON(http.StatusGone, gin.H{"error": "邀请已过期"})
			return
		case "invitation is not pending":
			c.JSON(http.StatusConflict, gin.H{"error": "邀请已处理或已撤销"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"team_id": accepted.TeamID,
		"role":    accepted.Role,
	})
}

func (s *Server) handleInvitationDecline(c *gin.Context) {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的输入数据"})
		return
	}
	if _, err := s.declineInvitation(token); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ---- inbox -------------------------------------------------------

func (s *Server) handleInvitationInbox(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	user, err := s.getUserByID(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	if user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	invs, err := s.listMyPendingInvitations(user.Email)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "服务器错误"})
		return
	}
	out := make([]inboxInvitationView, 0, len(invs))
	for _, inv := range invs {
		team, err := s.getTeam(inv.TeamID)
		if err != nil || team == nil {
			continue
		}
		inviterName := ""
		if inviter, err := s.getUserByID(inv.InviterUserID); err == nil && inviter != nil {
			inviterName = firstNonEmpty(inviter.Username, inviter.Email)
		}
		out = append(out, inboxInvitationView{
			ID:          inv.ID,
			Token:       inv.Token,
			TeamID:      team.ID,
			TeamName:    team.Name,
			TeamSlug:    team.Slug,
			TeamAvatar:  team.AvatarURL,
			Role:        inv.Role,
			InviterName: inviterName,
			ExpiresAt:   inv.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
			CreatedAt:   inv.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	c.JSON(http.StatusOK, gin.H{"invitations": out})
}

