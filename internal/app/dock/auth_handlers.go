package dock

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (s *Server) handleRegister(c *gin.Context) {
	var req struct {
		Username       string `json:"username" binding:"required,min=3"`
		Email          string `json:"email" binding:"required,email"`
		Password       string `json:"password" binding:"required,min=6"`
		InvitationCode string `json:"invitation_code"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "common.invalid_input")
		return
	}

	now := time.Now()
	settings, err := s.getSiteSettings()
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	inviteRequired := settings != nil && settings.RegistrationRequiresInvite
	inviteCode := normalizeInviteCode(req.InvitationCode)
	inviteMarker := ""
	// The code is validated whenever one is *supplied*, not only when
	// the site requires one: a code is how a module role (opera 主创 /
	// 管理员) is claimed, so an unrecognized code must be rejected
	// rather than silently downgraded to a plain registration. Mirrors
	// classifyInviteCode() in polar-opera's auth-identity.ts.
	var claimedInvite *InviteCode
	if inviteRequired || inviteCode != "" {
		if inviteCode == "" {
			jsonError(c, http.StatusForbidden, "auth.invite_required")
			return
		}
		inviteMarker = "pending:" + strings.ToLower(strings.TrimSpace(req.Email)) + ":" + generateResourceID()[:8]
		claimed, err := s.consumeInviteCode(inviteCode, inviteMarker, now)
		if err != nil {
			jsonError(c, http.StatusInternalServerError, "common.server_error")
			return
		}
		if claimed == nil {
			jsonError(c, http.StatusForbidden, "auth.invite_invalid")
			return
		}
		claimedInvite = claimed
	}

	existingUser, err := s.getUserByEmail(req.Email)
	if err != nil {
		if inviteMarker != "" {
			_ = s.releaseInviteCode(inviteCode, inviteMarker)
		}
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if existingUser != nil {
		if inviteMarker != "" {
			_ = s.releaseInviteCode(inviteCode, inviteMarker)
		}
		jsonError(c, http.StatusConflict, "auth.email_registered")
		return
	}

	hashedPassword, err := hashPassword(req.Password)
	if err != nil {
		if inviteMarker != "" {
			_ = s.releaseInviteCode(inviteCode, inviteMarker)
		}
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}

	deviceType, pushToken, deviceID := s.parseClientInfo(c.GetHeader("X-Device-Type"), c.GetHeader("X-Push-Token"), c.GetHeader("X-Device-Id"))

	user := &User{
		ID:         generateSessionID()[:16],
		Username:   req.Username,
		Email:      req.Email,
		Password:   hashedPassword,
		DeviceType: deviceType,
		CreatedAt:  now,
	}
	if err := s.createUser(user); err != nil {
		if inviteMarker != "" {
			_ = s.releaseInviteCode(inviteCode, inviteMarker)
		}
		if err == errEmailExists {
			jsonError(c, http.StatusConflict, "auth.email_registered")
			return
		}
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if inviteMarker != "" {
		if err := s.bindInviteCodeToUser(inviteCode, inviteMarker, user.ID); err != nil {
			log.Printf("bind invite code failed: code=%s user=%s err=%v", inviteCode, user.ID, err)
		}
	}
	// A role-bearing code lands its grant now, before the session is
	// issued, so the very first /me the portal makes already carries
	// the module role and can route to the right landing page.
	moduleRole := ""
	if claimedInvite != nil && claimedInvite.Module != "" && claimedInvite.Role != "" {
		if err := s.grantModuleRole(claimedInvite.Module, user.ID, claimedInvite.Role, "invite:"+claimedInvite.CreatedBy, inviteCode, now); err != nil {
			// Registration itself succeeded; failing the request here
			// would leave the user unable to retry (email taken, code
			// burnt). Log loudly — an admin can re-grant by hand.
			log.Printf("grant module role failed: module=%s role=%s user=%s err=%v",
				claimedInvite.Module, claimedInvite.Role, user.ID, err)
		} else {
			moduleRole = claimedInvite.Module + ":" + claimedInvite.Role
		}
	}

	// A module role says who someone is; it does not say which collaboration
	// workspace their data lives in — and plugins scope every read and write
	// by team. Granting one without a team is what produced accounts that
	// registered fine and then hit "you are not in any team" with no way
	// forward, so the two land together or the code is not doing its job.
	//
	// Which team: the code's own, else the site default when this deployment
	// is single-tenant. With multi-tenancy on, a code that names no team
	// deliberately grants none — there is no sensible default to pick.
	if claimedInvite != nil {
		targetTeam := strings.TrimSpace(claimedInvite.TeamID)
		if targetTeam == "" && settings != nil && !settings.MultiTenantEnabled {
			targetTeam = strings.TrimSpace(settings.DefaultTeamID)
		}
		if targetTeam != "" {
			// createUser does not create the home team (it is lazy, via
			// resolveActiveWorkspace). /internal/v1/auth/verify defaults
			// workspace_id to the home team, so without this the first
			// plugin call back gets an empty workspace.
			if _, err := s.ensurePersonalTeam(user.ID); err != nil {
				log.Printf("ensure personal team failed: user=%s err=%v", user.ID, err)
			}
			// Same failure stance as grantModuleRole above: registration has
			// already succeeded and the code is burnt, so never fail the
			// request here — log and let an admin add the member by hand.
			if err := s.addTeamMember(targetTeam, user.ID, claimedInvite.TeamRole); err != nil {
				log.Printf("join team failed: team=%s user=%s role=%s err=%v",
					targetTeam, user.ID, claimedInvite.TeamRole, err)
			}
		}
	}

	if err := s.upsertUserDeviceWithID(user.ID, deviceType, deviceID, pushToken, now); err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if err := s.syncUserPresence(user.ID, now); err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}

	accessToken, refreshToken, _, err := s.createTokenFamily(user, deviceType, deviceID, pushToken)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	s.setAuthCookies(c, accessToken, refreshToken)
	s.recordLoginEvent(c, user.ID, "register", deviceType)

	payload := gin.H{
		"user_id":  user.ID,
		"username": user.Username,
	}
	if moduleRole != "" {
		payload["module"] = claimedInvite.Module
		payload["module_role"] = claimedInvite.Role
	}
	jsonMessage(c, http.StatusCreated, "auth.register_success", payload)
}

func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "common.invalid_input")
		return
	}

	user, err := s.getUserByEmail(req.Email)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if user == nil || !checkPassword(req.Password, user.Password) {
		jsonError(c, http.StatusUnauthorized, "auth.invalid_credentials")
		return
	}

	deviceType, pushToken, deviceID := s.parseClientInfo(c.GetHeader("X-Device-Type"), c.GetHeader("X-Push-Token"), c.GetHeader("X-Device-Id"))
	now := time.Now()
	if err := s.upsertUserDeviceWithID(user.ID, deviceType, deviceID, pushToken, now); err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if err := s.syncUserPresence(user.ID, now); err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}

	accessToken, refreshToken, _, err := s.createTokenFamily(user, deviceType, deviceID, pushToken)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	s.setAuthCookies(c, accessToken, refreshToken)
	s.recordLoginEvent(c, user.ID, "password", deviceType)

	jsonMessage(c, http.StatusOK, "auth.login_success", gin.H{
		"user_id":           user.ID,
		"username":          user.Username,
		"access_expires_in": int(AccessTokenTTL / time.Second),
	})
}

func (s *Server) handleTokenRefresh(c *gin.Context) {
	presented := extractRefreshToken(c)
	if presented == "" {
		s.clearAuthCookies(c)
		jsonError(c, http.StatusUnauthorized, "auth.unauthorized")
		return
	}

	deviceType, pushToken, deviceID := s.parseClientInfo(c.GetHeader("X-Device-Type"), c.GetHeader("X-Push-Token"), c.GetHeader("X-Device-Id"))
	accessToken, newRefresh, _, err := s.rotateRefreshToken(presented, deviceType, deviceID, pushToken)
	if err != nil {
		// On replay we've already revoked the entire family — force
		// the client back to a full login instead of a retry loop.
		s.clearAuthCookies(c)
		jsonError(c, http.StatusUnauthorized, "auth.unauthorized")
		return
	}

	s.setAuthCookies(c, accessToken, newRefresh)
	jsonMessage(c, http.StatusOK, "auth.token_refreshed", gin.H{
		"access_expires_in": int(AccessTokenTTL / time.Second),
	})
}

func (s *Server) handleLogout(c *gin.Context) {
	if token := extractAccessToken(c); token != "" {
		if session := s.getAccessSession(token); session != nil {
			if session.FamilyID != "" {
				if err := s.revokeTokenFamily(session.FamilyID); err != nil {
					log.Printf("logout revoke family failed: %v", err)
				}
			}
		}
		if err := s.deleteAccessSession(token); err != nil {
			log.Printf("logout delete access session failed: %v", err)
		}
	}
	if refreshTok := extractRefreshToken(c); refreshTok != "" {
		if refresh := s.getRefreshToken(refreshTok); refresh != nil && refresh.FamilyID != "" {
			if err := s.revokeTokenFamily(refresh.FamilyID); err != nil {
				log.Printf("logout revoke family via refresh failed: %v", err)
			}
		}
	}

	s.clearAuthCookies(c)
	jsonMessage(c, http.StatusOK, "auth.logout_success", nil)
}

func (s *Server) handleMe(c *gin.Context) {
	userID, _ := c.Get("user_id")
	username, _ := c.Get("username")
	role, _ := c.Get("role")
	userIDStr, _ := userID.(string)
	var user *User
	if userIDStr != "" {
		user, _ = s.getUserByID(userIDStr)
	}

	// Permission-model P0: surface site-admin status so the UI can
	// gate root-team-only pages (admin-plugins, audit log, …) without
	// loading-then-403. Computed independently of users.role — see
	// permission-model.md §0 for why.
	isSiteAdmin := false
	if userIDStr != "" {
		if ok, err := s.IsSiteAdmin(userIDStr); err == nil {
			isSiteAdmin = ok
		}
	}

	// Per-module roles ({"opera":"researcher"}). An auth portal skinned
	// for one module (opera-dock-ui) reads this straight after login to
	// pick the landing page, instead of guessing from users.role.
	moduleRoles, err := s.moduleRolesForUser(userIDStr)
	if err != nil {
		log.Printf("me: module roles lookup failed user=%s err=%v", userIDStr, err)
		moduleRoles = map[string]string{}
	}

	c.JSON(http.StatusOK, gin.H{
		"user_id":      userID,
		"username":     username,
		"role":         role,
		"site_admin":   isSiteAdmin,
		"module_roles": moduleRoles,
		"email": func() string {
			if user != nil {
				return user.Email
			}
			return ""
		}(),
		"email_verified": func() bool {
			return user != nil && user.EmailVerified
		}(),
		"icon_url": func() string {
			if user != nil {
				return user.IconURL
			}
			return ""
		}(),
		"is_online": func() bool {
			return user != nil && user.IsOnline
		}(),
		"device_type": func() string {
			if user != nil && user.DeviceType != "" {
				return user.DeviceType
			}
			return deviceTypeBrowser
		}(),
		"last_seen_at": func() *time.Time {
			if user != nil {
				return user.LastSeenAt
			}
			return nil
		}(),
		"bio": func() string {
			if user != nil {
				return user.Bio
			}
			return ""
		}(),
	})
}

func (s *Server) handleLoginHistory(c *gin.Context) {
	userID, _ := c.Get("user_id")
	userIDStr, ok := userID.(string)
	if !ok || userIDStr == "" {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}

	limit := 5
	if limitStr := c.Query("limit"); limitStr != "" {
		parsed, err := strconv.Atoi(limitStr)
		if err != nil || parsed <= 0 {
			jsonError(c, http.StatusBadRequest, "common.invalid_input")
			return
		}
		limit = parsed
	}

	records, err := s.listLoginRecords(userIDStr, limit)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"records": records,
	})
}

// handleChangePassword — self-service password change for the logged-in user.
// POST /api/me/password {old_password, new_password}. Verifies the current
// password before setting the new one (admins change others' via
// PUT /api/admin/users/:id/password).
func (s *Server) handleChangePassword(c *gin.Context) {
	userID, ok := requireUserID(c)
	if !ok {
		return
	}
	var req struct {
		OldPassword string `json:"old_password" binding:"required"`
		NewPassword string `json:"new_password" binding:"required,min=6"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonError(c, http.StatusBadRequest, "common.invalid_input")
		return
	}
	if req.OldPassword == req.NewPassword {
		jsonError(c, http.StatusBadRequest, "common.invalid_input")
		return
	}
	user, err := s.getUserByID(userID)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if user == nil || !checkPassword(req.OldPassword, user.Password) {
		jsonError(c, http.StatusUnauthorized, "auth.invalid_credentials")
		return
	}
	hash, err := hashPassword(req.NewPassword)
	if err != nil {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	if ok, err := s.updateUserPasswordHash(userID, hash); err != nil || !ok {
		jsonError(c, http.StatusInternalServerError, "common.server_error")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "密码已更新"})
}
